package cio

import (
	"net"
	"testing"
	"time"
)

type fakeClock struct {
	now time.Time
}

func (c *fakeClock) Now() time.Time {
	return c.now
}

func (c *fakeClock) Advance(d time.Duration) {
	c.now = c.now.Add(d)
}

func TestConnStatsSnapshotTracksBytesAndRates(t *testing.T) {
	clock := &fakeClock{now: time.Unix(1700000000, 0)}
	stats := NewConnStatsWithClock(clock.Now)

	stats.RecordRead(400)
	stats.RecordWrite(600)
	snap := stats.Snapshot()
	if snap.BytesIn != 400 || snap.BytesOut != 600 {
		t.Fatalf("unexpected byte totals: %+v", snap)
	}
	if snap.RateInBps != 400 || snap.RateOutBps != 600 {
		t.Fatalf("unexpected rates: %+v", snap)
	}

	clock.Advance(1100 * time.Millisecond)
	snap = stats.Snapshot()
	if snap.RateInBps != 0 || snap.RateOutBps != 0 {
		t.Fatalf("expected rates to decay to zero, got %+v", snap)
	}
	if snap.BytesIn != 400 || snap.BytesOut != 600 {
		t.Fatalf("totals should remain cumulative, got %+v", snap)
	}
}

func TestWrapStatsConnRecordsTraffic(t *testing.T) {
	left, right := net.Pipe()
	defer left.Close()
	defer right.Close()

	wrapped := WrapStatsConn(left)

	readErr := make(chan error, 1)
	go func() {
		buf := make([]byte, 5)
		_, err := right.Read(buf)
		readErr <- err
	}()
	if _, err := wrapped.Write([]byte("hello")); err != nil {
		t.Fatalf("write: %v", err)
	}
	if err := <-readErr; err != nil {
		t.Fatalf("peer read: %v", err)
	}

	go func() {
		_, _ = right.Write([]byte("world"))
	}()
	buf := make([]byte, 5)
	if _, err := wrapped.Read(buf); err != nil {
		t.Fatalf("read: %v", err)
	}

	snap := wrapped.Snapshot()
	if snap.BytesIn != 5 || snap.BytesOut != 5 {
		t.Fatalf("unexpected snapshot: %+v", snap)
	}
}
