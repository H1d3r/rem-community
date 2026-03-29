package simplex

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/chainreactors/logs"
	"github.com/chainreactors/rem/x/arq"
)

var (
	DefaultSimplexInternal    = 100                   // 最大发包间隔(毫秒)
	DefaultSimplexMinInternal = 10 * time.Millisecond // 最小发包间隔(毫秒)
)

const simplexQueueBackpressurePollInterval = time.Millisecond

const (
	DefaultItemsPerCycle = 4   // 每 tick 默认最大发送 item 数
	DefaultSendQueueCap  = 512 // 数据包发送队列默认容量
	DefaultCtrlQueueCap  = 64  // 控制包发送队列默认容量
)

// SimplexConfig 传输通道层配置，控制 Simplex 的发送行为和队列参数。
// 零值表示使用默认值。
//
// 用户只需设置 MaxPacketSize（希望单次发送的最大包大小），
// ItemsPerCycle 会自动从 ceil(MaxPacketSize / MaxBodySize) 推导。
type SimplexConfig struct {
	Interval      time.Duration // 轮询间隔 (从 SimplexAddr 获取)
	MaxBodySize   int           // 单个 item 的最大 raw 字节 (从传输层获取)
	MaxPacketSize int           // 用户期望的最大包大小 (0=MaxBodySize*4, 自动推导 ItemsPerCycle)
	ItemsPerCycle int           // 每 tick 最大发送 item 数 (通常由 MaxPacketSize 自动推导)
	SendQueueCap  int           // 数据包发送队列容量 (0=512)
	CtrlQueueCap  int           // 控制包发送队列容量 (0=64)
	RecvQueueCap  int           // 接收队列容量 (0=自动按 MaxBodySize 调整)
}

// DataBudget returns the total bytes that can be queued for one tick
// cycle of multi-item sending: MaxBodySize × ItemsPerCycle.
func (sc SimplexConfig) DataBudget() int {
	items := sc.ItemsPerCycle
	if items <= 0 {
		items = DefaultItemsPerCycle
	}
	return sc.MaxBodySize * items
}

// Normalize fills zero-valued fields with defaults.
// If MaxPacketSize is set, ItemsPerCycle is derived from it.
func (sc *SimplexConfig) Normalize() {
	if sc.MaxPacketSize > 0 && sc.MaxBodySize > 0 && sc.ItemsPerCycle <= 0 {
		sc.ItemsPerCycle = (sc.MaxPacketSize + sc.MaxBodySize - 1) / sc.MaxBodySize
		if sc.ItemsPerCycle < 1 {
			sc.ItemsPerCycle = 1
		}
	}
	if sc.ItemsPerCycle <= 0 {
		sc.ItemsPerCycle = DefaultItemsPerCycle
	}
	if sc.MaxPacketSize <= 0 && sc.MaxBodySize > 0 {
		sc.MaxPacketSize = sc.MaxBodySize * sc.ItemsPerCycle
	}
	if sc.SendQueueCap <= 0 {
		sc.SendQueueCap = DefaultSendQueueCap
	}
	if sc.CtrlQueueCap <= 0 {
		sc.CtrlQueueCap = DefaultCtrlQueueCap
	}
}

var defaultSimplexServerRecvChannelCapacity = 128

var simplexClientCreators = make(map[string]func(*SimplexAddr) (Simplex, error))

type pendingSendBatch struct {
	packets      *SimplexPackets
	firstFailure time.Time
}

func isExpectedTransportShutdownError(err error) bool {
	if err == nil {
		return false
	}
	if errors.Is(err, io.ErrClosedPipe) || errors.Is(err, net.ErrClosed) ||
		errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return true
	}

	msg := strings.ToLower(err.Error())
	return strings.Contains(msg, "connection refused") ||
		strings.Contains(msg, "actively refused") ||
		strings.Contains(msg, "use of closed network connection") ||
		strings.Contains(msg, "server closed")
}

func isTerminalSimplexTransportError(err error) bool {
	return errors.Is(err, io.ErrClosedPipe) ||
		errors.Is(err, net.ErrClosed) ||
		errors.Is(err, context.Canceled)
}

func simplexAcceptanceStallTimeout(addr *SimplexAddr) time.Duration {
	cfg := addr.ARQConfig()

	rto := time.Duration(cfg.RTO) * time.Millisecond
	if rto <= 0 {
		rto = time.Duration(arq.ARQ_RTO) * time.Millisecond
	}

	maxRetransmissions := cfg.MaxRetransmissions
	if maxRetransmissions <= 0 {
		maxRetransmissions = arq.ARQ_MAX_RETRANS
	}

	timeout := time.Duration(maxRetransmissions+1) * rto
	if timeout < 200*time.Millisecond {
		timeout = 200 * time.Millisecond
	}
	if interval := addr.Interval(); interval > timeout {
		timeout = interval
	}
	return timeout
}

func newSimplexStallError(timeout time.Duration, err error) error {
	if err == nil {
		return fmt.Errorf("simplex transport acceptance stalled for %v: %w", timeout, io.ErrClosedPipe)
	}
	return fmt.Errorf("simplex transport acceptance stalled for %v after repeated send failures: %v: %w", timeout, err, io.ErrClosedPipe)
}

func (p *pendingSendBatch) set(packets *SimplexPackets) {
	if packets == nil || packets.Size() == 0 {
		p.clear()
		return
	}
	p.packets = packets
	p.firstFailure = time.Time{}
}

func (p *pendingSendBatch) clear() {
	p.packets = nil
	p.firstFailure = time.Time{}
}

func (p *pendingSendBatch) ready() bool {
	return p != nil && p.packets != nil && p.packets.Size() > 0
}

func (p *pendingSendBatch) markFailure(now time.Time) {
	if p.firstFailure.IsZero() {
		p.firstFailure = now
	}
}

func (p *pendingSendBatch) stalled(now time.Time, timeout time.Duration) bool {
	return p.ready() && !p.firstFailure.IsZero() && now.Sub(p.firstFailure) >= timeout
}

var simplexServerCreators = make(map[string]func(string, string) (Simplex, error))
var simplexAddrResolvers = make(map[string]func(string, string) (*SimplexAddr, error))

func RegisterSimplex(name string, client func(*SimplexAddr) (Simplex, error), server func(string, string) (Simplex, error), addrResolver func(string, string) (*SimplexAddr, error)) error {
	if _, ok := simplexClientCreators[name]; ok {
		return fmt.Errorf("simplex client [%s] is already registered", name)
	} else {
		simplexClientCreators[name] = client
	}
	if _, ok := simplexServerCreators[name]; ok {
		return fmt.Errorf("simplex server [%s] is already registered", name)
	} else {
		simplexServerCreators[name] = server
	}
	if _, ok := simplexAddrResolvers[name]; ok {
		return fmt.Errorf("simplex addr resolver [%s] is already registered", name)
	} else {
		simplexAddrResolvers[name] = addrResolver
	}
	return nil
}

type Simplex interface {
	Receive() (pkts *SimplexPacket, addr *SimplexAddr, err error)
	Send(pkts *SimplexPackets, addr *SimplexAddr) (n int, err error)
	Addr() *SimplexAddr
	Close() error
}

// GetSimplexClient 获取注册的客户端创建器
func GetSimplexClient(name string) (func(*SimplexAddr) (Simplex, error), error) {
	creator, ok := simplexClientCreators[name]
	if ok {
		return creator, nil
	}
	return nil, fmt.Errorf("simplex client [%s] is not registered", name)
}

// GetSimplexServer 获取注册的服务端创建器
func GetSimplexServer(name string) (func(string, string) (Simplex, error), error) {
	creator, ok := simplexServerCreators[name]
	if ok {
		return creator, nil
	}
	return nil, fmt.Errorf("simplex server [%s] is not registered", name)
}

func NewSimplexClient(addr *SimplexAddr) (*SimplexClient, error) {
	creator, err := GetSimplexClient(addr.Scheme)
	if err != nil {
		return nil, err
	}
	sx, err := creator(addr)
	if err != nil {
		return nil, err
	}

	ctx, cancel := context.WithCancel(context.Background())

	// 初始化 isCtrl 函数来识别控制包（NACK + standalone ACK + PUSH）
	isCtrl := func(data []byte) bool {
		if len(data) < 11 { // ARQ_OVERHEAD = 11
			return false
		}
		cmd := data[0]
		return cmd == 2 || cmd == 3 || cmd == 4 // CMD_NACK || CMD_ACK || CMD_PUSH
	}

	client := &SimplexClient{
		Simplex: sx,
		buf:     NewSimplexBuffer(addr),
		ctx:     ctx,
		cancel:  cancel,
		isCtrl:  isCtrl,
		failed:  make(chan struct{}),
	}
	sc := addr.SimplexConfig()
	client.buf.EnableDataBudget(sc.DataBudget())
	go client.polling()
	return client, nil
}

type SimplexClient struct {
	Simplex
	buf    *SimplexBuffer
	ctx    context.Context
	cancel context.CancelFunc
	isCtrl func([]byte) bool

	closeOnce sync.Once
	closeErr  error
	failed    chan struct{}

	transportFailOnce sync.Once
	transportErr      atomic.Value
}

func (c *SimplexClient) SetIsControlPacket(f func([]byte) bool) {
	c.isCtrl = f
}

func (c *SimplexClient) ReadFrom(p []byte) (n int, addr net.Addr, err error) {
	if err := c.transportFailure(); err != nil {
		return 0, nil, err
	}
	select {
	case <-c.ctx.Done():
		return 0, nil, io.ErrClosedPipe
	default:
	}
	data, err := c.buf.RecvGet()
	if err != nil || data == nil {
		if terr := c.transportFailure(); terr != nil {
			return 0, nil, terr
		}
		select {
		case <-c.ctx.Done():
			return 0, nil, io.ErrClosedPipe
		default:
		}
		return 0, c.Addr(), err
	}
	return copy(p, data), c.Addr(), nil
}

func (c *SimplexClient) WriteTo(p []byte, addr net.Addr) (n int, err error) {
	if terr := c.transportFailure(); terr != nil {
		return 0, terr
	}
	select {
	case <-c.ctx.Done():
		return 0, io.ErrClosedPipe
	default:
		var packetType SimplexPacketType
		if c.isCtrl(p) {
			packetType = SimplexPacketTypeCTRL
		} else {
			packetType = SimplexPacketTypeDATA
		}
		packet := NewSimplexPacketWithMaxSize(p, packetType, c.Addr().MaxBodySize())
		err = c.buf.PutPackets(packet)
		if err != nil {
			return 0, err
		}
		return len(p), nil
	}
}

func (c *SimplexClient) TryWriteTo(p []byte, addr net.Addr) (n int, err error) {
	if terr := c.transportFailure(); terr != nil {
		return 0, terr
	}
	select {
	case <-c.ctx.Done():
		return 0, io.ErrClosedPipe
	default:
		var packetType SimplexPacketType
		if c.isCtrl(p) {
			packetType = SimplexPacketTypeCTRL
		} else {
			packetType = SimplexPacketTypeDATA
		}
		packet := NewSimplexPacketWithMaxSize(p, packetType, c.Addr().MaxBodySize())
		ok, err := c.buf.TryPutPackets(packet)
		if err != nil {
			return 0, err
		}
		if !ok {
			return 0, arq.ErrWouldBlock
		}
		return len(p), nil
	}
}

func (c *SimplexClient) ARQWriteBudget() int {
	return c.buf.DataBudget()
}

func (c *SimplexClient) LocalAddr() net.Addr {
	return c.Addr()
}

// Close stops the polling goroutine and closes the underlying Simplex transport.
// Without cancelling c.ctx, the polling goroutine would continue sending stale
// ARQ packets to the remote after the connection drops, polluting the channel.
func (c *SimplexClient) Close() error {
	return c.shutdown()
}

func (c *SimplexClient) SetDeadline(t time.Time) error      { return nil }
func (c *SimplexClient) SetReadDeadline(t time.Time) error  { return nil }
func (c *SimplexClient) SetWriteDeadline(t time.Time) error { return nil }

func (c *SimplexClient) shutdown() error {
	c.closeOnce.Do(func() {
		c.cancel()
		c.buf.Close()
		c.closeErr = c.Simplex.Close()
	})
	return c.closeErr
}

func (c *SimplexClient) transportFailure() error {
	if v := c.transportErr.Load(); v != nil {
		return v.(error)
	}
	return nil
}

func (c *SimplexClient) failTransport(err error) {
	if err == nil {
		return
	}
	c.transportFailOnce.Do(func() {
		c.transportErr.Store(err)
		c.buf.Close()
		if c.failed != nil {
			close(c.failed)
		}
	})
}

// maybePreemptPending gives ARQ control packets a chance to cut ahead of a
// previously dequeued data-only batch. This keeps ACK/NACK/PUSH moving even
// when poll-based transports are draining a large backlog slowly.
func (c *SimplexClient) polling() {
	interval := c.Addr().Interval()
	recvTicker := time.NewTicker(interval)
	defer recvTicker.Stop()

	sendTicker := time.NewTicker(interval)
	defer sendTicker.Stop()

	go func() {
		sendInterval := interval
		stallTimeout := simplexAcceptanceStallTimeout(c.Addr())
		var pendingCtrl pendingSendBatch
		var pendingData pendingSendBatch
		for {
			select {
			case <-c.ctx.Done():
				return
			case <-c.failed:
				return
			case <-sendTicker.C:
				// 检查 interval 是否被动态修改
				if ni := c.Addr().Interval(); ni != sendInterval {
					sendInterval = ni
					sendTicker.Reset(ni)
				}
				stallTimeout = simplexAcceptanceStallTimeout(c.Addr())
				// 优先发送控制包
				if !pendingCtrl.ready() {
					body, err := c.buf.GetControlPackets()
					if err != nil {
						if errors.Is(err, io.ErrClosedPipe) || errors.Is(err, context.Canceled) {
							return
						}
						logs.Log.Warnf("simplex client control dequeue failed: %v", err)
						continue
					}
					pendingCtrl.set(body)
				}
				if pendingCtrl.ready() {
					if _, err := c.Send(pendingCtrl.packets, c.Addr()); err != nil {
						if isTerminalSimplexTransportError(err) {
							c.failTransport(err)
							return
						}
						pendingCtrl.markFailure(time.Now())
						if pendingCtrl.stalled(time.Now(), stallTimeout) {
							c.failTransport(newSimplexStallError(stallTimeout, err))
							return
						}
						logs.Log.Warnf("simplex client ctrl send failed: %v", err)
						continue
					}
					c.buf.MarkPacketsSent(pendingCtrl.packets)
					pendingCtrl.clear()
				}
				// 取所有待发数据 (上限 DataBudget)，一次性交给 Send，内部按 maxBodySize 拆分成多 items
				if !pendingData.ready() {
					body, err := c.buf.GetAllDataPackets()
					if err != nil {
						if errors.Is(err, io.ErrClosedPipe) || errors.Is(err, context.Canceled) {
							return
						}
						logs.Log.Warnf("simplex client data dequeue failed: %v", err)
						continue
					}
					pendingData.set(body)
				}
				if pendingData.ready() {
					payloadBytes := 0
					for _, pkt := range pendingData.packets.Packets {
						payloadBytes += len(pkt.Data)
					}
					logs.Log.Debugf("[SimplexClient] send: %d packets, %d payload bytes, %d serialized bytes",
						len(pendingData.packets.Packets), payloadBytes, pendingData.packets.Size())
					if _, err := c.Send(pendingData.packets, c.Addr()); err != nil {
						if isTerminalSimplexTransportError(err) {
							c.failTransport(err)
							return
						}
						pendingData.markFailure(time.Now())
						if pendingData.stalled(time.Now(), stallTimeout) {
							c.failTransport(newSimplexStallError(stallTimeout, err))
							return
						}
						logs.Log.Warnf("simplex client send failed: %v", err)
						continue
					}
					c.buf.MarkPacketsSent(pendingData.packets)
					pendingData.clear()
				}
			}
		}
	}()

	// 接收处理
	recvInterval := interval
	for {
		select {
		case <-c.ctx.Done():
			return
		case <-c.failed:
			return
		case <-recvTicker.C:
			// 检查 interval 是否被动态修改
			if ni := c.Addr().Interval(); ni != recvInterval {
				recvInterval = ni
				recvTicker.Reset(ni)
			}
			// 每次 tick 排空所有可用的 packet，而不是只读一个。
			// 对于 cloud storage 等高延迟传输，一个 cloud storage item 可能包含多个
			// SimplexPacket（多个 ARQ 段），需要全部交付给 ARQ 层才能组装完整帧。
			for {
				pkt, _, err := c.Receive()
				if err != nil {
					if isTerminalSimplexTransportError(err) {
						c.failTransport(err)
						return
					}
					break
				}
				if pkt == nil {
					break
				}
				// 复制数据并放入 ChannelBuffer，保持包边界完整
				pktCopy := make([]byte, len(pkt.Data))
				copy(pktCopy, pkt.Data)
				if err := c.buf.RecvPut(pktCopy); err != nil {
					if errors.Is(err, io.ErrClosedPipe) || errors.Is(err, context.Canceled) {
						return
					}
					logs.Log.Warnf("simplex client recv buffer enqueue failed: %v", err)
					return
				}
			}
		}
	}
}

// recvEntry holds a received packet with its source address.
type recvEntry struct {
	data []byte
	addr net.Addr
	err  error
}

type SimplexServer struct {
	Simplex
	buffers      sync.Map
	recvCh       chan recvEntry // 接收通道，保持包边界
	ctx          context.Context
	cancel       context.CancelFunc
	isCtrl       func([]byte) bool
	onDisconnect func(net.Addr) // 客户端断开回调（由 ARQListener 注册）
	disconnectMu sync.Mutex     // 保护 onDisconnect
	closeOnce    sync.Once
	closeErr     error
}

func (c *SimplexServer) SetIsControlPacket(f func([]byte) bool) {
	c.isCtrl = f
}

func (c *SimplexServer) polling() {
	for {
		select {
		case <-c.ctx.Done():
			return
		default:
			pkt, addr, err := c.Receive()
			if err != nil {
				if isTerminalSimplexTransportError(err) {
					_ = c.enqueueRecv(recvEntry{addr: addr, err: err})
					return
				}
				time.Sleep(DefaultSimplexMinInternal)
				continue
			}
			if pkt == nil {
				time.Sleep(DefaultSimplexMinInternal)
				continue
			}
			// 复制数据并放入共享接收通道，保持包边界完整
			pktCopy := make([]byte, len(pkt.Data))
			copy(pktCopy, pkt.Data)
			if err := c.enqueueRecv(recvEntry{data: pktCopy, addr: addr}); err != nil {
				if errors.Is(err, io.ErrClosedPipe) || errors.Is(err, context.Canceled) {
					return
				}
				logs.Log.Warnf("simplex server recv enqueue failed: %v", err)
			}
		}
	}
}

func (c *SimplexServer) emitPeerError(addr *SimplexAddr, err error) {
	if addr == nil || err == nil {
		return
	}
	select {
	case <-c.ctx.Done():
		return
	default:
	}
	if enqueueErr := c.enqueueRecv(recvEntry{addr: addr, err: err}); enqueueErr != nil &&
		!errors.Is(enqueueErr, io.ErrClosedPipe) && !errors.Is(enqueueErr, context.Canceled) {
		logs.Log.Warnf("simplex server peer error enqueue failed for %s: %v", addr.ID(), enqueueErr)
	}
	c.notifyDisconnect(addr)
}

func (c *SimplexServer) enqueueRecv(entry recvEntry) error {
	for {
		select {
		case <-c.ctx.Done():
			return io.ErrClosedPipe
		case c.recvCh <- entry:
			return nil
		default:
			time.Sleep(simplexQueueBackpressurePollInterval)
		}
	}
}

func (c *SimplexServer) GetBuffer(addr *SimplexAddr) *SimplexBuffer {
	buf, ok := c.buffers.Load(addr.ID())
	if ok {
		return buf.(*SimplexBuffer)
	}
	sbuf := NewSimplexBuffer(addr)
	sbuf.EnableDataBudget(addr.SimplexConfig().DataBudget())
	c.buffers.Store(addr.ID(), sbuf)
	go func() {
		stallTimeout := simplexAcceptanceStallTimeout(addr)
		var pendingCtrl pendingSendBatch
		var pendingData pendingSendBatch
		for {
			select {
			case <-c.ctx.Done():
				return
			default:
			}

			stallTimeout = simplexAcceptanceStallTimeout(addr)
			if !pendingCtrl.ready() {
				body, err := sbuf.GetControlPackets()
				if err != nil {
					if _, loaded := c.buffers.LoadAndDelete(addr.ID()); loaded {
						c.emitPeerError(addr, io.ErrClosedPipe)
					}
					return
				}
				pendingCtrl.set(body)
			}
			if !pendingCtrl.ready() && !pendingData.ready() {
				body, err := sbuf.GetAllDataPackets()
				if err != nil {
					if _, loaded := c.buffers.LoadAndDelete(addr.ID()); loaded {
						c.emitPeerError(addr, io.ErrClosedPipe)
					}
					return
				}
				if body.Size() == 0 {
					time.Sleep(time.Millisecond)
					continue
				}
				pendingData.set(body)
			}

			active := &pendingCtrl
			if !active.ready() {
				active = &pendingData
			}
			if !active.ready() {
				time.Sleep(time.Millisecond)
				continue
			}

			if _, err := c.Send(active.packets, addr); err != nil {
				if isTerminalSimplexTransportError(err) {
					logs.Log.Infof("simplex server send: transport closed for %s, stopping", addr.ID())
					if buf, loaded := c.buffers.LoadAndDelete(addr.ID()); loaded {
						buf.(*SimplexBuffer).Close()
						c.emitPeerError(addr, err)
					}
					return
				}
				if isExpectedTransportShutdownError(err) {
					logs.Log.Debugf("simplex server send stopping during transport shutdown for %s: %v", addr.ID(), err)
				} else {
					logs.Log.Warnf("simplex server send failed: %v", err)
				}
				active.markFailure(time.Now())
				if active.stalled(time.Now(), stallTimeout) {
					stallErr := newSimplexStallError(stallTimeout, err)
					logs.Log.Warnf("simplex server send stalled for %s, stopping: %v", addr.ID(), stallErr)
					if buf, loaded := c.buffers.LoadAndDelete(addr.ID()); loaded {
						buf.(*SimplexBuffer).Close()
						c.emitPeerError(addr, stallErr)
					}
					return
				}
				time.Sleep(DefaultSimplexMinInternal)
				continue
			}
			sbuf.MarkPacketsSent(active.packets)
			active.clear()
		}
	}()
	return sbuf
}

// Server端方法实现
func (c *SimplexServer) ReadFrom(p []byte) (n int, addr net.Addr, err error) {
	select {
	case <-c.ctx.Done():
		return 0, nil, io.ErrClosedPipe
	case entry := <-c.recvCh:
		if entry.err != nil {
			return 0, entry.addr, entry.err
		}
		return copy(p, entry.data), entry.addr, nil
	default:
		return 0, nil, nil
	}
}

func (c *SimplexServer) WriteTo(p []byte, addr net.Addr) (n int, err error) {
	select {
	case <-c.ctx.Done():
		return 0, io.ErrClosedPipe
	default:
		a, ok := addr.(*SimplexAddr)
		if !ok {
			return 0, fmt.Errorf("invalid address type: %T", addr)
		}

		// 获取或创建buffer
		buf := c.GetBuffer(a)

		// 创建数据包
		var packetType SimplexPacketType
		if c.isCtrl(p) {
			packetType = SimplexPacketTypeCTRL
		} else {
			packetType = SimplexPacketTypeDATA
		}

		// 创建数据包并添加到writeChannel
		packet := NewSimplexPacketWithMaxSize(p, packetType, a.MaxBodySize())
		err = buf.PutPackets(packet)
		if err != nil {
			return 0, err
		}

		return len(p), nil
	}
}

func (c *SimplexServer) TryWriteTo(p []byte, addr net.Addr) (n int, err error) {
	select {
	case <-c.ctx.Done():
		return 0, io.ErrClosedPipe
	default:
		a, ok := addr.(*SimplexAddr)
		if !ok {
			return 0, fmt.Errorf("invalid address type: %T", addr)
		}

		buf := c.GetBuffer(a)

		var packetType SimplexPacketType
		if c.isCtrl(p) {
			packetType = SimplexPacketTypeCTRL
		} else {
			packetType = SimplexPacketTypeDATA
		}

		packet := NewSimplexPacketWithMaxSize(p, packetType, a.MaxBodySize())
		accepted, err := buf.TryPutPackets(packet)
		if err != nil {
			return 0, err
		}
		if !accepted {
			return 0, arq.ErrWouldBlock
		}
		return len(p), nil
	}
}

func (c *SimplexServer) ARQWriteBudget() int {
	return c.Addr().SimplexConfig().DataBudget()
}

func (c *SimplexServer) LocalAddr() net.Addr {
	return c.Addr()
}

// OnDisconnect 注册客户端断开回调。由 ARQListener 在创建时注册。
// 回调会在 transport 层检测到客户端断开时被调用（从 GetBuffer goroutine）。
// 回调必须是线程安全的，因为可能从多个 goroutine 同时调用。
func (c *SimplexServer) OnDisconnect(fn func(net.Addr)) {
	c.disconnectMu.Lock()
	c.onDisconnect = fn
	c.disconnectMu.Unlock()
}

// notifyDisconnect 安全地调用 onDisconnect 回调。
func (c *SimplexServer) notifyDisconnect(addr net.Addr) {
	c.disconnectMu.Lock()
	fn := c.onDisconnect
	c.disconnectMu.Unlock()
	if fn != nil {
		fn(addr)
	}
}

func (c *SimplexServer) SetDeadline(t time.Time) error      { return nil }
func (c *SimplexServer) SetReadDeadline(t time.Time) error  { return nil }
func (c *SimplexServer) SetWriteDeadline(t time.Time) error { return nil }

func (c *SimplexServer) Close() error {
	c.closeOnce.Do(func() {
		c.cancel()
		c.buffers.Range(func(_, value interface{}) bool {
			value.(*SimplexBuffer).Close()
			return true
		})
		c.closeErr = c.Simplex.Close()
	})
	return c.closeErr
}

// simplexSessionNotifier is an optional interface implemented by underlying Simplex
// transports (e.g. cloud storageServer) that can notify the wrapper when sessions are cleaned up.
// This allows SimplexServer to close its wrapper buffers and propagate disconnect to upper layers.
type simplexSessionNotifier interface {
	OnSessionClosed(func(*SimplexAddr))
}

func NewSimplexServer(network, address string) (*SimplexServer, error) {
	creator, err := GetSimplexServer(network)
	if err != nil {
		return nil, err
	}
	sx, err := creator(network, address)
	if err != nil {
		return nil, err
	}
	ctx, cancel := context.WithCancel(context.Background())

	// 导入 SimpleARQChecker 来检查控制包
	// 该函数由 x/arq 包提供，用于识别 ARQ 协议的控制包（NACK）
	var isCtrl func([]byte) bool

	// SimpleARQChecker 识别 CMD_NACK / CMD_ACK / CMD_PUSH
	isCtrl = func(data []byte) bool {
		if len(data) < 11 { // ARQ_OVERHEAD = 11
			return false
		}
		cmd := data[0]
		return cmd == 2 || cmd == 3 || cmd == 4 // CMD_NACK || CMD_ACK || CMD_PUSH
	}

	server := &SimplexServer{
		Simplex: sx,
		recvCh:  make(chan recvEntry, defaultSimplexServerRecvChannelCapacity),
		ctx:     ctx,
		cancel:  cancel,
		isCtrl:  isCtrl,
	}
	go server.polling()

	// 如果底层 transport 支持 session 生命周期通知，注册回调以清理 wrapper buffer。
	// 没有这个回调，wrapper buffer 和对应的 GetBuffer goroutine 会在 transport 清理 session 后泄漏。
	if notifier, ok := sx.(simplexSessionNotifier); ok {
		notifier.OnSessionClosed(func(addr *SimplexAddr) {
			if buf, loaded := server.buffers.LoadAndDelete(addr.ID()); loaded {
				buf.(*SimplexBuffer).Close()
				server.emitPeerError(addr, io.ErrClosedPipe)
			}
		})
	}

	return server, nil
}
