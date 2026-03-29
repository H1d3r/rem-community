package arq

import (
	"encoding/binary"
	"errors"
	"net"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// === Shared test infrastructure for x/arq tests ===

// mockAddr is a minimal net.Addr implementation for testing.
type mockAddr struct{ id string }

func (a *mockAddr) Network() string { return "mock" }
func (a *mockAddr) String() string  { return a.id }

// intervalMockAddr extends mockAddr with an Interval() method,
// used to test interval-aware code paths.
type intervalMockAddr struct {
	mockAddr
	interval time.Duration
}

func (a *intervalMockAddr) Interval() time.Duration { return a.interval }

// packetEntry represents a single packet in the mock conn's queue.
type packetEntry struct {
	data []byte
	addr net.Addr
	err  error
}

// mockPacketConn is an in-memory net.PacketConn for unit testing ARQ sessions.
type mockPacketConn struct {
	mu        sync.Mutex
	incoming  []packetEntry
	outgoing  []packetEntry
	closed    int32
	readCount int64

	disconnectMu sync.Mutex
	onDisconnect func(net.Addr)
}

func newMockPacketConn() *mockPacketConn {
	return &mockPacketConn{}
}

func (m *mockPacketConn) Inject(data []byte, addr net.Addr) {
	d := make([]byte, len(data))
	copy(d, data)
	m.mu.Lock()
	m.incoming = append(m.incoming, packetEntry{data: d, addr: addr})
	m.mu.Unlock()
}

func (m *mockPacketConn) InjectError(err error, addr net.Addr) {
	m.mu.Lock()
	m.incoming = append(m.incoming, packetEntry{addr: addr, err: err})
	m.mu.Unlock()
}

func (m *mockPacketConn) PendingCount() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return len(m.incoming)
}

func (m *mockPacketConn) ReadFrom(p []byte) (int, net.Addr, error) {
	atomic.AddInt64(&m.readCount, 1)
	if atomic.LoadInt32(&m.closed) != 0 {
		return 0, nil, net.ErrClosed
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if len(m.incoming) > 0 {
		entry := m.incoming[0]
		m.incoming = m.incoming[1:]
		if entry.err != nil {
			return 0, entry.addr, entry.err
		}
		n := copy(p, entry.data)
		return n, entry.addr, nil
	}
	return 0, nil, &timeoutError{}
}

func (m *mockPacketConn) WriteTo(p []byte, addr net.Addr) (int, error) {
	d := make([]byte, len(p))
	copy(d, p)
	m.mu.Lock()
	m.outgoing = append(m.outgoing, packetEntry{data: d, addr: addr})
	m.mu.Unlock()
	return len(p), nil
}

func (m *mockPacketConn) Close() error {
	atomic.StoreInt32(&m.closed, 1)
	return nil
}

func (m *mockPacketConn) OnDisconnect(fn func(net.Addr)) {
	m.disconnectMu.Lock()
	m.onDisconnect = fn
	m.disconnectMu.Unlock()
}

func (m *mockPacketConn) TriggerDisconnect(addr net.Addr) {
	m.disconnectMu.Lock()
	fn := m.onDisconnect
	m.disconnectMu.Unlock()
	if fn != nil {
		fn(addr)
	}
}

func (m *mockPacketConn) LocalAddr() net.Addr                { return &mockAddr{"local"} }
func (m *mockPacketConn) SetDeadline(t time.Time) error      { return nil }
func (m *mockPacketConn) SetReadDeadline(t time.Time) error  { return nil }
func (m *mockPacketConn) SetWriteDeadline(t time.Time) error { return nil }

// controlledPacketConn wraps mockPacketConn with write-budget backpressure.
type controlledPacketConn struct {
	*mockPacketConn
	writeBudget int
	blocked     atomic.Int32
	tryCh       chan struct{}
}

func newControlledPacketConn(writeBudget int) *controlledPacketConn {
	return &controlledPacketConn{
		mockPacketConn: newMockPacketConn(),
		writeBudget:    writeBudget,
		tryCh:          make(chan struct{}, 32),
	}
}

func (c *controlledPacketConn) TryWriteTo(p []byte, addr net.Addr) (int, error) {
	select {
	case c.tryCh <- struct{}{}:
	default:
	}
	if c.blocked.Load() != 0 {
		return 0, ErrWouldBlock
	}
	return c.WriteTo(p, addr)
}

func (c *controlledPacketConn) ARQWriteBudget() int {
	return c.writeBudget
}

// makeARQDataPacket builds a raw ARQ DATA packet with the given SN and payload.
func makeARQDataPacket(sn uint32, payload []byte) []byte {
	buf := make([]byte, ARQ_OVERHEAD+len(payload))
	buf[0] = CMD_DATA
	binary.BigEndian.PutUint32(buf[1:5], sn)
	binary.BigEndian.PutUint32(buf[5:9], 0) // ack = 0
	binary.BigEndian.PutUint16(buf[9:11], uint16(len(payload)))
	copy(buf[ARQ_OVERHEAD:], payload)
	return buf
}

// acceptWithTimeout accepts a session from the listener with a timeout guard.
func acceptWithTimeout(t *testing.T, l *ARQListener, timeout time.Duration) net.Conn {
	t.Helper()
	var sess net.Conn
	done := make(chan struct{})
	go func() {
		sess, _ = l.Accept()
		close(done)
	}()
	select {
	case <-done:
		return sess
	case <-time.After(timeout):
		t.Fatal("Accept timed out")
		return nil
	}
}

// ---------------------------------------------------------------------------
// nettestConn wraps net.Conn to suppress ErrCloseDrainTimeout on Close,
// which is expected for ARQ sessions with pending data.
// ---------------------------------------------------------------------------

type nettestConn struct {
	net.Conn
}

func (c *nettestConn) Close() error {
	err := c.Conn.Close()
	if errors.Is(err, ErrCloseDrainTimeout) {
		return nil
	}
	return err
}

// ---------------------------------------------------------------------------
// pipePacketConn: lossless, in-order, bidirectional PacketConn pair for testing
// ---------------------------------------------------------------------------

type pipePacketConn struct {
	localAddr    net.Addr
	peerAddr     net.Addr
	sendCh       chan []byte
	recvCh       chan []byte
	closed       int32
	readDeadline int64 // UnixNano, 0 = no deadline
	closeCh      chan struct{}
	closeOnce    *sync.Once
	sendMu       sync.RWMutex
	sendClose    *sync.Once
}

func newPacketPipe() (a, b *pipePacketConn) {
	chAB := make(chan []byte, 4096)
	chBA := make(chan []byte, 4096)
	addrA := &mockAddr{"pipe-a"}
	addrB := &mockAddr{"pipe-b"}

	a = &pipePacketConn{
		localAddr: addrA,
		peerAddr:  addrB,
		sendCh:    chAB,
		recvCh:    chBA,
		closeCh:   make(chan struct{}),
		closeOnce: &sync.Once{},
		sendClose: &sync.Once{},
	}
	b = &pipePacketConn{
		localAddr: addrB,
		peerAddr:  addrA,
		sendCh:    chBA,
		recvCh:    chAB,
		closeCh:   make(chan struct{}),
		closeOnce: &sync.Once{},
		sendClose: &sync.Once{},
	}
	return a, b
}

func (p *pipePacketConn) ReadFrom(b []byte) (int, net.Addr, error) {
	if atomic.LoadInt32(&p.closed) != 0 {
		return 0, nil, net.ErrClosed
	}

	deadline := atomic.LoadInt64(&p.readDeadline)
	if deadline > 0 {
		remaining := time.Duration(deadline - time.Now().UnixNano())
		if remaining <= 0 {
			return 0, nil, &timeoutError{}
		}
		timer := time.NewTimer(remaining)
		defer timer.Stop()
		select {
		case data, ok := <-p.recvCh:
			if !ok {
				return 0, nil, net.ErrClosed
			}
			return copy(b, data), p.peerAddr, nil
		case <-timer.C:
			return 0, nil, &timeoutError{}
		case <-p.closeCh:
			return 0, nil, net.ErrClosed
		}
	}

	// No deadline — block until data or close
	select {
	case data, ok := <-p.recvCh:
		if !ok {
			return 0, nil, net.ErrClosed
		}
		return copy(b, data), p.peerAddr, nil
	case <-p.closeCh:
		return 0, nil, net.ErrClosed
	}
}

func (p *pipePacketConn) WriteTo(b []byte, addr net.Addr) (int, error) {
	p.sendMu.RLock()
	defer p.sendMu.RUnlock()
	if atomic.LoadInt32(&p.closed) != 0 {
		return 0, net.ErrClosed
	}
	data := make([]byte, len(b))
	copy(data, b)
	select {
	case p.sendCh <- data:
		return len(b), nil
	case <-time.After(5 * time.Second):
		return 0, net.ErrClosed
	}
}

func (p *pipePacketConn) Close() error {
	if atomic.CompareAndSwapInt32(&p.closed, 0, 1) {
		p.sendMu.Lock()
		defer p.sendMu.Unlock()
		p.closeOnce.Do(func() { close(p.closeCh) })
		p.sendClose.Do(func() { close(p.sendCh) })
	}
	return nil
}

func (p *pipePacketConn) LocalAddr() net.Addr { return p.localAddr }

func (p *pipePacketConn) SetDeadline(t time.Time) error {
	return p.SetReadDeadline(t)
}

func (p *pipePacketConn) SetReadDeadline(t time.Time) error {
	nano := int64(0)
	if !t.IsZero() {
		nano = t.UnixNano()
	}
	atomic.StoreInt64(&p.readDeadline, nano)
	return nil
}

func (p *pipePacketConn) SetWriteDeadline(t time.Time) error {
	return nil
}

// ---------------------------------------------------------------------------
// arqMakePipe: creates two ARQSessions connected via a lossless pipe
// (nettest.MakePipe signature)
// ---------------------------------------------------------------------------

func arqMakePipe() (c1, c2 net.Conn, stop func(), err error) {
	pA, pB := newPacketPipe()
	sessA := &nettestConn{Conn: NewARQSession(pA, pB.localAddr, 1024, 0)}
	sessB := &nettestConn{Conn: NewARQSession(pB, pA.localAddr, 1024, 0)}
	stop = func() {
		sessA.Close()
		sessB.Close()
		// Allow background goroutines to detect close and exit
		// before the test framework invalidates the test's t.
		time.Sleep(50 * time.Millisecond)
	}
	return sessA, sessB, stop, nil
}

