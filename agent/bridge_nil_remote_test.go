//go:build !tinygo

package agent

import (
	"context"
	"fmt"
	"io"
	"net"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/chainreactors/rem/protocol/core"
	"github.com/chainreactors/rem/x/utils"
)

// bridgeTestInbound is a minimal Inbound for testing.
type bridgeTestInbound struct {
	relayCalled atomic.Bool
	relayConn   net.Conn // captured conn argument
	mu          sync.Mutex
}

func (i *bridgeTestInbound) Name() string { return "test-inbound" }

func (i *bridgeTestInbound) Relay(conn net.Conn, bridge io.ReadWriteCloser) (net.Conn, error) {
	i.mu.Lock()
	i.relayConn = conn
	i.mu.Unlock()
	i.relayCalled.Store(true)
	if conn == nil {
		return nil, fmt.Errorf("conn is nil — this is the issue #9 bug")
	}
	return conn, nil
}

func (i *bridgeTestInbound) ToClash() *utils.Proxies { return nil }
func (i *bridgeTestInbound) URL() string             { return "test://0" }
func (i *bridgeTestInbound) String() string           { return "test-inbound" }
func (i *bridgeTestInbound) Listener() net.Listener   { return nil }

// TestIssue9_BridgeHandler_NilRemote reproduces the nil pointer dereference
// from https://github.com/chainreactors/rem/issues/9
//
// When alias-based bridging (-a/-d) is used, handleBridgeOpen creates a Bridge
// with b.remote = nil. Bridge.Handler() then calls Inbound.Relay(nil, stream)
// which panics in the real SOCKS5 code because it reads from a nil net.Conn.
func TestIssue9_BridgeHandler_NilRemote(t *testing.T) {
	streamA, streamB := net.Pipe()
	defer streamA.Close()
	defer streamB.Close()

	consoleURL, _ := core.NewConsoleURL("tcp://127.0.0.1:0")
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	agent := &Agent{
		Config: &Config{
			Alias: "inter",
			Type:  core.SERVER,
			URLs:  &core.URLs{ConsoleURL: consoleURL},
		},
		ID:       "inter",
		ctx:      ctx,
		canceler: cancel,
		log:      utils.NewRingLogWriter(32),
		connHub:  NewConnHub(""),
	}

	inbound := &bridgeTestInbound{}
	outbound := &bridgeTestOutbound{conn: streamB, ready: make(chan struct{})}
	agent.Inbound = inbound
	agent.Outbound = outbound

	// Simulate a bridge created by handleBridgeOpen — b.remote is nil.
	// This is what happens on the receiving end of alias-based routing.
	bridge := &Bridge{
		id:     1,
		remote: nil, // <-- THIS IS THE BUG: nil remote from handleBridgeOpen
		stream: streamA,
		ctx:    ctx,
		cancel: cancel,
		agent:  agent,
	}

	// Before the fix, this panics with nil pointer dereference in Inbound.Relay
	bridge.Handler(agent)

	// Give goroutines time to execute
	time.Sleep(300 * time.Millisecond)

	// The inbound Relay should NOT have been called with nil remote
	if inbound.relayCalled.Load() {
		t.Fatal("Inbound.Relay was called with nil b.remote — issue #9 bug present")
	}

	// Outbound Handle should still work
	select {
	case <-outbound.ready:
		t.Log("OK: Outbound.Handle was called correctly")
	case <-time.After(2 * time.Second):
		t.Fatal("Outbound.Handle was not called within timeout")
	}
}

// TestBridgeHandler_WithRemote verifies that Inbound.Relay is still called
// when b.remote is not nil (normal bridge from accepted connection).
func TestBridgeHandler_WithRemote(t *testing.T) {
	streamA, streamB := net.Pipe()
	remoteA, remoteB := net.Pipe()
	defer streamA.Close()
	defer streamB.Close()
	defer remoteB.Close()

	consoleURL, _ := core.NewConsoleURL("tcp://127.0.0.1:0")
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	agent := &Agent{
		Config: &Config{
			Alias: "test",
			Type:  core.SERVER,
			URLs:  &core.URLs{ConsoleURL: consoleURL},
		},
		ID:       "test",
		ctx:      ctx,
		canceler: cancel,
		log:      utils.NewRingLogWriter(32),
		connHub:  NewConnHub(""),
	}

	inbound := &bridgeTestInbound{}
	agent.Inbound = inbound
	agent.Outbound = nil

	// Normal bridge with b.remote set (from NewBridgeWithConn / Accept)
	bridge := &Bridge{
		id:     2,
		remote: remoteA, // <-- non-nil: normal bridge
		stream: streamA,
		ctx:    ctx,
		cancel: cancel,
		agent:  agent,
	}

	bridge.Handler(agent)
	time.Sleep(300 * time.Millisecond)

	if !inbound.relayCalled.Load() {
		t.Fatal("Inbound.Relay was NOT called when b.remote is valid — regression")
	}
	t.Log("OK: Inbound.Relay called correctly with non-nil remote")
}
