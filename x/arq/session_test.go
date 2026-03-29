package arq

import (
	"errors"
	"io"
	"net"
	"runtime"
	"sync/atomic"
	"testing"
	"time"

	harnessarq "github.com/chainreactors/rem/harness/arq"

	"golang.org/x/net/nettest"
)

// Shared helpers are in testutil_test.go:
// mockAddr, mockPacketConn, controlledPacketConn, makeARQDataPacket, acceptWithTimeout

// ========================== Regression Tests ==========================
// These tests assert CORRECT behavior. They should FAIL with the current
// buggy code and PASS after the fix is applied.

// TestListenerCleansUpClosedSessions verifies that when ARQSession.Close()
// is called, the session is removed from listener.sessions map.
//
// ROOT CAUSE: ARQSession.Close() does not notify the listener. The chClosed
// channel is defined in ARQListener but never written to.
func TestListenerCleansUpClosedSessions(t *testing.T) {
	conn := newMockPacketConn()
	listener, err := ServeConn(conn, 1400, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()

	addr := &mockAddr{"client-1"}
	conn.Inject(makeARQDataPacket(0, []byte("hello")), addr)

	sess := acceptWithTimeout(t, listener, 2*time.Second)

	// Verify session exists
	listener.sessionLock.RLock()
	_, exists := listener.sessions[addr.String()]
	listener.sessionLock.RUnlock()
	if !exists {
		t.Fatal("precondition: session should exist in listener.sessions after Accept")
	}

	// Close session (simulates yamux death)
	sess.Close()
	time.Sleep(200 * time.Millisecond)

	// EXPECTED: closed session should be removed from the map
	listener.sessionLock.RLock()
	_, stillExists := listener.sessions[addr.String()]
	listener.sessionLock.RUnlock()
	if stillExists {
		t.Fatal("closed session still in listener.sessions — cleanup not implemented")
	}
}

// TestBackgroundLoopDoesNotStealData verifies that when a session is managed
// by a listener, its backgroundLoop does NOT call ReadFrom on the shared conn.
// Only the listener's monitor should read from the shared conn.
//
// ROOT CAUSE: backgroundLoop calls s.conn.ReadFrom() which competes with the
// monitor. It reads data for other addresses and discards it (data loss).
//
// This test injects data for a DIFFERENT address and verifies it is not consumed
// by the existing session's backgroundLoop. With the fix, managed sessions skip
// ReadFrom in backgroundLoop, and the data stays available for the monitor.
func TestBackgroundLoopDoesNotStealData(t *testing.T) {
	conn := newMockPacketConn()
	addrA := &mockAddr{"client-A"}
	addrB := &mockAddr{"client-B"}

	listener, err := ServeConn(conn, 1400, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()

	// Create session A via listener
	conn.Inject(makeARQDataPacket(0, []byte("init-A")), addrA)
	acceptWithTimeout(t, listener, 2*time.Second)

	// Give session A's backgroundLoop time to start consuming
	time.Sleep(100 * time.Millisecond)

	// Inject data for addrB — the monitor should handle this, NOT session A
	for sn := uint32(0); sn < 5; sn++ {
		conn.Inject(makeARQDataPacket(sn, []byte("B")), addrB)
	}

	// Monitor should create session B for this new address
	sessB := acceptWithTimeout(t, listener, 3*time.Second)

	// Session B should have received at least the first packet's data
	time.Sleep(500 * time.Millisecond)
	buf := make([]byte, 4096)
	sessB.SetReadDeadline(time.Now().Add(1 * time.Second))
	n, err := sessB.Read(buf)
	if n == 0 {
		t.Fatalf("session B received no data (err=%v) — data was likely stolen by session A's backgroundLoop", err)
	}
	t.Logf("session B received %d bytes — no data stealing", n)

	sessB.Close()
}

func TestSessionFailsWhenRetransmissionBudgetExceeded(t *testing.T) {
	conn := newMockPacketConn()
	addr := &mockAddr{"client-fail"}
	sess := NewARQSessionWithConfig(conn, addr, ARQConfig{
		MTU:                ARQ_MTU,
		RTO:                20,
		MaxRetransmissions: 2,
	})
	defer sess.Close()

	if _, err := sess.Write([]byte("lost")); err != nil {
		t.Fatalf("initial write failed: %v", err)
	}

	deadline := time.Now().Add(500 * time.Millisecond)
	for time.Now().Before(deadline) {
		if atomic.LoadInt32(&sess.closed) != 0 {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if atomic.LoadInt32(&sess.closed) == 0 {
		t.Fatal("session did not close after retransmission budget exhaustion")
	}

	var deliveryErr *DeliveryFailureError

	buf := make([]byte, 16)
	_, err := sess.Read(buf)
	if !errors.As(err, &deliveryErr) {
		t.Fatalf("Read error = %v, want DeliveryFailureError", err)
	}

	_, err = sess.Write([]byte("again"))
	if !errors.As(err, &deliveryErr) {
		t.Fatalf("Write error = %v, want DeliveryFailureError", err)
	}
}

func TestSessionWriteBackpressureTimeout(t *testing.T) {
	const mtu = 32
	conn := newControlledPacketConn(mtu - ARQ_OVERHEAD)
	conn.blocked.Store(1)
	addr := &mockAddr{"client-backpressure-timeout"}
	sess := NewARQSessionWithConfig(conn, addr, ARQConfig{
		MTU:                mtu,
		RTO:                50,
		MaxRetransmissions: 2,
	})
	defer sess.Close()

	sess.SetWriteDeadline(time.Now().Add(120 * time.Millisecond))

	start := time.Now()
	n, err := sess.Write([]byte("abcdefghijklmnopqrstuvwxyz0123456789"))
	if err == nil {
		t.Fatal("Write should time out while transport credit is exhausted")
	}
	if !isTimeoutErr(err) {
		t.Fatalf("Write error = %v, want timeout", err)
	}
	if n == 0 {
		t.Fatalf("Write should report partial progress before timing out, got n=%d", n)
	}
	if elapsed := time.Since(start); elapsed < 100*time.Millisecond {
		t.Fatalf("Write timed out too early: %v", elapsed)
	}
}

func TestSessionWriteBackpressureResumesAfterCreditReturns(t *testing.T) {
	const mtu = 32
	conn := newControlledPacketConn(mtu - ARQ_OVERHEAD)
	conn.blocked.Store(1)
	addr := &mockAddr{"client-backpressure-resume"}
	sess := NewARQSessionWithConfig(conn, addr, ARQConfig{
		MTU:                mtu,
		RTO:                50,
		MaxRetransmissions: 2,
	})
	defer sess.Close()

	writeDone := make(chan struct {
		n   int
		err error
	}, 1)
	payload := []byte("abcdefghijklmnopqrstuvwxyz0123456789")
	go func() {
		n, err := sess.Write(payload)
		writeDone <- struct {
			n   int
			err error
		}{n: n, err: err}
	}()

	select {
	case <-conn.tryCh:
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for blocked transport write attempt")
	}

	conn.blocked.Store(0)

	select {
	case result := <-writeDone:
		if result.err != nil {
			t.Fatalf("Write resumed with error: %v", result.err)
		}
		if result.n != len(payload) {
			t.Fatalf("Write resumed with n=%d, want %d", result.n, len(payload))
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Write did not resume after transport credit returned")
	}
}

func TestCloseWithPendingData_ReturnsCloseDrainTimeout(t *testing.T) {
	const interval = 25 * time.Millisecond

	conn := newMockPacketConn()
	addr := &intervalMockAddr{
		mockAddr: mockAddr{id: "client-close-timeout"},
		interval: interval,
	}
	cfg := ARQConfig{
		MTU:                ARQ_MTU,
		RTO:                20,
		MaxRetransmissions: 100,
	}
	sess := NewARQSessionWithConfig(conn, addr, cfg)
	defer sess.Close()

	if _, err := sess.Write(make([]byte, 4*1024)); err != nil {
		t.Fatalf("initial write failed: %v", err)
	}
	if sess.arq.WaitSnd() == 0 {
		t.Fatal("precondition failed: expected pending data before Close")
	}

	start := time.Now()
	if err := sess.Close(); !errors.Is(err, ErrCloseDrainTimeout) {
		t.Fatalf("Close error = %v, want %v", err, ErrCloseDrainTimeout)
	}

	select {
	case <-sess.finalDone:
	case <-time.After(2 * time.Second):
		t.Fatal("session did not finalize after bounded close drain timeout")
	}

	elapsed := time.Since(start)
	want := closeDrainTimeoutForConfig(cfg)
	const jitter = 75 * time.Millisecond
	if elapsed+jitter < want {
		t.Fatalf("session finalized too early: elapsed=%v want_at_least=%v", elapsed, want)
	}
	if elapsed > want+300*time.Millisecond {
		t.Fatalf("session finalized too late: elapsed=%v want_about=%v", elapsed, want)
	}

	if sess.sessionErr() != nil {
		t.Fatalf("sessionErr = %v, want nil for local Close()", sess.sessionErr())
	}
	if atomic.LoadInt32(&sess.finalized) == 0 {
		t.Fatal("finalized flag was not set")
	}
	if atomic.LoadInt32(&conn.closed) == 0 {
		t.Fatal("underlying conn was not closed on forced finalization")
	}

	buf := make([]byte, 16)
	if _, err := sess.Read(buf); !errors.Is(err, io.EOF) {
		t.Fatalf("Read after forced close = %v, want %v", err, io.EOF)
	}
	if _, err := sess.Write([]byte("again")); !errors.Is(err, io.ErrClosedPipe) {
		t.Fatalf("Write after forced close = %v, want %v", err, io.ErrClosedPipe)
	}
}

func TestCloseWithPendingData_ReclaimsSessionGoroutines(t *testing.T) {
	const (
		interval   = 20 * time.Millisecond
		iterations = 12
	)

	baseline := runtime.NumGoroutine()
	sessions := make([]*ARQSession, 0, iterations)

	for i := 0; i < iterations; i++ {
		conn := newMockPacketConn()
		addr := &intervalMockAddr{
			mockAddr: mockAddr{id: "client-gc"},
			interval: interval,
		}
		sess := NewARQSessionWithConfig(conn, addr, ARQConfig{
			MTU:                ARQ_MTU,
			RTO:                20,
			MaxRetransmissions: 100,
		})
		if _, err := sess.Write(make([]byte, 2*1024)); err != nil {
			t.Fatalf("session %d initial write failed: %v", i, err)
		}
		if err := sess.Close(); !errors.Is(err, ErrCloseDrainTimeout) {
			t.Fatalf("session %d Close error = %v, want %v", i, err, ErrCloseDrainTimeout)
		}
		sessions = append(sessions, sess)
	}

	deadline := time.After(5 * time.Second)
	for i, sess := range sessions {
		select {
		case <-sess.finalDone:
		case <-deadline:
			t.Fatalf("session %d did not finalize", i)
		}
	}

	time.Sleep(200 * time.Millisecond)

	final := runtime.NumGoroutine()
	if leaked := final - baseline; leaked > 8 {
		t.Fatalf("possible session goroutine leak after forced close: baseline=%d final=%d leaked=%d",
			baseline, final, leaked)
	}
}

func TestCloseDrainTimeout_IsDerivedFromRTOBudget(t *testing.T) {
	conn := newMockPacketConn()
	addr := &intervalMockAddr{
		mockAddr: mockAddr{id: "client-default-close-grace"},
		interval: 500 * time.Millisecond,
	}
	cfg := ARQConfig{
		MTU:                ARQ_MTU,
		RTO:                20,
		MaxRetransmissions: 1,
	}
	sess := NewARQSessionWithConfig(conn, addr, cfg)
	defer sess.Close()

	if _, err := sess.Write(make([]byte, 1024)); err != nil {
		t.Fatalf("initial write failed: %v", err)
	}

	start := time.Now()
	if err := sess.Close(); !errors.Is(err, ErrCloseDrainTimeout) {
		t.Fatalf("Close error = %v, want %v", err, ErrCloseDrainTimeout)
	}

	select {
	case <-sess.finalDone:
	case <-time.After(500 * time.Millisecond):
		t.Fatal("session did not finalize with derived close drain timeout")
	}

	elapsed := time.Since(start)
	want := closeDrainTimeoutForConfig(cfg)
	if elapsed+25*time.Millisecond < want || elapsed > want+150*time.Millisecond {
		t.Fatalf("close drain timeout mismatch: elapsed=%v want_about=%v", elapsed, want)
	}
	if sess.sessionErr() != nil {
		t.Fatalf("sessionErr = %v, want nil for local Close()", sess.sessionErr())
	}
}

func TestListenerDisconnectAbortsSession(t *testing.T) {
	conn := newMockPacketConn()
	listener, err := ServeConn(conn, 1400, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()

	addr := &mockAddr{"client-disconnect"}
	conn.Inject(makeARQDataPacket(0, []byte("hello")), addr)

	sess := acceptWithTimeout(t, listener, 2*time.Second)
	buf := make([]byte, 16)
	if _, err := sess.Read(buf); err != nil {
		t.Fatalf("precondition read failed: %v", err)
	}

	conn.InjectError(io.ErrClosedPipe, addr)

	deadline := time.Now().Add(500 * time.Millisecond)
	for time.Now().Before(deadline) {
		if atomic.LoadInt32(&sess.(*ARQSession).finalized) != 0 {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if atomic.LoadInt32(&sess.(*ARQSession).finalized) == 0 {
		t.Fatal("session did not finalize after transport disconnect")
	}

	if _, err := sess.Read(buf); !errors.Is(err, io.ErrClosedPipe) {
		t.Fatalf("Read after transport disconnect = %v, want %v", err, io.ErrClosedPipe)
	}
	if _, err := sess.Write([]byte("again")); !errors.Is(err, io.ErrClosedPipe) {
		t.Fatalf("Write after transport disconnect = %v, want %v", err, io.ErrClosedPipe)
	}

	listener.sessionLock.RLock()
	_, exists := listener.sessions[addr.String()]
	listener.sessionLock.RUnlock()
	if exists {
		t.Fatal("listener still retains disconnected session")
	}
}

// ========================== Behavioral Tests ==========================
// These tests document the current session behavior under edge conditions.

// TestReadBufferDoesNotBlockBackgroundLoop verifies that the session-sized
// readBuffer absorbs moderate bursts so backgroundLoop can keep draining conn.
func TestReadBufferDoesNotBlockBackgroundLoop(t *testing.T) {
	conn := newMockPacketConn()
	addr := &mockAddr{"client-1"}
	mtu := 100

	sess := NewARQSession(conn, addr, mtu, 0)
	defer sess.Close()

	// With the current sizing strategy, readBuffer = MTU * ARQ_WND_SIZE.
	// A small burst like this should be absorbed without stalling reads.
	for sn := uint32(0); sn < 5; sn++ {
		conn.Inject(makeARQDataPacket(sn, make([]byte, 50)), addr)
	}
	time.Sleep(500 * time.Millisecond)

	// Canary: if backgroundLoop is draining correctly, this should also be consumed.
	conn.Inject(makeARQDataPacket(5, []byte("canary")), addr)
	time.Sleep(500 * time.Millisecond)

	pending := conn.PendingCount()
	if pending != 0 {
		t.Fatalf("backgroundLoop left %d packet(s) pending, readBuffer %d/%d",
			pending, sess.readBuffer.Size(), sess.readBuffer.Cap())
	}

	// ReadFrom should continue advancing while backgroundLoop drains the conn.
	rc1 := atomic.LoadInt64(&conn.readCount)
	time.Sleep(200 * time.Millisecond)
	rc2 := atomic.LoadInt64(&conn.readCount)
	if rc2 <= rc1 {
		t.Fatalf("backgroundLoop stopped calling ReadFrom: before=%d after=%d", rc1, rc2)
	}
}

// TestMonitorDrainsWhenBackgroundLoopBlocked confirms that the listener's
// monitor continues to drain the shared conn even when a session's
// backgroundLoop is blocked. This proves SimplexServer's buffer won't fill up.
func TestMonitorDrainsWhenBackgroundLoopBlocked(t *testing.T) {
	conn := newMockPacketConn()
	addr := &mockAddr{"client-1"}
	mtu := 100

	listener, err := ServeConn(conn, mtu, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()

	conn.Inject(makeARQDataPacket(0, make([]byte, 50)), addr)
	sess := acceptWithTimeout(t, listener, 2*time.Second)

	// Fill readBuffer to block backgroundLoop
	for sn := uint32(1); sn <= 5; sn++ {
		conn.Inject(makeARQDataPacket(sn, make([]byte, 50)), addr)
	}
	time.Sleep(500 * time.Millisecond)

	// Inject more data — monitor should drain it
	for sn := uint32(6); sn <= 10; sn++ {
		conn.Inject(makeARQDataPacket(sn, make([]byte, 50)), addr)
	}
	time.Sleep(500 * time.Millisecond)

	pending := conn.PendingCount()
	if pending == 0 {
		t.Log("CONFIRMED: monitor drains conn — SimplexServer buffer will NOT fill up")
	} else {
		t.Errorf("conn still has %d unread packets — monitor not draining", pending)
	}

	arqSession := sess.(*ARQSession)
	arqSession.arq.mu.Lock()
	queueLen := len(arqSession.arq.rcv_queue)
	arqSession.arq.mu.Unlock()
	if queueLen > 0 {
		t.Logf("arq.rcv_queue has %d bytes (memory leak from unread data)", queueLen)
	}

	sess.Close()
}

// ---------------------------------------------------------------------------
// golang.org/x/net/nettest conformance test suite
// ---------------------------------------------------------------------------

func TestARQSession_NetConn(t *testing.T) {
	nettest.TestConn(t, arqMakePipe)
}

// ---------------------------------------------------------------------------
// harness/arq conformance test suite
// ---------------------------------------------------------------------------

func TestARQSession_TransferSuite(t *testing.T) {
	harnessarq.TestTransfer(t, func(t *testing.T) (c1, c2 net.Conn, stop func(), err error) {
		return arqMakePipe()
	})
}

func TestARQSession_StreamSemanticsSuite(t *testing.T) {
	harnessarq.TestStreamSemantics(t, func(t *testing.T) (c1, c2 net.Conn, stop func(), err error) {
		return arqMakePipe()
	})
}

func TestARQSession_ConcurrencySuite(t *testing.T) {
	harnessarq.TestConcurrency(t, func(t *testing.T) (c1, c2 net.Conn, stop func(), err error) {
		return arqMakePipe()
	})
}
