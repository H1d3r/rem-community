//go:build graph
// +build graph

package simplex

import (
	"crypto/rand"
	"fmt"
	"os"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// ============================================================
// 极限吞吐量测试 — 1 分钟持续灌包，测量真实 API 带宽
//
// 运行:
//   SHAREPOINT_REAL=1 go test -v -tags graph -run "TestThroughput_" ./x/simplex/ -timeout 600s
// ============================================================

func skipIfNotRealThroughput(t *testing.T) {
	t.Helper()
	if os.Getenv("SHAREPOINT_REAL") != "1" {
		t.Skip("Skipping real API throughput test (set SHAREPOINT_REAL=1)")
	}
}

// throughputResult holds 1-minute sustained throughput results
type throughputResult struct {
	Transport    string
	Direction    string
	Duration     time.Duration
	TotalBytes   int64
	TotalPackets int64
	Errors       int64
	PacketSize   int
}

func (r *throughputResult) report(t *testing.T) {
	t.Helper()
	t.Logf("=== %s %s Throughput ===", r.Transport, r.Direction)
	t.Logf("  Duration:     %v", r.Duration)
	t.Logf("  Packet size:  %s", formatBytes(r.PacketSize))
	t.Logf("  Packets sent: %d (errors: %d)", r.TotalPackets, r.Errors)
	t.Logf("  Total data:   %s", formatBytes(int(r.TotalBytes)))
	if r.Duration.Seconds() > 0 {
		mbps := float64(r.TotalBytes) / r.Duration.Seconds() / 1024 / 1024
		kbps := float64(r.TotalBytes) / r.Duration.Seconds() / 1024
		if mbps >= 1 {
			t.Logf("  Throughput:   %.2f MB/s", mbps)
		} else {
			t.Logf("  Throughput:   %.2f KB/s", kbps)
		}
	}
}

// ---------------------------------------------------------------------------
// OneDrive: 1 分钟极限吞吐 (Client→Server, 4MB packets)
//
// OneDrive Send() 是异步的（写入 pendingData，polling 协程实际上传），
// 因此必须以 polling 间隔节奏发送，并以 server 实际收到的字节为吞吐量指标。
// ---------------------------------------------------------------------------

func TestThroughput_OneDrive_1min(t *testing.T) {
	skipIfNotRealThroughput(t)

	const (
		testDuration = 60 * time.Second
		intervalMs   = 1000
	)

	// 测试多种包大小，每种发送一个并等待投递，测量真实端到端吞吐
	prefix := "tp-od-" + randomString(6)
	server, client, addr := setupOneDriveBench(t, prefix, intervalMs)
	defer server.Close()
	defer client.Close()

	t.Logf("MaxOneDriveMessageSize = %s", formatBytes(MaxOneDriveMessageSize))
	t.Logf("")
	t.Logf("--- Phase 1: 阶梯测试（单包端到端延迟）---")
	t.Logf("%-12s  %-12s  %-14s  %s", "Size", "Latency", "Bandwidth", "Status")

	// 阶梯: 测量不同大小的单包端到端延迟
	stepSizes := []int{64 * 1024, 256 * 1024, 512 * 1024, 1024 * 1024}
	if MaxOneDriveMessageSize >= 2*1024*1024 {
		stepSizes = append(stepSizes, MaxOneDriveMessageSize-1024)
	}

	for _, size := range stepSizes {
		data := make([]byte, size)
		rand.Read(data)
		lat, ok := sendAndMeasure(t, client, server, addr, data)
		status := "OK"
		if !ok {
			status = "FAIL"
		}
		bw := float64(size) / lat.Seconds() / 1024
		t.Logf("%-12s  %-12v  %-14s  %s",
			formatBytes(size), lat.Truncate(time.Millisecond), fmt.Sprintf("%.1f KB/s", bw), status)
	}

	// Phase 2: 持续发送 1 分钟，send-wait-recv 模式
	t.Logf("")
	t.Logf("--- Phase 2: 持续吞吐（send→wait→recv 循环，1 分钟）---")

	packetSize := MaxOneDriveMessageSize - 1024
	payload := make([]byte, packetSize)
	rand.Read(payload)

	var totalBytes int64
	var totalPkts int64
	start := time.Now()
	deadline := start.Add(testDuration)

	for time.Now().Before(deadline) {
		lat, ok := sendAndMeasure(t, client, server, addr, payload)
		if !ok {
			t.Logf("  [%v] Send-recv FAILED", time.Since(start).Truncate(time.Second))
			continue
		}
		totalPkts++
		totalBytes += int64(packetSize)
		elapsed := time.Since(start).Truncate(time.Second)
		bw := float64(packetSize) / lat.Seconds() / 1024
		t.Logf("  [%v] #%d delivered (%s in %v, %.1f KB/s)",
			elapsed, totalPkts, formatBytes(packetSize), lat.Truncate(time.Millisecond), bw)
	}

	duration := time.Since(start)
	t.Logf("")
	t.Logf("=== OneDrive Throughput Result ===")
	t.Logf("  Duration:       %v", duration.Truncate(time.Second))
	t.Logf("  Packet size:    %s", formatBytes(packetSize))
	t.Logf("  Delivered:      %d packets (%s)", totalPkts, formatBytes(int(totalBytes)))
	if duration.Seconds() > 0 && totalBytes > 0 {
		kbps := float64(totalBytes) / duration.Seconds() / 1024
		if kbps >= 1024 {
			t.Logf("  Throughput:     %.2f MB/s", kbps/1024)
		} else {
			t.Logf("  Throughput:     %.2f KB/s", kbps)
		}
	}
}

// ---------------------------------------------------------------------------
// SharePoint: 1 分钟极限吞吐 (Client→Server, 190KB packets)
// ---------------------------------------------------------------------------

func TestThroughput_SharePoint_1min(t *testing.T) {
	skipIfNotRealThroughput(t)

	const (
		testDuration = 60 * time.Second
		intervalMs   = 3000 // 3s polling — SharePoint 默认
	)

	// 使用接近 MaxSharePointMessageSize 的包大小
	packetSize := MaxSharePointMessageSize - 1024 // ~189KB
	t.Logf("SharePoint max packet: %s, test packet: %s", formatBytes(MaxSharePointMessageSize), formatBytes(packetSize))

	listName := "tp-sp-" + randomString(6)

	// 创建 server
	serverURL := fmt.Sprintf("sharepoint:///%s?tenant=%s&client=%s&secret=%s&site=%s&interval=%d",
		listName, TestTenantID, TestClientID, TestClientSecret, TestSiteID, intervalMs)
	server, err := NewSharePointServer("sharepoint", serverURL)
	if err != nil {
		t.Fatalf("NewSharePointServer: %v", err)
	}
	defer server.Close()

	// 提取 server 的 list_id 和 data_col
	serverConfig := server.config
	t.Logf("Server list: %s (id=%s, data_col=%s)", serverConfig.ListName, serverConfig.ListID, serverConfig.DataColumn)

	// 创建 client（需要 list_id 和 data_col）
	clientURL := fmt.Sprintf("sharepoint:///%s?tenant=%s&client=%s&secret=%s&site=%s&list_id=%s&data_col=%s&interval=%d",
		listName, TestTenantID, TestClientID, TestClientSecret, TestSiteID,
		serverConfig.ListID, serverConfig.DataColumn, intervalMs)
	clientAddr, err := ResolveSharePointAddr("sharepoint", clientURL)
	if err != nil {
		t.Fatalf("ResolveSharePointAddr: %v", err)
	}
	client, err := NewSharePointClient(clientAddr)
	if err != nil {
		t.Fatalf("NewSharePointClient: %v", err)
	}
	defer client.Close()

	// 预热: 发一个小包让 server 发现 client
	warmupData := []byte("warmup-throughput-test")
	warmupPkts := NewSimplexPacketWithMaxSize(warmupData, SimplexPacketTypeDATA, MaxSharePointMessageSize)
	client.Send(warmupPkts, clientAddr)

	warmupDeadline := time.After(60 * time.Second)
	for {
		select {
		case <-warmupDeadline:
			t.Fatal("Timeout waiting for SharePoint warmup")
		default:
		}
		server.pollSharePointList()
		pkt, _, _ := server.Receive()
		if pkt != nil {
			t.Logf("Warmup done, SharePoint connection established")
			break
		}
		time.Sleep(2 * time.Second)
	}

	// 预生成随机数据
	payload := make([]byte, packetSize)
	rand.Read(payload)

	var totalBytes int64
	var totalPkts int64
	var errors int64

	// 接收 goroutine
	var recvBytes int64
	var wg sync.WaitGroup
	stopRecv := make(chan struct{})
	wg.Add(1)
	go func() {
		defer wg.Done()
		for {
			select {
			case <-stopRecv:
				// drain
				for i := 0; i < 15; i++ {
					server.pollSharePointList()
					pkt, _, _ := server.Receive()
					if pkt != nil {
						atomic.AddInt64(&recvBytes, int64(len(pkt.Data)))
					}
					time.Sleep(1 * time.Second)
				}
				return
			default:
				server.pollSharePointList()
				pkt, _, _ := server.Receive()
				if pkt != nil {
					atomic.AddInt64(&recvBytes, int64(len(pkt.Data)))
				}
				time.Sleep(1 * time.Second) // SharePoint 不要太频繁 poll
			}
		}
	}()

	t.Logf("Starting 1-minute sustained send...")
	start := time.Now()
	deadline := start.Add(testDuration)

	for time.Now().Before(deadline) {
		pkts := NewSimplexPacketWithMaxSize(payload, SimplexPacketTypeDATA, MaxSharePointMessageSize)
		_, err := client.Send(pkts, clientAddr)
		if err != nil {
			errors++
			t.Logf("  [%v] Send error: %v", time.Since(start).Truncate(time.Second), err)
			time.Sleep(3 * time.Second)
			continue
		}
		totalBytes += int64(packetSize)
		totalPkts++
		elapsed := time.Since(start).Truncate(time.Second)
		t.Logf("  [%v] Sent packet #%d (%s), total: %s",
			elapsed, totalPkts, formatBytes(packetSize), formatBytes(int(totalBytes)))
	}

	t.Logf("Waiting for receiver to drain...")
	close(stopRecv)
	wg.Wait()

	duration := time.Since(start)
	result := &throughputResult{
		Transport:    "SharePoint",
		Direction:    "Client→Server",
		Duration:     duration,
		TotalBytes:   totalBytes,
		TotalPackets: totalPkts,
		Errors:       errors,
		PacketSize:   packetSize,
	}
	result.report(t)

	t.Logf("  Recv bytes:   %s (%.1f%% delivered)",
		formatBytes(int(recvBytes)),
		float64(recvBytes)/float64(totalBytes)*100)
}
