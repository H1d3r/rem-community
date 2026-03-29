package simplex

import (
	"context"
	"io"
	"sync"
	"time"

	"github.com/chainreactors/rem/x/utils"
)

// SimplexBuffer 使用PeekableChannel存储发送数据包，ChannelBuffer存储接收数据包
type SimplexBuffer struct {
	recvBuf *utils.ChannelBuffer // 接收：保持包边界的 packet buffer
	addr    *SimplexAddr

	closeOnce sync.Once

	queueMu   sync.Mutex
	closed    bool
	ctrlQueue []*SimplexPacket
	dataQueue []*SimplexPacket
	ctrlCap   int
	dataCap   int

	creditMu    sync.Mutex
	dataBudget  int
	dataQueued  int
	creditReady chan struct{}
}

var (
	defaultSimplexCtrlChannelCapacity       = 64
	defaultSimplexSendChannelCapacity       = 512
	defaultSimplexRecvChannelCapacity       = 128
	defaultSimplexRecvChannelLargePacketCap = 32
	defaultSimplexRecvChannelHugePacketCap  = 16
)

func NewSimplexBuffer(addr *SimplexAddr) *SimplexBuffer {
	// 根据包大小动态调整接收通道容量：大包少 slot 控制内存
	recvCap := defaultSimplexRecvChannelCapacity
	if addr.maxBodySize > 256*1024 {
		recvCap = defaultSimplexRecvChannelHugePacketCap // >256KB 包: 最多缓存 16 个 (如 4MB*16=64MB)
	} else if addr.maxBodySize > 64*1024 {
		recvCap = defaultSimplexRecvChannelLargePacketCap // >64KB 包: 最多缓存 32 个
	}

	// 发送通道容量需要容纳 ARQ 一个完整窗口的 segment 输出。
	// ARQ flush() 每 10ms 可输出 WND_SIZE(128) 个 segment，
	// 而 SimplexClient polling 每 interval(3s) 才消费一次。
	// 容量太小会把上层长流量/多路复用流控一并卡住。
	sendCap := defaultSimplexSendChannelCapacity

	return &SimplexBuffer{
		recvBuf:     utils.NewChannel(recvCap),
		addr:        addr,
		ctrlCap:     defaultSimplexCtrlChannelCapacity,
		dataCap:     sendCap,
		creditReady: make(chan struct{}, 1),
	}
}

func (b *SimplexBuffer) Addr() *SimplexAddr {
	return b.addr
}

// RecvPut 写入一个接收到的完整包（保持包边界）
func (b *SimplexBuffer) RecvPut(data []byte) error {
	return b.recvBuf.PutWait(context.Background(), data)
}

// RecvGet 读取一个完整的接收包（保持包边界）
func (b *SimplexBuffer) RecvGet() ([]byte, error) {
	return b.recvBuf.Get()
}

// PutPacket: 按类型投放到不同通道
func (b *SimplexBuffer) PutPacket(pkt *SimplexPacket) error {
	_, err := b.putPacketWait(context.Background(), pkt)
	return err
}

func (b *SimplexBuffer) TryPutPacket(pkt *SimplexPacket) (bool, error) {
	return b.tryPutPacket(pkt)
}

func (b *SimplexBuffer) dataPacketBytes(pkt *SimplexPacket) int {
	if pkt == nil || pkt.PacketType != SimplexPacketTypeDATA {
		return 0
	}
	return pkt.Size()
}

func (b *SimplexBuffer) signalCreditReady() {
	select {
	case b.creditReady <- struct{}{}:
	default:
	}
}

func (b *SimplexBuffer) waitReserveData(ctx context.Context, bytes int) error {
	if bytes <= 0 || b.dataBudget <= 0 {
		return nil
	}
	for {
		b.creditMu.Lock()
		if b.dataQueued+bytes <= b.dataBudget {
			b.dataQueued += bytes
			b.creditMu.Unlock()
			return nil
		}
		b.creditMu.Unlock()

		if ctx != nil {
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-b.creditReady:
			case <-time.After(simplexQueueBackpressurePollInterval):
			}
		} else {
			select {
			case <-b.creditReady:
			case <-time.After(simplexQueueBackpressurePollInterval):
			}
		}
	}
}

func (b *SimplexBuffer) tryReserveData(bytes int) (bool, error) {
	if bytes <= 0 || b.dataBudget <= 0 {
		return true, nil
	}
	b.creditMu.Lock()
	defer b.creditMu.Unlock()
	if b.dataQueued+bytes > b.dataBudget {
		return false, nil
	}
	b.dataQueued += bytes
	return true, nil
}

func (b *SimplexBuffer) releaseData(bytes int) {
	if bytes <= 0 {
		return
	}
	b.creditMu.Lock()
	b.dataQueued -= bytes
	if b.dataQueued < 0 {
		b.dataQueued = 0
	}
	b.creditMu.Unlock()
	b.signalCreditReady()
}

func (b *SimplexBuffer) putPacketWait(ctx context.Context, pkt *SimplexPacket) (bool, error) {
	if pkt == nil {
		return true, nil
	}
	bytes := b.dataPacketBytes(pkt)
	if err := b.waitReserveData(ctx, bytes); err != nil {
		return false, err
	}

	for {
		b.queueMu.Lock()
		if b.closed {
			b.queueMu.Unlock()
			b.releaseData(bytes)
			return false, io.ErrClosedPipe
		}
		if b.enqueuePacketLocked(pkt) {
			b.queueMu.Unlock()
			return true, nil
		}
		b.queueMu.Unlock()

		if ctx != nil {
			select {
			case <-ctx.Done():
				b.releaseData(bytes)
				return false, ctx.Err()
			case <-time.After(simplexQueueBackpressurePollInterval):
			}
		} else {
			time.Sleep(simplexQueueBackpressurePollInterval)
		}
	}
}

func (b *SimplexBuffer) tryPutPacket(pkt *SimplexPacket) (bool, error) {
	if pkt == nil {
		return true, nil
	}
	bytes := b.dataPacketBytes(pkt)
	ok, err := b.tryReserveData(bytes)
	if err != nil || !ok {
		return ok, err
	}

	b.queueMu.Lock()
	defer b.queueMu.Unlock()

	if b.closed {
		b.releaseData(bytes)
		return false, io.ErrClosedPipe
	}
	if !b.enqueuePacketLocked(pkt) {
		b.releaseData(bytes)
		return false, nil
	}
	return true, nil
}

func (b *SimplexBuffer) PutPacketFront(pkt *SimplexPacket) error {
	if pkt == nil {
		return nil
	}

	b.queueMu.Lock()
	defer b.queueMu.Unlock()

	if b.closed {
		return io.ErrClosedPipe
	}

	switch pkt.PacketType {
	case SimplexPacketTypeCTRL:
		b.ctrlQueue = append([]*SimplexPacket{pkt}, b.ctrlQueue...)
	default:
		b.dataQueue = append([]*SimplexPacket{pkt}, b.dataQueue...)
	}
	return nil
}

func (b *SimplexBuffer) PutPackets(packets *SimplexPackets) error {
	for _, packet := range packets.Packets {
		if _, err := b.putPacketWait(context.Background(), packet); err != nil {
			return err
		}
	}
	return nil
}

func (b *SimplexBuffer) TryPutPackets(packets *SimplexPackets) (bool, error) {
	if packets == nil {
		return true, nil
	}

	dataBytes := 0
	ctrlCount := 0
	dataCount := 0
	for _, packet := range packets.Packets {
		if packet == nil {
			continue
		}
		dataBytes += b.dataPacketBytes(packet)
		if packet.PacketType == SimplexPacketTypeCTRL {
			ctrlCount++
		} else {
			dataCount++
		}
	}

	ok, err := b.tryReserveData(dataBytes)
	if err != nil || !ok {
		return ok, err
	}

	b.queueMu.Lock()
	defer b.queueMu.Unlock()

	if b.closed {
		b.releaseData(dataBytes)
		return false, io.ErrClosedPipe
	}
	if len(b.ctrlQueue)+ctrlCount > b.ctrlCap || len(b.dataQueue)+dataCount > b.dataCap {
		b.releaseData(dataBytes)
		return false, nil
	}

	for _, packet := range packets.Packets {
		if packet == nil {
			continue
		}
		b.enqueuePacketLocked(packet)
	}
	return true, nil
}

func (b *SimplexBuffer) RequeuePacketsFront(packets *SimplexPackets) error {
	if packets == nil {
		return nil
	}
	for i := len(packets.Packets) - 1; i >= 0; i-- {
		if err := b.PutPacketFront(packets.Packets[i]); err != nil {
			return err
		}
	}
	return nil
}

func (b *SimplexBuffer) HasQueuedControl() bool {
	if b == nil {
		return false
	}
	b.queueMu.Lock()
	defer b.queueMu.Unlock()
	return len(b.ctrlQueue) > 0
}

func (b *SimplexBuffer) DataBudget() int {
	if b == nil {
		return 0
	}
	return b.dataBudget
}

func (b *SimplexBuffer) EnableDataBudget(limit int) {
	if b == nil {
		return
	}
	b.creditMu.Lock()
	b.dataBudget = limit
	b.creditMu.Unlock()
	b.signalCreditReady()
}

func (b *SimplexBuffer) MarkPacketsSent(packets *SimplexPackets) {
	if packets == nil {
		return
	}
	released := 0
	for _, packet := range packets.Packets {
		released += b.dataPacketBytes(packet)
	}
	b.releaseData(released)
}

// GetPacket: 优先从ctrlChannel取
func (b *SimplexBuffer) GetPacket() (*SimplexPacket, error) {
	b.queueMu.Lock()
	defer b.queueMu.Unlock()

	if b.closed {
		return nil, io.ErrClosedPipe
	}
	if len(b.ctrlQueue) > 0 {
		packet := b.ctrlQueue[0]
		b.ctrlQueue = b.ctrlQueue[1:]
		return packet, nil
	}
	if len(b.dataQueue) > 0 {
		packet := b.dataQueue[0]
		b.dataQueue = b.dataQueue[1:]
		return packet, nil
	}
	return nil, nil
}

func (b *SimplexBuffer) Peek() (*SimplexPacket, error) {
	b.queueMu.Lock()
	defer b.queueMu.Unlock()

	if b.closed {
		return nil, io.ErrClosedPipe
	}
	if len(b.ctrlQueue) > 0 {
		return b.ctrlQueue[0], nil
	}
	if len(b.dataQueue) > 0 {
		return b.dataQueue[0], nil
	}
	return nil, nil
}

func (b *SimplexBuffer) GetPackets() (*SimplexPackets, error) {
	b.queueMu.Lock()
	defer b.queueMu.Unlock()

	if b.closed {
		return nil, io.ErrClosedPipe
	}

	packets := NewSimplexPackets()
	b.appendFromQueueLocked(&b.ctrlQueue, packets)
	b.appendFromQueueLocked(&b.dataQueue, packets)
	return packets, nil
}

func (b *SimplexBuffer) GetControlPackets() (*SimplexPackets, error) {
	b.queueMu.Lock()
	defer b.queueMu.Unlock()

	if b.closed {
		return nil, io.ErrClosedPipe
	}

	packets := NewSimplexPackets()
	b.appendFromQueueLocked(&b.ctrlQueue, packets)
	return packets, nil
}

func (b *SimplexBuffer) GetDataPackets() (*SimplexPackets, error) {
	b.queueMu.Lock()
	defer b.queueMu.Unlock()

	if b.closed {
		return nil, io.ErrClosedPipe
	}

	packets := NewSimplexPackets()
	b.appendFromQueueLocked(&b.dataQueue, packets)
	return packets, nil
}

// GetAllDataPackets drains all queued data packets up to dataBudget bytes.
// Unlike GetDataPackets (capped at maxBodySize per call), this returns the full
// budget worth of data so the transport layer can split it into multiple items.
func (b *SimplexBuffer) GetAllDataPackets() (*SimplexPackets, error) {
	b.queueMu.Lock()
	defer b.queueMu.Unlock()

	if b.closed {
		return nil, io.ErrClosedPipe
	}

	packets := NewSimplexPackets()
	budget := b.dataBudget
	if budget <= 0 {
		budget = b.addr.maxBodySize
	}
	for len(b.dataQueue) > 0 {
		pkt := b.dataQueue[0]
		if packets.Size()+pkt.Size() > budget {
			break
		}
		packets.Append(pkt)
		b.dataQueue = b.dataQueue[1:]
	}
	return packets, nil
}

// Close 关闭 SimplexBuffer，释放所有内部 channel 资源。
// 关闭后 PutPacket/GetPacket 返回 io.ErrClosedPipe。
func (b *SimplexBuffer) Close() error {
	b.closeOnce.Do(func() {
		b.queueMu.Lock()
		b.closed = true
		b.ctrlQueue = nil
		b.dataQueue = nil
		b.queueMu.Unlock()
		b.recvBuf.Close()
		b.creditMu.Lock()
		b.dataQueued = 0
		b.creditMu.Unlock()
		b.signalCreditReady()
	})
	return nil
}

func (b *SimplexBuffer) ControlQueueSize() int {
	b.queueMu.Lock()
	defer b.queueMu.Unlock()
	return len(b.ctrlQueue)
}

func (b *SimplexBuffer) DataQueueSize() int {
	b.queueMu.Lock()
	defer b.queueMu.Unlock()
	return len(b.dataQueue)
}

func (b *SimplexBuffer) enqueuePacketLocked(pkt *SimplexPacket) bool {
	switch pkt.PacketType {
	case SimplexPacketTypeCTRL:
		if len(b.ctrlQueue) >= b.ctrlCap {
			return false
		}
		b.ctrlQueue = append(b.ctrlQueue, pkt)
	default:
		if len(b.dataQueue) >= b.dataCap {
			return false
		}
		b.dataQueue = append(b.dataQueue, pkt)
	}
	return true
}

func (b *SimplexBuffer) appendFromQueueLocked(queue *[]*SimplexPacket, packets *SimplexPackets) {
	for len(*queue) > 0 {
		pkt := (*queue)[0]
		if packets.Size()+pkt.Size() > b.addr.maxBodySize {
			break
		}
		packets.Append(pkt)
		*queue = (*queue)[1:]
	}
}

// AsymBuffer 非对称通信缓冲区，用于DNS、HTTP等只允许客户端主动发送的协议
// 特点：客户端只能发送请求并接收响应，服务端只能接收请求并发送响应
type AsymBuffer struct {
	readBuf    *SimplexBuffer // 接收数据的缓冲区
	writeBuf   *SimplexBuffer // 发送数据的缓冲区
	addr       *SimplexAddr   // 地址信息
	lastActive time.Time
}

// NewAsymBuffer 创建新的非对称缓冲区
func NewAsymBuffer(addr *SimplexAddr) *AsymBuffer {
	return &AsymBuffer{
		readBuf:    NewSimplexBuffer(addr),
		writeBuf:   NewSimplexBuffer(addr),
		addr:       addr,
		lastActive: time.Now(),
	}
}

// Close 关闭缓冲区，释放内部 SimplexBuffer 资源。
func (buf *AsymBuffer) Close() error {
	buf.readBuf.Close()
	buf.writeBuf.Close()
	return nil
}

// Touch updates the last active timestamp.
func (buf *AsymBuffer) Touch() {
	buf.lastActive = time.Now()
}

// LastActive returns the last active timestamp.
func (buf *AsymBuffer) LastActive() time.Time {
	return buf.lastActive
}

// Addr 返回地址信息
func (buf *AsymBuffer) Addr() *SimplexAddr {
	return buf.addr
}

// ReadBuf 返回读缓冲区
func (buf *AsymBuffer) ReadBuf() *SimplexBuffer {
	return buf.readBuf
}

// WriteBuf 返回写缓冲区
func (buf *AsymBuffer) WriteBuf() *SimplexBuffer {
	return buf.writeBuf
}

// AsymServerReceive iterates all AsymBuffers in a sync.Map and returns the first
// available packet from any client's ReadBuf. Shared by DNS/HTTP server Receive().
func AsymServerReceive(ctx context.Context, buffers *sync.Map) (*SimplexPacket, *SimplexAddr, error) {
	var pkt *SimplexPacket
	var addr *SimplexAddr
	var pktErr error

	buffers.Range(func(_, value interface{}) bool {
		buf := value.(*AsymBuffer)
		p, err := buf.ReadBuf().GetPacket()
		if p != nil {
			pkt = p
			addr = buf.Addr()
			pktErr = err
			return false
		}
		return true
	})

	if pkt != nil {
		return pkt, addr, pktErr
	}

	select {
	case <-ctx.Done():
		return nil, nil, io.ErrClosedPipe
	default:
		return nil, nil, nil
	}
}

// AsymServerSend routes packets to the specified client's WriteBuf via LoadOrStore.
// Shared by DNS/HTTP server Send().
func AsymServerSend(ctx context.Context, buffers *sync.Map, pkts *SimplexPackets, addr *SimplexAddr) (int, error) {
	select {
	case <-ctx.Done():
		return 0, io.ErrClosedPipe
	default:
		if pkts == nil || pkts.Size() == 0 {
			return 0, nil
		}
		value, _ := buffers.LoadOrStore(addr.id, NewAsymBuffer(addr))
		buf := value.(*AsymBuffer)
		if err := buf.WriteBuf().PutPackets(pkts); err != nil {
			return 0, err
		}
		return pkts.Size(), nil
	}
}
