package cio

import (
	"net"
	"sync"
	"sync/atomic"
	"time"
)

const (
	statsBucketDuration = 250 * time.Millisecond
	statsBucketCount    = 4
	statsWindowSeconds  = int64(statsBucketDuration/time.Millisecond) * statsBucketCount / 1000
)

type ConnStatsSnapshot struct {
	BytesIn    int64
	BytesOut   int64
	RateInBps  int64
	RateOutBps int64
	StartedAt  time.Time
	LastActive time.Time
}

type trafficBucket struct {
	startedAt time.Time
	inBytes   int64
	outBytes  int64
}

// ConnStats tracks bidirectional bytes and a smoothed 1s rate for a net.Conn.
type ConnStats struct {
	now func() time.Time

	startedAt  time.Time
	lastActive int64
	bytesIn    int64
	bytesOut   int64

	mu            sync.Mutex
	currentBucket time.Time
	currentIndex  int
	buckets       [statsBucketCount]trafficBucket
}

func NewConnStats() *ConnStats {
	return NewConnStatsWithClock(time.Now)
}

func NewConnStatsWithClock(now func() time.Time) *ConnStats {
	if now == nil {
		now = time.Now
	}
	started := now()
	return &ConnStats{
		now:        now,
		startedAt:  started,
		lastActive: started.UnixNano(),
	}
}

func (s *ConnStats) RecordRead(n int) {
	s.record(int64(n), false)
}

func (s *ConnStats) RecordWrite(n int) {
	s.record(int64(n), true)
}

func (s *ConnStats) record(n int64, outbound bool) {
	if s == nil || n <= 0 {
		return
	}

	now := s.now()
	if outbound {
		atomic.AddInt64(&s.bytesOut, n)
	} else {
		atomic.AddInt64(&s.bytesIn, n)
	}
	atomic.StoreInt64(&s.lastActive, now.UnixNano())

	s.mu.Lock()
	defer s.mu.Unlock()

	s.rotateLocked(now)
	bucket := &s.buckets[s.currentIndex]
	if outbound {
		bucket.outBytes += n
	} else {
		bucket.inBytes += n
	}
}

func (s *ConnStats) Snapshot() ConnStatsSnapshot {
	if s == nil {
		return ConnStatsSnapshot{}
	}

	now := s.now()
	s.mu.Lock()
	s.rotateLocked(now)
	var rateIn, rateOut int64
	for i := range s.buckets {
		rateIn += s.buckets[i].inBytes
		rateOut += s.buckets[i].outBytes
	}
	s.mu.Unlock()

	lastActiveUnix := atomic.LoadInt64(&s.lastActive)
	lastActive := time.Time{}
	if lastActiveUnix > 0 {
		lastActive = time.Unix(0, lastActiveUnix)
	}

	return ConnStatsSnapshot{
		BytesIn:    atomic.LoadInt64(&s.bytesIn),
		BytesOut:   atomic.LoadInt64(&s.bytesOut),
		RateInBps:  rateIn / statsWindowSeconds,
		RateOutBps: rateOut / statsWindowSeconds,
		StartedAt:  s.startedAt,
		LastActive: lastActive,
	}
}

func (s *ConnStats) rotateLocked(now time.Time) {
	bucketStart := now.Truncate(statsBucketDuration)
	if s.currentBucket.IsZero() {
		s.currentBucket = bucketStart
		s.buckets[s.currentIndex] = trafficBucket{startedAt: bucketStart}
		return
	}
	if !bucketStart.After(s.currentBucket) {
		return
	}

	steps := int(bucketStart.Sub(s.currentBucket) / statsBucketDuration)
	if steps >= statsBucketCount {
		for i := range s.buckets {
			s.buckets[i] = trafficBucket{}
		}
		s.currentIndex = 0
		s.currentBucket = bucketStart
		s.buckets[0] = trafficBucket{startedAt: bucketStart}
		return
	}

	for i := 0; i < steps; i++ {
		s.currentIndex = (s.currentIndex + 1) % statsBucketCount
		s.buckets[s.currentIndex] = trafficBucket{
			startedAt: s.currentBucket.Add(time.Duration(i+1) * statsBucketDuration),
		}
	}
	s.currentBucket = bucketStart
}

type statsProvider interface {
	Stats() *ConnStats
}

// StatsConn transparently wraps a net.Conn and records byte counters/rates.
type StatsConn struct {
	net.Conn
	stats *ConnStats
}

func WrapStatsConn(conn net.Conn) *StatsConn {
	if conn == nil {
		return nil
	}
	if wrapped, ok := conn.(*StatsConn); ok {
		return wrapped
	}
	if provider, ok := conn.(statsProvider); ok && provider.Stats() != nil {
		return &StatsConn{Conn: conn, stats: provider.Stats()}
	}
	return &StatsConn{
		Conn:  conn,
		stats: NewConnStats(),
	}
}

func StatsOf(conn net.Conn) *ConnStats {
	if provider, ok := conn.(statsProvider); ok {
		return provider.Stats()
	}
	return nil
}

func (c *StatsConn) Stats() *ConnStats {
	if c == nil {
		return nil
	}
	return c.stats
}

func (c *StatsConn) Snapshot() ConnStatsSnapshot {
	if c == nil || c.stats == nil {
		return ConnStatsSnapshot{}
	}
	return c.stats.Snapshot()
}

func (c *StatsConn) Read(p []byte) (int, error) {
	n, err := c.Conn.Read(p)
	if n > 0 && c.stats != nil {
		c.stats.RecordRead(n)
	}
	return n, err
}

func (c *StatsConn) Write(p []byte) (int, error) {
	n, err := c.Conn.Write(p)
	if n > 0 && c.stats != nil {
		c.stats.RecordWrite(n)
	}
	return n, err
}
