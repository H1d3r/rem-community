//go:build graph
// +build graph

package simplex

import (
	"bytes"
	"crypto/rand"
	"crypto/sha256"
	"fmt"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// ============================================================
// OneDrive Long-Running Stability Tests — 真实 Microsoft Graph API
//
// 持续性测试: 10 分钟 / 1 小时
// 验证: 数据完整性、连接持久性、错误恢复、资源泄漏
//
// 运行:
//   go test -v -tags graph -run "TestOneDriveLongRun_10min" ./x/simplex/ -timeout 900s
//   go test -v -tags graph -run "TestOneDriveLongRun_1h"    ./x/simplex/ -timeout 4200s
// ============================================================

type longRunStats struct {
	totalSent     int64
	totalRecv     int64
	totalBytes    int64
	errors        int64
	hashFails     int64
	maxLatency    time.Duration
	minLatency    time.Duration
	totalLatency  time.Duration
	latencyCount  int64
	mu            sync.Mutex
}

func (s *longRunStats) recordLatency(d time.Duration) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.totalLatency += d
	s.latencyCount++
	if d > s.maxLatency {
		s.maxLatency = d
	}
	if s.minLatency == 0 || d < s.minLatency {
		s.minLatency = d
	}
}

func (s *longRunStats) report(t *testing.T, label string, elapsed time.Duration) {
	t.Helper()
	sent := atomic.LoadInt64(&s.totalSent)
	recv := atomic.LoadInt64(&s.totalRecv)
	errs := atomic.LoadInt64(&s.errors)
	hashF := atomic.LoadInt64(&s.hashFails)
	byts := atomic.LoadInt64(&s.totalBytes)

	t.Logf("=== %s Final Report ===", label)
	t.Logf("  Duration:      %v", elapsed)
	t.Logf("  Sent:          %d messages", sent)
	t.Logf("  Received:      %d messages", recv)
	t.Logf("  Errors:        %d", errs)
	t.Logf("  Hash failures: %d", hashF)
	t.Logf("  Total bytes:   %d", byts)

	if byts > 0 && elapsed.Seconds() > 0 {
		bps := float64(byts) / elapsed.Seconds()
		t.Logf("  Avg bandwidth: %.2f KB/s", bps/1024)
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	if s.latencyCount > 0 {
		avg := s.totalLatency / time.Duration(s.latencyCount)
		t.Logf("  Latency avg:   %v", avg)
		t.Logf("  Latency min:   %v", s.minLatency)
		t.Logf("  Latency max:   %v", s.maxLatency)
	}

	if recv < sent*8/10 {
		t.Errorf("Too many lost messages: sent %d, received %d (< 80%%)", sent, recv)
	}
	if hashF > 0 {
		t.Errorf("Data integrity failures: %d hash mismatches", hashF)
	}
}

// oneDriveLongRunURL builds a real-API URL with configurable interval
func oneDriveLongRunURL(prefix string, intervalMs int) string {
	return fmt.Sprintf("onedrive:///?tenant=%s&client=%s&secret=%s&site=%s&prefix=%s&interval=%d",
		TestTenantID, TestClientID, TestClientSecret, TestSiteID, prefix, intervalMs)
}

// setupLongRunPair creates a server + client pair for long-run tests
func setupLongRunPair(t *testing.T, prefix string, intervalMs int) (*OneDriveServer, *OneDriveClient, *SimplexAddr) {
	t.Helper()

	serverURL := oneDriveLongRunURL(prefix, intervalMs)
	server, err := NewOneDriveServer("onedrive", serverURL)
	if err != nil {
		t.Fatalf("NewOneDriveServer error: %v", err)
	}

	clientURL := oneDriveLongRunURL(prefix, intervalMs)
	addr, err := ResolveOneDriveAddr("onedrive", clientURL)
	if err != nil {
		server.Close()
		t.Fatalf("ResolveOneDriveAddr error: %v", err)
	}

	client, err := NewOneDriveClient(addr)
	if err != nil {
		server.Close()
		t.Fatalf("NewOneDriveClient error: %v", err)
	}

	// 预热
	warmup := NewSimplexPacketWithMaxSize([]byte("warmup"), SimplexPacketTypeDATA, MaxOneDriveMessageSize)
	client.Send(warmup, addr)

	timeout := time.After(60 * time.Second)
	for {
		select {
		case <-timeout:
			client.Close()
			server.Close()
			t.Fatal("Timeout waiting for warmup")
		default:
			pkt, _, _ := server.Receive()
			if pkt != nil {
				t.Logf("Warmup done, connection established")
				return server, client, addr
			}
			time.Sleep(500 * time.Millisecond)
		}
	}
}

// receiveAll accumulates multiple SimplexPackets until expectedLen bytes received
func receiveAll(server *OneDriveServer, expectedLen int, timeout time.Duration) ([]byte, bool) {
	var received []byte
	deadline := time.After(timeout)
	for len(received) < expectedLen {
		select {
		case <-deadline:
			return received, false
		default:
			pkt, _, _ := server.Receive()
			if pkt != nil {
				received = append(received, pkt.Data...)
			} else {
				time.Sleep(500 * time.Millisecond)
			}
		}
	}
	return received, true
}

// receiveAllClient accumulates from client side
func receiveAllClient(client *OneDriveClient, expectedLen int, timeout time.Duration) ([]byte, bool) {
	var received []byte
	deadline := time.After(timeout)
	for len(received) < expectedLen {
		select {
		case <-deadline:
			return received, false
		default:
			pkt, _, _ := client.Receive()
			if pkt != nil {
				received = append(received, pkt.Data...)
			} else {
				time.Sleep(500 * time.Millisecond)
			}
		}
	}
	return received, true
}

// --- 10 分钟持续通信稳定性测试 ---

func TestOneDriveLongRun_10min(t *testing.T) {
	runLongStabilityTest(t, 10*time.Minute, "10min")
}

// --- 1 小时持续通信稳定性测试 ---

func TestOneDriveLongRun_1h(t *testing.T) {
	runLongStabilityTest(t, 1*time.Hour, "1h")
}

func runLongStabilityTest(t *testing.T, duration time.Duration, label string) {
	prefix := fmt.Sprintf("longrun-%s-%s", label, randomString(6))
	intervalMs := 2000

	t.Logf("=== Long-Run Stability Test: %s ===", label)
	t.Logf("Prefix: %s, Interval: %dms, Duration: %v", prefix, intervalMs, duration)

	server, client, addr := setupLongRunPair(t, prefix, intervalMs)
	defer server.Close()
	defer client.Close()

	stats := &longRunStats{}
	start := time.Now()
	deadline := time.After(duration)

	// 数据大小阶梯循环: 小→中→大 交替
	sizes := []int{
		256,          // 小消息
		4 * 1024,     // 4 KB
		16 * 1024,    // 16 KB
		60 * 1024,    // 60 KB (单包极限)
		128 * 1024,   // 128 KB (多包)
		256 * 1024,   // 256 KB
	}

	// 每 30 秒报告一次进度
	progressTicker := time.NewTicker(30 * time.Second)
	defer progressTicker.Stop()

	round := 0
	for {
		select {
		case <-deadline:
			elapsed := time.Since(start)
			stats.report(t, label, elapsed)
			return
		case <-progressTicker.C:
			elapsed := time.Since(start)
			sent := atomic.LoadInt64(&stats.totalSent)
			recv := atomic.LoadInt64(&stats.totalRecv)
			errs := atomic.LoadInt64(&stats.errors)
			byts := atomic.LoadInt64(&stats.totalBytes)
			t.Logf("[%v] sent=%d recv=%d errs=%d bytes=%d",
				elapsed.Round(time.Second), sent, recv, errs, byts)
		default:
		}

		size := sizes[round%len(sizes)]
		data := make([]byte, size)
		rand.Read(data)
		expectedHash := sha256.Sum256(data)

		// Client → Server
		sendStart := time.Now()
		pkts := NewSimplexPacketWithMaxSize(data, SimplexPacketTypeDATA, MaxOneDriveMessageSize)
		_, err := client.Send(pkts, addr)
		if err != nil {
			atomic.AddInt64(&stats.errors, 1)
			t.Logf("Round %d: Send error: %v", round, err)
			time.Sleep(5 * time.Second)
			round++
			continue
		}
		atomic.AddInt64(&stats.totalSent, 1)

		// 接收（可能多包）
		received, ok := receiveAll(server, size, 120*time.Second)
		if !ok {
			atomic.AddInt64(&stats.errors, 1)
			t.Logf("Round %d: Timeout receiving %d bytes (got %d)", round, size, len(received))
			round++
			continue
		}

		lat := time.Since(sendStart)
		atomic.AddInt64(&stats.totalRecv, 1)
		atomic.AddInt64(&stats.totalBytes, int64(size))
		stats.recordLatency(lat)

		// 校验完整性
		actualHash := sha256.Sum256(received)
		if !bytes.Equal(expectedHash[:], actualHash[:]) {
			atomic.AddInt64(&stats.hashFails, 1)
			t.Logf("Round %d: Hash mismatch for %d bytes!", round, size)
		}

		round++
	}
}

// --- 10 分钟双向通信测试 ---

func TestOneDriveLongRun_10min_Bidirectional(t *testing.T) {
	runBidirectionalStabilityTest(t, 10*time.Minute, "10min-bidir")
}

// --- 1 小时双向通信测试 ---

func TestOneDriveLongRun_1h_Bidirectional(t *testing.T) {
	runBidirectionalStabilityTest(t, 1*time.Hour, "1h-bidir")
}

func runBidirectionalStabilityTest(t *testing.T, duration time.Duration, label string) {
	prefix := fmt.Sprintf("longrun-%s-%s", label, randomString(6))
	intervalMs := 2000

	t.Logf("=== Bidirectional Long-Run Test: %s ===", label)
	t.Logf("Prefix: %s, Interval: %dms, Duration: %v", prefix, intervalMs, duration)

	server, client, addr := setupLongRunPair(t, prefix, intervalMs)
	defer server.Close()
	defer client.Close()

	c2sStats := &longRunStats{}
	s2cStats := &longRunStats{}
	start := time.Now()
	deadline := time.After(duration)

	progressTicker := time.NewTicker(30 * time.Second)
	defer progressTicker.Stop()

	round := 0
	msgSize := 4 * 1024 // 4 KB per direction

	for {
		select {
		case <-deadline:
			elapsed := time.Since(start)
			t.Logf("")
			t.Logf("--- Client → Server ---")
			c2sStats.report(t, label+"-C2S", elapsed)
			t.Logf("")
			t.Logf("--- Server → Client ---")
			s2cStats.report(t, label+"-S2C", elapsed)
			return
		case <-progressTicker.C:
			elapsed := time.Since(start)
			c2sSent := atomic.LoadInt64(&c2sStats.totalSent)
			c2sRecv := atomic.LoadInt64(&c2sStats.totalRecv)
			s2cSent := atomic.LoadInt64(&s2cStats.totalSent)
			s2cRecv := atomic.LoadInt64(&s2cStats.totalRecv)
			t.Logf("[%v] C2S: sent=%d recv=%d | S2C: sent=%d recv=%d",
				elapsed.Round(time.Second), c2sSent, c2sRecv, s2cSent, s2cRecv)
		default:
		}

		// === Client → Server ===
		c2sData := make([]byte, msgSize)
		rand.Read(c2sData)
		c2sHash := sha256.Sum256(c2sData)

		sendStart := time.Now()
		pkts := NewSimplexPacketWithMaxSize(c2sData, SimplexPacketTypeDATA, MaxOneDriveMessageSize)
		_, err := client.Send(pkts, addr)
		if err != nil {
			atomic.AddInt64(&c2sStats.errors, 1)
			t.Logf("Round %d C2S: Send error: %v", round, err)
			time.Sleep(5 * time.Second)
			round++
			continue
		}
		atomic.AddInt64(&c2sStats.totalSent, 1)

		received, serverAddr, ok := receiveAllWithAddr(server, msgSize, 120*time.Second)
		if !ok {
			atomic.AddInt64(&c2sStats.errors, 1)
			t.Logf("Round %d C2S: Timeout (got %d/%d bytes)", round, len(received), msgSize)
			round++
			continue
		}

		c2sLat := time.Since(sendStart)
		atomic.AddInt64(&c2sStats.totalRecv, 1)
		atomic.AddInt64(&c2sStats.totalBytes, int64(msgSize))
		c2sStats.recordLatency(c2sLat)

		recvHash := sha256.Sum256(received)
		if !bytes.Equal(c2sHash[:], recvHash[:]) {
			atomic.AddInt64(&c2sStats.hashFails, 1)
			t.Logf("Round %d C2S: Hash mismatch!", round)
		}

		// === Server → Client ===
		s2cData := make([]byte, msgSize)
		rand.Read(s2cData)
		s2cHash := sha256.Sum256(s2cData)

		sendStart = time.Now()
		respPkts := NewSimplexPacketWithMaxSize(s2cData, SimplexPacketTypeDATA, MaxOneDriveMessageSize)
		_, err = server.Send(respPkts, serverAddr)
		if err != nil {
			atomic.AddInt64(&s2cStats.errors, 1)
			t.Logf("Round %d S2C: Send error: %v", round, err)
			round++
			continue
		}
		atomic.AddInt64(&s2cStats.totalSent, 1)

		clientRecv, ok := receiveAllClient(client, msgSize, 120*time.Second)
		if !ok {
			atomic.AddInt64(&s2cStats.errors, 1)
			t.Logf("Round %d S2C: Timeout (got %d/%d bytes)", round, len(clientRecv), msgSize)
			round++
			continue
		}

		s2cLat := time.Since(sendStart)
		atomic.AddInt64(&s2cStats.totalRecv, 1)
		atomic.AddInt64(&s2cStats.totalBytes, int64(msgSize))
		s2cStats.recordLatency(s2cLat)

		clientHash := sha256.Sum256(clientRecv)
		if !bytes.Equal(s2cHash[:], clientHash[:]) {
			atomic.AddInt64(&s2cStats.hashFails, 1)
			t.Logf("Round %d S2C: Hash mismatch!", round)
		}

		round++
	}
}

// receiveAllWithAddr is like receiveAll but also returns the sender address
func receiveAllWithAddr(server *OneDriveServer, expectedLen int, timeout time.Duration) ([]byte, *SimplexAddr, bool) {
	var received []byte
	var senderAddr *SimplexAddr
	deadline := time.After(timeout)
	for len(received) < expectedLen {
		select {
		case <-deadline:
			return received, senderAddr, false
		default:
			pkt, addr, _ := server.Receive()
			if pkt != nil {
				received = append(received, pkt.Data...)
				if senderAddr == nil {
					senderAddr = addr
				}
			} else {
				time.Sleep(500 * time.Millisecond)
			}
		}
	}
	return received, senderAddr, true
}
