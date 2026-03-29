// Package arq implements a selective-repeat ARQ protocol for low-frequency,
// high-latency transports such as cloud storage-style polling channels.
//
// Contract:
//   - Data returned to the upper layer is ordered and reliable.
//   - If ARQ can no longer preserve that guarantee, the session must fail.
//
// Responsibilities:
//   - ARQ owns ordered, reliable delivery semantics for the logical conn.
//   - The underlying transport only reports packet/request level failures.
//
// Design:
//   - Data is retransmitted on an RTO schedule.
//   - Standalone ACKs proactively release sender windows on one-way / polling links.
//   - Every in-flight segment has a bounded retransmission budget.
//   - If any segment exceeds that budget without being acknowledged, ARQ marks
//     the session failed instead of silently discarding data or blocking forever.
package arq

import (
	"encoding/binary"
	"errors"
	"fmt"
	"sync"
	"time"
)

var nowFunc = time.Now

// ErrWouldBlock indicates the send buffer is full and the caller should retry later.
var ErrWouldBlock = errors.New("arq: would block")

// SimpleARQChecker 简单ARQ协议的包类型判断（默认）。
// NACK / standalone ACK / PUSH 都走 CTRL 通道优先发送，避免窗口释放被数据队列阻塞。
func SimpleARQChecker(data []byte) bool {
	if len(data) < ARQ_OVERHEAD {
		return false
	}
	cmd := data[0]
	return cmd == CMD_NACK || cmd == CMD_ACK || cmd == CMD_PUSH
}

// 包类型常量
const (
	CMD_DATA = 1
	CMD_NACK = 2 // 负确认，要求重传特定包
	CMD_ACK  = 3 // 独立 ACK 包
	CMD_PUSH = 4 // 优先数据：不走 snd_queue/snd_buf，直接发送，不受 backpressure 影响
)

// 配置常量 (用作默认值)
const (
	ARQ_OVERHEAD       = 11                     // 包头大小: 1+4+4+2 (cmd+sn+ack+len)
	ARQ_MTU            = 1400                   // 最大传输单元
	ARQ_MSS            = ARQ_MTU - ARQ_OVERHEAD // 最大段大小
	ARQ_MAX_PAYLOAD    = 0xFFFF                 // len 字段是 uint16，单 segment payload 不能超过 65535
	ARQ_MAX_MTU        = ARQ_OVERHEAD + ARQ_MAX_PAYLOAD
	ARQ_WND_SIZE       = 128  // 窗口大小（匹配 cloud storage 190KB/POST 容量）
	ARQ_RTO            = 6000 // 重传超时时间(ms)
	ARQ_INTERVAL       = 100  // 更新间隔(ms)
	ARQ_NACK_THRESHOLD = 1    // 触发NACK的丢包阈值 (降为1，适配低频场景)
	ARQ_MAX_RETRANS    = 3    // 单个 segment 最大重传次数（不含首次发送）
)

// ARQConfig holds per-instance configuration for ARQ.
// Zero values mean "use default".
type ARQConfig struct {
	MTU                   int // 0 = default (1400)
	WndSize               int // 发送窗口大小 (0 = default 128)
	Timeout               int // ms, 0 = no timeout
	RTO                   int // ms, 0 = default (6000) - retransmission interval, NACK interval, standalone ACK timer
	StandaloneAckSegments int // 0 = default (WndSize/2)
	MaxRetransmissions    int // 0 = default (3); exceeded budget fails the session
}

// Segment 数据段
type Segment struct {
	cmd             uint8  // 命令类型
	sn              uint32 // 序列号
	len             uint16 // 数据长度
	ts              uint32 // 最近一次发送/重传时间戳
	retransmissions int    // 已执行的重传次数（不含首次发送）
	data            []byte // 数据
}

// ErrDeliveryFailed indicates ARQ can no longer guarantee reliable, ordered delivery.
var ErrDeliveryFailed = errors.New("arq reliable delivery guarantee lost")

// DeliveryFailureError identifies the segment that exhausted its retransmission budget.
type DeliveryFailureError struct {
	SN                 uint32
	Retransmissions    int
	MaxRetransmissions int
}

func (e *DeliveryFailureError) Error() string {
	return fmt.Sprintf("%s: segment %d exceeded retransmission budget (%d/%d)",
		ErrDeliveryFailed, e.SN, e.Retransmissions, e.MaxRetransmissions)
}

func (e *DeliveryFailureError) Unwrap() error {
	return ErrDeliveryFailed
}

// ARQ 简化的ARQ实现
type ARQ struct {
	mu   sync.Mutex // 保护所有状态的并发访问
	conv uint32     // 会话ID

	// 可配置参数
	mtu                int // 最大传输单元
	mss                int // 最大段大小 (mtu - overhead)
	wnd_size           int // 发送窗口大小
	timeout            int // 默认超时时间(ms)，0表示无超时
	rto                int // 重传超时(ms) - retransmission, NACK interval, standalone ACK timer
	maxRetransmissions int

	// 状态
	snd_nxt uint32 // 下一个发送序列号
	snd_una uint32 // 最早未确认的 SN
	rcv_nxt uint32 // 下一个期望接收序列号

	// 缓冲区
	snd_queue   []Segment          // 发送队列
	snd_bytes   int                // snd_queue 中待发送的 payload 字节数
	snd_pending []byte             // 不足 MSS 的尾部暂存，下次 Queue 时合并
	snd_buf     []Segment          // 发送缓冲区(等待确认或超时)
	rcv_buf   map[uint32]Segment // 接收缓冲区(乱序包)
	rcv_queue []byte             // 接收队列(有序数据)

	// 接收端状态跟踪
	highest_rcv uint32 // 接收到的最高序列号
	nack_count  int    // 连续gap计数
	nack_sent   uint32 // 最后发送NACK的时间
	nack_retry  int    // NACK重试次数
	nack_gap_ts uint32 // 首次检测到 gap 的时间戳 (用于时间触发)

	// 时间相关
	current               uint32 // 当前时间
	last_ack_sent         uint32 // 上次发送 standalone ACK 的时间（定时兜底）
	rcv_since_ack         uint32 // 自上次 ACK 后接收到的 segment 数量
	standaloneAckSegments uint32 // standalone ACK 计数触发阈值（按 segment 数）

	// 旁路信道 (CMD_PUSH)
	push_queue [][]byte // 接收到的 CMD_PUSH 数据，独立于 rcv_queue

	// 输出函数
	output func([]byte)

	// 失败状态：一旦无法继续保证可靠交付，整个 session 必须失败。
	err error
}

// NewSimpleARQ 创建新的ARQ实例
func NewSimpleARQ(conv uint32, output func([]byte)) *ARQ {
	return NewSimpleARQWithMTU(conv, output, ARQ_MTU, 0)
}

// NewSimpleARQWithOutput creates an ARQ instance, preserving the output function reference
// for cases where the caller needs to swap the output (e.g., receiver restart tests).
func NewSimpleARQWithOutput(conv uint32, output func([]byte)) *ARQ {
	return NewARQWithConfig(conv, output, ARQConfig{})
}

// NewSimpleARQWithMTU 创建新的ARQ实例，支持自定义MTU和超时
func NewSimpleARQWithMTU(conv uint32, output func([]byte), mtu int, timeout int) *ARQ {
	return NewARQWithConfig(conv, output, ARQConfig{
		MTU:     mtu,
		Timeout: timeout,
	})
}

// NewARQWithConfig 创建新的ARQ实例，支持完整配置
func NewARQWithConfig(conv uint32, output func([]byte), cfg ARQConfig) *ARQ {
	if cfg.MTU <= ARQ_OVERHEAD {
		cfg.MTU = ARQ_MTU
	}
	if cfg.MTU > ARQ_MAX_MTU {
		cfg.MTU = ARQ_MAX_MTU
	}
	if cfg.RTO == 0 {
		cfg.RTO = ARQ_RTO
	}
	if cfg.WndSize <= 0 {
		cfg.WndSize = ARQ_WND_SIZE
	}
	if cfg.StandaloneAckSegments <= 0 {
		cfg.StandaloneAckSegments = cfg.WndSize / 2
	}
	if cfg.MaxRetransmissions <= 0 {
		cfg.MaxRetransmissions = ARQ_MAX_RETRANS
	}

	arq := &ARQ{
		conv:                  conv,
		mtu:                   cfg.MTU,
		mss:                   cfg.MTU - ARQ_OVERHEAD,
		wnd_size:              cfg.WndSize,
		timeout:               cfg.Timeout,
		rto:                   cfg.RTO,
		maxRetransmissions:    cfg.MaxRetransmissions,
		standaloneAckSegments: uint32(cfg.StandaloneAckSegments),
		rcv_buf:               make(map[uint32]Segment),
		rcv_queue:             make([]byte, 0),
		output:                output,
		current:               currentMs(),
	}
	return arq
}

// currentMs 获取当前时间戳(毫秒)
func currentMs() uint32 {
	return uint32(nowFunc().UnixNano() / 1000000)
}

func (arq *ARQ) queueLocked(data []byte) error {
	if len(data) == 0 {
		return nil
	}
	if arq.err != nil {
		return arq.err
	}

	// 合并 pending 碎片：不足 MSS 的尾部和新数据拼接
	if len(arq.snd_pending) > 0 {
		data = append(arq.snd_pending, data...)
		arq.snd_pending = nil
	}

	// 只创建满 MSS 的 segment，不足 MSS 的尾部暂存
	for len(data) >= arq.mss {
		size := arq.mss
		seg := Segment{
			cmd:  CMD_DATA,
			sn:   arq.snd_nxt,
			len:  uint16(size),
			data: make([]byte, size),
		}
		copy(seg.data, data[:size])
		arq.snd_queue = append(arq.snd_queue, seg)
		arq.snd_bytes += size
		arq.snd_nxt++
		data = data[size:]
	}
	if len(data) > 0 {
		arq.snd_pending = make([]byte, len(data))
		copy(arq.snd_pending, data)
	}
	return nil
}

// FlushPending forces any buffered sub-MSS data into snd_queue as a final segment.
func (arq *ARQ) FlushPending() {
	arq.mu.Lock()
	defer arq.mu.Unlock()
	arq.flushPendingLocked()
}

func (arq *ARQ) flushPendingLocked() {
	if len(arq.snd_pending) == 0 {
		return
	}
	size := len(arq.snd_pending)
	seg := Segment{
		cmd:  CMD_DATA,
		sn:   arq.snd_nxt,
		len:  uint16(size),
		data: arq.snd_pending,
	}
	arq.snd_queue = append(arq.snd_queue, seg)
	arq.snd_bytes += size
	arq.snd_nxt++
	arq.snd_pending = nil
}

// Queue 发送数据，支持自动分片，并在 ARQ 已失败时返回 error。
func (arq *ARQ) Queue(data []byte) error {
	arq.mu.Lock()
	defer arq.mu.Unlock()
	return arq.queueLocked(data)
}

// Send 发送数据，支持自动分片。
// 保持兼容：调用方如不关心 error，可继续使用该方法。
func (arq *ARQ) Send(data []byte) {
	_ = arq.Queue(data)
}

// Recv 接收数据
func (arq *ARQ) Recv() []byte {
	arq.mu.Lock()
	defer arq.mu.Unlock()

	if len(arq.rcv_queue) == 0 {
		return nil
	}

	data := make([]byte, len(arq.rcv_queue))
	copy(data, arq.rcv_queue)
	arq.rcv_queue = arq.rcv_queue[:0]
	return data
}

// Input 输入接收到的数据包
func (arq *ARQ) Input(data []byte) {
	if len(data) < ARQ_OVERHEAD {
		return
	}

	arq.mu.Lock()
	defer arq.mu.Unlock()
	if arq.err != nil {
		return
	}

	for len(data) >= ARQ_OVERHEAD {
		// 解析 11 字节包头: cmd(1) + sn(4) + ack(4) + len(2)
		cmd := data[0]
		sn := binary.BigEndian.Uint32(data[1:5])
		ack := binary.BigEndian.Uint32(data[5:9])
		length := binary.BigEndian.Uint16(data[9:11])

		data = data[ARQ_OVERHEAD:]
		if len(data) < int(length) {
			break
		}

		switch cmd {
		case CMD_DATA:
			arq.handleData(sn, data[:length])
			arq.processAck(ack)
		case CMD_NACK:
			// 批量 NACK: sn 是第一个缺失 SN, payload 是后续缺失 SN 列表
			arq.handleBatchNack(sn, data[:length])
			arq.processAck(ack)
		case CMD_ACK:
			arq.processAck(ack)
		case CMD_PUSH:
			// 旁路信道：直接入 push_queue，不经过 rcv_buf/rcv_queue
			pushData := make([]byte, length)
			copy(pushData, data[:length])
			arq.push_queue = append(arq.push_queue, pushData)
			arq.processAck(ack) // 捎带的 ACK 仍然处理
		}

		data = data[length:]
	}
}

// handleData 处理数据包
func (arq *ARQ) handleData(sn uint32, data []byte) {
	// 更新接收到的最高序列号
	if sn > arq.highest_rcv {
		arq.highest_rcv = sn
	}

	// 检查序列号
	if sn < arq.rcv_nxt {
		// 重复包，丢弃
		return
	}

	// 计数：自上次 ACK 后接收的 segment 数量
	arq.rcv_since_ack++

	if sn == arq.rcv_nxt {
		// 按序到达
		arq.rcv_queue = append(arq.rcv_queue, data...)
		arq.rcv_nxt++
		arq.nack_count = 0 // 重置gap计数
		arq.nack_retry = 0 // 重置NACK重试计数
		arq.nack_gap_ts = 0

		// 检查缓冲区中的连续包
		for {
			if seg, exists := arq.rcv_buf[arq.rcv_nxt]; exists {
				arq.rcv_queue = append(arq.rcv_queue, seg.data...)
				delete(arq.rcv_buf, arq.rcv_nxt)
				arq.rcv_nxt++
			} else {
				break
			}
		}
	} else {
		// 乱序到达，存入缓冲区
		seg := Segment{
			sn:   sn,
			data: make([]byte, len(data)),
		}
		copy(seg.data, data)
		arq.rcv_buf[sn] = seg

		// 检查是否需要发送NACK
		arq.checkAndSendNack()
	}

	// 在更新 rcv_nxt / 处理乱序缓存后再发 ACK，确保 ACK 携带最新连续序号。
	// 否则收满一个窗口时会只释放 WND_SIZE-1 个 segment，把一个窗口拆成 127+1 两次发送。
	if arq.rcv_since_ack >= arq.standaloneAckSegments {
		arq.sendStandaloneAck()
		arq.rcv_since_ack = 0
		arq.last_ack_sent = arq.current
	}
}

// processAck 处理捎带 ACK，清理 snd_buf 中已确认的段
func (arq *ARQ) processAck(ack uint32) {
	if ack == 0 {
		return
	}

	// 移除所有 sn < ack 的段，原地压缩避免高频 ACK 带来的分配风暴。
	writeIdx := 0
	for _, seg := range arq.snd_buf {
		if seg.sn >= ack {
			arq.snd_buf[writeIdx] = seg
			writeIdx++
		}
	}
	arq.snd_buf = arq.snd_buf[:writeIdx]

	// 更新 snd_una
	if ack > arq.snd_una {
		arq.snd_una = ack
	}
}

// checkAndSendNack 检查并发送批量 NACK
func (arq *ARQ) checkAndSendNack() {
	// 收集 rcv_nxt 到 highest_rcv 之间所有缺失的 SN
	var missing []uint32
	for sn := arq.rcv_nxt; sn < arq.highest_rcv; sn++ {
		if _, exists := arq.rcv_buf[sn]; !exists {
			missing = append(missing, sn)
		}
	}

	if len(missing) == 0 {
		arq.nack_retry = 0
		arq.nack_gap_ts = 0
		return
	}

	// 记录首次检测到 gap 的时间
	if arq.nack_gap_ts == 0 {
		arq.nack_gap_ts = arq.current
	}

	// 条件1: gap_count >= 阈值 (降为1)
	// 条件2: gap 存在超过 2*RTO (覆盖低频场景)
	gapThresholdMet := len(missing) >= ARQ_NACK_THRESHOLD
	timeThresholdMet := arq.nack_gap_ts > 0 && arq.current-arq.nack_gap_ts > uint32(2*arq.rto)

	if !gapThresholdMet && !timeThresholdMet {
		return
	}

	// 计算NACK重传间隔，使用指数退避
	nack_interval := uint32(100 * (1 << min(arq.nack_retry, 5))) // 最大3.2秒

	// 检查是否需要重传NACK
	if arq.current-arq.nack_sent > nack_interval {
		arq.sendBatchNack(missing)
		arq.nack_sent = arq.current
		arq.nack_retry++
	}
}

// min 辅助函数
func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}

// handleBatchNack 处理批量 NACK 包
func (arq *ARQ) handleBatchNack(firstSN uint32, data []byte) {
	// 重传第一个缺失 SN
	arq.retransmitSN(firstSN)

	// 解析 payload 中的后续缺失 SN
	for len(data) >= 4 {
		sn := binary.BigEndian.Uint32(data[:4])
		arq.retransmitSN(sn)
		data = data[4:]
	}
}

// retransmitSN 在 snd_buf 中查找并重传指定 SN
func (arq *ARQ) retransmitSN(sn uint32) {
	for i := range arq.snd_buf {
		if arq.snd_buf[i].sn == sn {
			arq.sendSegment(&arq.snd_buf[i])
			break
		}
	}
}

// sendBatchNack 发送批量 NACK 包
func (arq *ARQ) sendBatchNack(missing []uint32) {
	if len(missing) == 0 {
		return
	}

	// NACK 包: sn = missing[0], payload = 后续缺失 SN 列表 (每个 4 字节)
	payloadLen := (len(missing) - 1) * 4
	buf := make([]byte, ARQ_OVERHEAD+payloadLen)
	buf[0] = CMD_NACK
	binary.BigEndian.PutUint32(buf[1:5], missing[0])
	binary.BigEndian.PutUint32(buf[5:9], arq.rcv_nxt) // 捎带 ACK
	binary.BigEndian.PutUint16(buf[9:11], uint16(payloadLen))

	// 写入后续缺失 SN
	offset := ARQ_OVERHEAD
	for i := 1; i < len(missing); i++ {
		binary.BigEndian.PutUint32(buf[offset:offset+4], missing[i])
		offset += 4
	}

	arq.output(buf)
}

// Update 更新状态
func (arq *ARQ) Update() {
	arq.mu.Lock()
	defer arq.mu.Unlock()
	if arq.err != nil {
		return
	}

	arq.current = currentMs()
	arq.flush()
}

// flush 刷新数据
func (arq *ARQ) flush() {
	// 如果 snd_queue 为空但有 pending 碎片，推入队列（避免尾部数据无限等待）
	if len(arq.snd_queue) == 0 && len(arq.snd_pending) > 0 {
		arq.flushPendingLocked()
	}
	// 移动队列中的包到发送缓冲区（首次发送）
	for len(arq.snd_queue) > 0 && len(arq.snd_buf) < arq.wnd_size {
		seg := arq.snd_queue[0]
		arq.snd_queue = arq.snd_queue[1:]
		arq.snd_bytes -= len(seg.data)

		seg.ts = arq.current
		arq.sendSegment(&seg)
		arq.snd_buf = append(arq.snd_buf, seg)
	}

	// 超时重传：snd_buf 中等待超过 RTO 未收到 ACK 的 segment 重发。
	// 这覆盖了 SimplexClient Send 失败（cloud storage POST 超时）导致的整批丢失。
	rto := uint32(arq.rto)
	for i := range arq.snd_buf {
		seg := &arq.snd_buf[i]
		if arq.current-seg.ts >= rto {
			if seg.retransmissions >= arq.maxRetransmissions {
				arq.fail(&DeliveryFailureError{
					SN:                 seg.sn,
					Retransmissions:    seg.retransmissions,
					MaxRetransmissions: arq.maxRetransmissions,
				})
				return
			}
			seg.retransmissions++
			seg.ts = arq.current // 重置最近一次发送时间戳
			arq.sendSegment(seg)
		}
	}

	// 定时兜底 ACK：当没有自己的数据要发、且计数触发也没机会时（空闲场景），
	// 使用 rto 作为兜底间隔（基于 transport 的 polling interval 动态配置）。
	if len(arq.snd_queue) == 0 {
		if arq.current-arq.last_ack_sent >= uint32(arq.rto) {
			arq.sendStandaloneAck()
			arq.rcv_since_ack = 0
			arq.last_ack_sent = arq.current
		}
	}
}

// sendStandaloneAck 发送独立 ACK 包（无数据负载），
// 用于在接收方无数据要发时主动确认已收到的 SN。
func (arq *ARQ) sendStandaloneAck() {
	buf := make([]byte, ARQ_OVERHEAD)
	buf[0] = CMD_ACK
	binary.BigEndian.PutUint32(buf[1:5], 0)           // sn=0 (无意义)
	binary.BigEndian.PutUint32(buf[5:9], arq.rcv_nxt) // ACK: 已收到的最新连续 SN
	binary.BigEndian.PutUint16(buf[9:11], 0)          // len=0 (无数据)
	arq.output(buf)
}

func (arq *ARQ) queuePushLocked(data []byte) error {
	if arq.err != nil {
		return arq.err
	}

	buf := make([]byte, ARQ_OVERHEAD+len(data))
	buf[0] = CMD_PUSH
	binary.BigEndian.PutUint32(buf[1:5], 0)           // sn=0 (无序号)
	binary.BigEndian.PutUint32(buf[5:9], arq.rcv_nxt) // 捎带 ACK
	binary.BigEndian.PutUint16(buf[9:11], uint16(len(data)))
	copy(buf[ARQ_OVERHEAD:], data)
	arq.output(buf)
	return nil
}

// QueuePush 通过旁路信道发送优先数据（CMD_PUSH）。
// 不经过 snd_queue/snd_buf，直接发送 — 不受 backpressure 和窗口限制。
// 捎带 ACK 帮助释放发送方窗口。不可靠（无重传），适用于 keepalive ping/pong。
func (arq *ARQ) QueuePush(data []byte) error {
	arq.mu.Lock()
	defer arq.mu.Unlock()
	return arq.queuePushLocked(data)
}

// SendPush 保持兼容：忽略 error 的旧调用仍可工作。
func (arq *ARQ) SendPush(data []byte) {
	_ = arq.QueuePush(data)
}

// RecvPush 接收旁路信道的数据（CMD_PUSH）。
// 返回最早一条 push 数据；无数据时返回 nil。
func (arq *ARQ) RecvPush() []byte {
	arq.mu.Lock()
	defer arq.mu.Unlock()

	if len(arq.push_queue) == 0 {
		return nil
	}
	data := arq.push_queue[0]
	arq.push_queue = arq.push_queue[1:]
	return data
}

// HasPush 返回 push_queue 是否有待读取数据。
func (arq *ARQ) HasPush() bool {
	arq.mu.Lock()
	defer arq.mu.Unlock()
	return len(arq.push_queue) > 0
}

// sendSegment 发送数据段 (11 字节头)
func (arq *ARQ) sendSegment(seg *Segment) {
	buf := make([]byte, ARQ_OVERHEAD+len(seg.data))
	buf[0] = seg.cmd
	binary.BigEndian.PutUint32(buf[1:5], seg.sn)
	binary.BigEndian.PutUint32(buf[5:9], arq.rcv_nxt) // 捎带 ACK
	binary.BigEndian.PutUint16(buf[9:11], seg.len)
	copy(buf[ARQ_OVERHEAD:], seg.data)
	arq.output(buf)
}

// WaitSnd 返回等待发送的包数量
func (arq *ARQ) WaitSnd() int {
	arq.mu.Lock()
	defer arq.mu.Unlock()
	return len(arq.snd_queue) + len(arq.snd_buf)
}

// WaitBuf 返回 in-flight 的包数量（仅 snd_buf，不含 snd_queue）。
// 用于 backpressure：只在网络 in-flight 满时阻塞，内存排队不限制。
func (arq *ARQ) WaitBuf() int {
	arq.mu.Lock()
	defer arq.mu.Unlock()
	return len(arq.snd_buf)
}

// Err returns the fatal delivery error, if the session has lost its reliability guarantee.
func (arq *ARQ) Err() error {
	arq.mu.Lock()
	defer arq.mu.Unlock()
	return arq.err
}

func (arq *ARQ) fail(err error) {
	if arq.err == nil {
		arq.err = err
	}
}

// WaitRcv 返回乱序接收缓冲区的包数量
func (arq *ARQ) WaitRcv() int {
	arq.mu.Lock()
	defer arq.mu.Unlock()
	return len(arq.rcv_buf)
}

// HasTimeout 返回是否设置了超时
func (arq *ARQ) HasTimeout() bool {
	return arq.timeout > 0
}

// GetTimeout 返回配置的超时时间（毫秒）
func (arq *ARQ) GetTimeout() int {
	return arq.timeout
}
