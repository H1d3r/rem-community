//go:build !tinygo

package agent

import (
	"io"
	"math/rand"
	"net"
	"sync"
	"testing"
	"time"

	"github.com/chainreactors/rem/protocol/cio"
	"github.com/chainreactors/rem/protocol/core"
	"github.com/chainreactors/rem/protocol/message"
)

// --- ConnHub tests ---

func newTestManagedConn(id string) *managedConn {
	m := &managedConn{id: id, label: id}
	m.healthy.Store(true)
	return m
}

func newTestConnHub(algo string, ids ...string) *ConnHub {
	h := NewConnHub(algo)
	h.conns = map[string]*managedConn{}
	for _, id := range ids {
		h.conns[id] = newTestManagedConn(id)
	}
	if len(ids) > 0 {
		h.preferredID = ids[0]
	}
	return h
}

func TestConnHubPickRoundRobin(t *testing.T) {
	h := newTestConnHub(ConnBalanceRoundRobin, "a", "b", "c")
	got := make([]string, 0, 6)
	for i := 0; i < 6; i++ {
		c := h.pick(nil)
		if c == nil {
			t.Fatalf("pick returned nil")
		}
		got = append(got, c.id)
	}
	want := []string{"a", "b", "c", "a", "b", "c"}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("round-robin pick mismatch at %d: got=%v want=%v", i, got, want)
		}
	}
}

func TestConnHubPickFallback(t *testing.T) {
	h := newTestConnHub(ConnBalanceFallback, "a", "b", "c")
	for i := 0; i < 5; i++ {
		c := h.pick(nil)
		if c == nil || c.id != "a" {
			t.Fatalf("fallback should choose preferred a, got=%v", c)
		}
	}

	h.conns["a"].healthy.Store(false)
	c := h.pick(nil)
	if c == nil {
		t.Fatalf("fallback after preferred unhealthy should still pick conn")
	}
	if c.id == "a" {
		t.Fatalf("fallback should degrade away from unhealthy preferred conn")
	}
}

func TestConnHubPickRandom(t *testing.T) {
	h := newTestConnHub(ConnBalanceRandom, "a", "b", "c")
	h.rng = rand.New(rand.NewSource(7))

	seen := map[string]int{}
	for i := 0; i < 128; i++ {
		c := h.pick(nil)
		if c == nil {
			t.Fatalf("random pick returned nil")
		}
		seen[c.id]++
	}
	for _, id := range []string{"a", "b", "c"} {
		if seen[id] == 0 {
			t.Fatalf("random pick never selected %s: %+v", id, seen)
		}
	}
}

func TestConnHubReleaseBridge(t *testing.T) {
	h := newTestConnHub(ConnBalanceFallback, "a")
	h.bridgeRoute.Store(uint64(10), "a")
	h.conns["a"].active.Store(3)

	h.ReleaseBridge(10)
	if h.conns["a"].active.Load() != 2 {
		t.Fatalf("ReleaseBridge should decrement active, got=%d", h.conns["a"].active.Load())
	}
}

func TestConnHubAggregateTrafficSnapshot(t *testing.T) {
	h := newTestConnHub(ConnBalanceFallback, "a", "b")

	statsA := cio.NewConnStats()
	statsA.RecordRead(128)
	statsA.RecordWrite(64)
	h.conns["a"].stats = statsA

	statsB := cio.NewConnStats()
	statsB.RecordRead(32)
	statsB.RecordWrite(256)
	h.conns["b"].stats = statsB

	snap := h.AggregateTrafficSnapshot()
	if snap.BytesIn != 160 || snap.BytesOut != 320 {
		t.Fatalf("unexpected aggregate snapshot: %+v", snap)
	}
	if snap.RateInBps != 160 || snap.RateOutBps != 320 {
		t.Fatalf("unexpected aggregate rates: %+v", snap)
	}
}

func TestConnHubAggregateTrafficSnapshotWithLiveConn(t *testing.T) {
	h := newTestConnHub(ConnBalanceFallback, "live")

	local, peer := net.Pipe()
	defer local.Close()
	defer peer.Close()

	wrapped := cio.WrapStatsConn(local)
	h.conns["live"].conn = wrapped
	h.conns["live"].stats = wrapped.Stats()

	peerRead := make(chan struct{})
	go func() {
		defer close(peerRead)
		buf := make([]byte, 5)
		_, _ = io.ReadFull(peer, buf)
	}()
	if _, err := wrapped.Write([]byte("hello")); err != nil {
		t.Fatalf("write outbound traffic: %v", err)
	}
	<-peerRead

	go func() {
		_, _ = peer.Write([]byte("world!"))
	}()
	buf := make([]byte, 6)
	if _, err := io.ReadFull(wrapped, buf); err != nil {
		t.Fatalf("read inbound traffic: %v", err)
	}

	snap := h.AggregateTrafficSnapshot()
	if snap.BytesIn != 6 || snap.BytesOut != 5 {
		t.Fatalf("unexpected aggregate snapshot from live conn: %+v", snap)
	}
	if snap.RateInBps != 6 || snap.RateOutBps != 5 {
		t.Fatalf("unexpected aggregate rates from live conn: %+v", snap)
	}
	if snap.LastActive.IsZero() {
		t.Fatalf("expected LastActive to be set: %+v", snap)
	}
}

func TestAgentTrafficSnapshotUsesConnHubAndForkedAgentDoesNotDuplicate(t *testing.T) {
	parent := &Agent{
		startTime: time.Now(),
		connHub:   newTestConnHub(ConnBalanceFallback, "a"),
	}
	parent.connHub.conns["a"].stats = cio.NewConnStats()
	parent.connHub.conns["a"].stats.RecordRead(90)
	parent.connHub.conns["a"].stats.RecordWrite(45)

	child := &Agent{
		startTime: time.Now(),
		parent:    parent,
		connHub:   parent.connHub,
	}

	parentSnap := parent.TrafficSnapshot()
	if parentSnap.BytesIn != 90 || parentSnap.BytesOut != 45 {
		t.Fatalf("unexpected parent snapshot: %+v", parentSnap)
	}

	childSnap := child.TrafficSnapshot()
	if childSnap.BytesIn != 0 || childSnap.BytesOut != 0 {
		t.Fatalf("forked agent should not duplicate shared transport stats: %+v", childSnap)
	}
}

// --- Redirect routing tests ---

func resetAgentsState() {
	Agents.Map = &sync.Map{}
}

func newTestAgent(t *testing.T, id, typ, redirect string) *Agent {
	t.Helper()
	a, err := NewAgent(&Config{Alias: id, Type: typ, Redirect: redirect})
	if err != nil {
		t.Fatalf("NewAgent(%s): %v", id, err)
	}
	return a
}

func TestRouteControl_ForwardsByDestinationAlias(t *testing.T) {
	resetAgentsState()

	src := newTestAgent(t, "src", core.SERVER, "")

	dst := newTestAgent(t, "dst", core.SERVER, "")
	writer, reader := net.Pipe()
	defer writer.Close()
	defer reader.Close()
	dst.connHub = NewConnHub("")
	controlConn := &managedConn{
		id:      "redirect-ctrl",
		label:   "redirect-ctrl",
		control: writer,
	}
	controlConn.healthy.Store(true)
	dst.connHub.conns["redirect-ctrl"] = controlConn
	Agents.Add(dst)

	done := make(chan *message.Control, 1)
	go func() {
		msg, err := cio.ReadAndAssertMsg(reader, message.ControlMsg)
		if err != nil {
			done <- nil
			return
		}
		done <- msg.(*message.Control)
	}()

	ctrl := &message.Control{Source: "src", Destination: "dst"}
	if !src.routeControl(ctrl) {
		t.Fatal("expected routeControl to route")
	}
	if src.Type != core.Redirect {
		t.Fatalf("expected source type to switch to redirect, got %s", src.Type)
	}

	select {
	case got := <-done:
		if got == nil {
			t.Fatal("failed to read forwarded control")
		}
		if got.Source != "src" || got.Destination != "dst" {
			t.Fatalf("unexpected forwarded control: source=%q destination=%q", got.Source, got.Destination)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for forwarded control")
	}
}

func TestRouteControl_DestinationAliasMatchesSelf(t *testing.T) {
	resetAgentsState()

	a := newTestAgent(t, "internal", core.SERVER, "")
	ctrl := &message.Control{Source: "client-b", Destination: "internal"}
	if a.routeControl(ctrl) {
		t.Fatal("expected routeControl not to route when destination matches alias")
	}
	if a.Type != core.SERVER {
		t.Fatalf("expected type to remain server, got %s", a.Type)
	}
}
