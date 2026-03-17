//go:build graph
// +build graph

package simplex

import (
	"bytes"
	"crypto/rand"
	"crypto/sha256"
	"fmt"
	"sort"
	"testing"
	"time"
)

// ============================================================
// OneDrive Bandwidth Benchmark — 真实 Microsoft Graph API
//
// 核心思路: OneDrive 轮询频率低，但单次文件传输可以携带大 payload。
// 因此带宽测试关注的是: 单次传输不同大小数据的实际吞吐量。
//
// 运行:
//   go test -v -tags graph -run "TestOneDriveBench" ./x/simplex/ -timeout 600s
// ============================================================

type oneDriveBenchResult struct {
	Scenario   string
	Duration   time.Duration
	TotalBytes int64
	TotalMsgs  int64
	Errors     int64
	Latencies  []time.Duration
}

func (r *oneDriveBenchResult) report(t *testing.T) {
	t.Helper()
	t.Logf("=== %s ===", r.Scenario)
	t.Logf("  Duration:    %v", r.Duration)
	t.Logf("  Messages:    %d (errors: %d)", r.TotalMsgs, r.Errors)

	if r.TotalBytes > 0 && r.Duration.Seconds() > 0 {
		bps := float64(r.TotalBytes) / r.Duration.Seconds()
		if bps > 1024*1024 {
			t.Logf("  Throughput:  %.2f MB/s (%d bytes)", bps/1024/1024, r.TotalBytes)
		} else {
			t.Logf("  Throughput:  %.2f KB/s (%d bytes)", bps/1024, r.TotalBytes)
		}
	}
	if len(r.Latencies) > 0 {
		sort.Slice(r.Latencies, func(i, j int) bool { return r.Latencies[i] < r.Latencies[j] })
		n := len(r.Latencies)
		var sum time.Duration
		for _, l := range r.Latencies {
			sum += l
		}
		t.Logf("  Latency avg: %v", sum/time.Duration(n))
		t.Logf("  Latency min: %v", r.Latencies[0])
		t.Logf("  Latency p50: %v", r.Latencies[n/2])
		t.Logf("  Latency max: %v", r.Latencies[n-1])
	}
}

func oneDriveBenchServerURL(prefix string, intervalMs int) string {
	return fmt.Sprintf("onedrive:///?tenant=%s&client=%s&secret=%s&site=%s&prefix=%s&interval=%d",
		TestTenantID, TestClientID, TestClientSecret, TestSiteID, prefix, intervalMs)
}

func oneDriveBenchClientURL(prefix string, intervalMs int) string {
	return fmt.Sprintf("onedrive:///?tenant=%s&client=%s&secret=%s&site=%s&prefix=%s&interval=%d",
		TestTenantID, TestClientID, TestClientSecret, TestSiteID, prefix, intervalMs)
}

// setupOneDriveBench 创建一个 server + client 并等待连接建立
func setupOneDriveBench(t *testing.T, prefix string, intervalMs int) (*OneDriveServer, *OneDriveClient, *SimplexAddr) {
	t.Helper()

	server, err := NewOneDriveServer("onedrive", oneDriveBenchServerURL(prefix, intervalMs))
	if err != nil {
		t.Fatalf("NewOneDriveServer error: %v", err)
	}

	addr, _ := ResolveOneDriveAddr("onedrive", oneDriveBenchClientURL(prefix, intervalMs))
	client, err := NewOneDriveClient(addr)
	if err != nil {
		server.Close()
		t.Fatalf("NewOneDriveClient error: %v", err)
	}

	// 用一个小消息预热，确保连接建立 + 服务端发现客户端
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

// sendAndMeasure 发送一次大数据并测量端到端延迟和数据完整性
// 当 data > MaxOneDriveMessageSize 时，数据被分成多个 SimplexPacket，
// 需要多次 Receive() 并拼接才能得到完整数据。
func sendAndMeasure(t *testing.T, client *OneDriveClient, server *OneDriveServer, addr *SimplexAddr, data []byte) (time.Duration, bool) {
	t.Helper()

	expectedHash := sha256.Sum256(data)
	expectedLen := len(data)

	start := time.Now()

	pkts := NewSimplexPacketWithMaxSize(data, SimplexPacketTypeDATA, MaxOneDriveMessageSize)
	_, err := client.Send(pkts, addr)
	if err != nil {
		t.Errorf("Send error: %v", err)
		return 0, false
	}

	// 累积接收所有分片
	var received []byte
	timeout := time.After(120 * time.Second)
	for len(received) < expectedLen {
		select {
		case <-timeout:
			t.Errorf("Timeout receiving %d bytes (got %d)", expectedLen, len(received))
			return time.Since(start), false
		default:
			pkt, _, _ := server.Receive()
			if pkt != nil {
				received = append(received, pkt.Data...)
			} else {
				time.Sleep(500 * time.Millisecond)
			}
		}
	}

	lat := time.Since(start)

	// 校验数据完整性
	actualHash := sha256.Sum256(received)
	if !bytes.Equal(expectedHash[:], actualHash[:]) {
		t.Errorf("Data integrity check FAILED: expected %d bytes, got %d, hash mismatch", expectedLen, len(received))
		return lat, false
	}

	return lat, true
}

// --- Scenario 1: 单包带宽阶梯测试 ---
// 逐步增大单次传输的数据量，测量 OneDrive API 的实际上传+下载带宽

func TestOneDriveBench_SinglePacket_Bandwidth(t *testing.T) {
	prefix := "bench-bw-" + randomString(6)
	intervalMs := 1000 // 1 秒轮询，减少等待开销

	t.Logf("=== Single Packet Bandwidth Test ===")
	t.Logf("Prefix: %s, Interval: %dms", prefix, intervalMs)

	server, client, addr := setupOneDriveBench(t, prefix, intervalMs)
	defer server.Close()
	defer client.Close()

	// 阶梯测试: 从 1KB 到 MaxOneDriveMessageSize (65KB)
	// 每个大小只发一包，测量纯 API 传输带宽
	sizes := []int{
		1 * 1024,  // 1 KB
		4 * 1024,  // 4 KB
		16 * 1024, // 16 KB
		32 * 1024, // 32 KB
		60 * 1024, // 60 KB (接近 MaxOneDriveMessageSize)
	}

	t.Logf("")
	t.Logf("%-12s  %-12s  %-14s  %s", "Size", "Latency", "Bandwidth", "Integrity")
	t.Logf("%-12s  %-12s  %-14s  %s", "----", "-------", "---------", "---------")

	var allLatencies []time.Duration
	var allBytes int64

	for _, size := range sizes {
		data := make([]byte, size)
		rand.Read(data)

		lat, ok := sendAndMeasure(t, client, server, addr, data)

		status := "OK"
		if !ok {
			status = "FAIL"
		}

		bw := float64(size) / lat.Seconds() / 1024
		t.Logf("%-12s  %-12v  %-14s  %s",
			formatBytes(size), lat, fmt.Sprintf("%.1f KB/s", bw), status)

		if ok {
			allLatencies = append(allLatencies, lat)
			allBytes += int64(size)
		}
	}

	t.Logf("")
	result := &oneDriveBenchResult{
		Scenario:   "Single Packet Bandwidth (1KB→60KB)",
		TotalBytes: allBytes,
		TotalMsgs:  int64(len(allLatencies)),
		Latencies:  allLatencies,
	}
	var totalDur time.Duration
	for _, l := range allLatencies {
		totalDur += l
	}
	result.Duration = totalDur
	result.report(t)
}

// --- Scenario 2: 大文件带宽测试 ---
// 发送超过 MaxOneDriveMessageSize 的数据（会被分成多个 SimplexPacket 但一次文件上传）

func TestOneDriveBench_LargeFile_Bandwidth(t *testing.T) {
	prefix := "bench-lg-" + randomString(6)
	intervalMs := 1000

	t.Logf("=== Large File Bandwidth Test ===")
	t.Logf("Prefix: %s, Interval: %dms", prefix, intervalMs)
	t.Logf("Note: data > %d bytes is split into multiple SimplexPackets but uploaded as one file", MaxOneDriveMessageSize)

	server, client, addr := setupOneDriveBench(t, prefix, intervalMs)
	defer server.Close()
	defer client.Close()

	// 大文件: 超过单个 SimplexPacket 上限，测试分片 + 一次上传的带宽
	sizes := []int{
		64 * 1024,  // 64 KB (1 packet + 余量)
		128 * 1024, // 128 KB (~2 packets)
		256 * 1024, // 256 KB (~4 packets)
		512 * 1024, // 512 KB (~8 packets)
		1024 * 1024, // 1 MB
	}

	t.Logf("")
	t.Logf("%-12s  %-12s  %-14s  %s", "Size", "Latency", "Bandwidth", "Integrity")
	t.Logf("%-12s  %-12s  %-14s  %s", "----", "-------", "---------", "---------")

	var allLatencies []time.Duration
	var allBytes int64

	for _, size := range sizes {
		data := make([]byte, size)
		rand.Read(data)

		lat, ok := sendAndMeasure(t, client, server, addr, data)

		status := "OK"
		if !ok {
			status = "FAIL"
		}

		bw := float64(size) / lat.Seconds() / 1024
		t.Logf("%-12s  %-12v  %-14s  %s",
			formatBytes(size), lat, fmt.Sprintf("%.1f KB/s", bw), status)

		if ok {
			allLatencies = append(allLatencies, lat)
			allBytes += int64(size)
		}
	}

	t.Logf("")
	result := &oneDriveBenchResult{
		Scenario:   "Large File Bandwidth (64KB→1MB)",
		TotalBytes: allBytes,
		TotalMsgs:  int64(len(allLatencies)),
		Latencies:  allLatencies,
	}
	var totalDur time.Duration
	for _, l := range allLatencies {
		totalDur += l
	}
	result.Duration = totalDur
	result.report(t)
}

// --- Scenario 3: RTT 延迟 (双向) ---

func TestOneDriveBench_RTT(t *testing.T) {
	prefix := "bench-rtt-" + randomString(6)
	intervalMs := 1000

	t.Logf("=== RTT Benchmark (C→S→C) ===")
	t.Logf("Prefix: %s, Interval: %dms", prefix, intervalMs)

	server, client, addr := setupOneDriveBench(t, prefix, intervalMs)
	defer server.Close()
	defer client.Close()

	const rounds = 5
	const msgSize = 4096
	var latencies []time.Duration
	var totalBytes int64

	for round := 0; round < rounds; round++ {
		data := make([]byte, msgSize)
		rand.Read(data)

		rttStart := time.Now()

		// Client → Server
		pkts := NewSimplexPacketWithMaxSize(data, SimplexPacketTypeDATA, MaxOneDriveMessageSize)
		client.Send(pkts, addr)

		var recvAddr *SimplexAddr
		timeout := time.After(60 * time.Second)
		for {
			select {
			case <-timeout:
				t.Fatalf("Round %d: timeout C→S", round)
			default:
			}
			pkt, a, _ := server.Receive()
			if pkt != nil {
				recvAddr = a
				break
			}
			time.Sleep(500 * time.Millisecond)
		}

		// Server → Client
		respData := make([]byte, msgSize)
		rand.Read(respData)
		respPkts := NewSimplexPacketWithMaxSize(respData, SimplexPacketTypeDATA, MaxOneDriveMessageSize)
		server.Send(respPkts, recvAddr)

		timeout2 := time.After(60 * time.Second)
		for {
			select {
			case <-timeout2:
				t.Fatalf("Round %d: timeout S→C", round)
			default:
			}
			pkt, _, _ := client.Receive()
			if pkt != nil {
				break
			}
			time.Sleep(500 * time.Millisecond)
		}

		rtt := time.Since(rttStart)
		latencies = append(latencies, rtt)
		totalBytes += int64(msgSize * 2)
		t.Logf("  Round %d: RTT = %v", round, rtt)
	}

	var totalDur time.Duration
	for _, l := range latencies {
		totalDur += l
	}
	result := &oneDriveBenchResult{
		Scenario:   fmt.Sprintf("RTT (%d rounds, %s/msg)", rounds, formatBytes(msgSize)),
		Duration:   totalDur,
		TotalBytes: totalBytes,
		TotalMsgs:  int64(rounds),
		Latencies:  latencies,
	}
	result.report(t)
}

// --- helpers ---

func formatBytes(n int) string {
	switch {
	case n >= 1024*1024:
		return fmt.Sprintf("%d MB", n/1024/1024)
	case n >= 1024:
		return fmt.Sprintf("%d KB", n/1024)
	default:
		return fmt.Sprintf("%d B", n)
	}
}
