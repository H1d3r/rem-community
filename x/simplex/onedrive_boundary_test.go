//go:build graph
// +build graph

package simplex

import (
	"bytes"
	"crypto/rand"
	"crypto/sha256"
	"fmt"
	"testing"
	"time"
)

// ============================================================
// OneDrive Boundary Tests — 信道可靠性边界场景验证
//
// Mock 测试使用 FakeGraphAPIServer, Real 测试使用真实 Graph API.
//
// 运行 Mock 测试:
//   go test -v -tags graph -run "TestOneDriveBoundary_" ./x/simplex/ -timeout 120s
//
// 运行 Real 测试:
//   SHAREPOINT_REAL=1 go test -v -tags graph -run "TestOneDriveBoundary_" ./x/simplex/ -timeout 600s
// ============================================================

// --- Mock boundary test helpers ---

func oneDriveBoundarySetup(t *testing.T) (*OneDriveServer, *OneDriveClient, *SimplexAddr) {
	t.Helper()
	fake := NewFakeGraphAPIServer()
	fake.Install(t)
	prefix := "boundary-" + randomString(6)
	return oneDriveBoundarySetupWithPrefix(t, prefix, false)
}

func oneDriveBoundarySetupReal(t *testing.T) (*OneDriveServer, *OneDriveClient, *SimplexAddr) {
	t.Helper()
	skipIfNotReal(t)
	prefix := "rem-bd-" + randomString(6)
	return oneDriveBoundarySetupWithPrefix(t, prefix, true)
}

func oneDriveBoundarySetupWithPrefix(t *testing.T, prefix string, real bool) (*OneDriveServer, *OneDriveClient, *SimplexAddr) {
	t.Helper()
	var serverURLStr, clientURLStr string
	if real {
		serverURLStr = oneDriveRealServerURL(prefix)
		clientURLStr = oneDriveRealClientURL(prefix)
	} else {
		serverURLStr = oneDriveServerURL(prefix)
		clientURLStr = oneDriveClientURL(prefix)
	}

	server, err := NewOneDriveServer("onedrive", serverURLStr)
	if err != nil {
		t.Fatalf("NewOneDriveServer: %v", err)
	}
	t.Cleanup(func() { server.Close() })

	addr, err := ResolveOneDriveAddr("onedrive", clientURLStr)
	if err != nil {
		t.Fatalf("ResolveOneDriveAddr: %v", err)
	}

	client, err := NewOneDriveClient(addr)
	if err != nil {
		t.Fatalf("NewOneDriveClient: %v", err)
	}
	t.Cleanup(func() { client.Close() })

	return server, client, addr
}

func oneDriveBoundaryPollTimeout(real bool) time.Duration {
	if real {
		return 90 * time.Second
	}
	return 10 * time.Second
}

func oneDrivePollAllFromServer(t *testing.T, server *OneDriveServer, timeout time.Duration, count int) []string {
	t.Helper()
	deadline := time.Now().Add(timeout)
	var results []string
	for len(results) < count && time.Now().Before(deadline) {
		pkt, _, err := server.Receive()
		if err != nil {
			t.Fatalf("Server.Receive error: %v", err)
		}
		if pkt != nil {
			results = append(results, string(pkt.Data))
		} else {
			time.Sleep(100 * time.Millisecond)
		}
	}
	return results
}

func oneDrivePollAllFromClient(t *testing.T, client *OneDriveClient, timeout time.Duration, count int) []string {
	t.Helper()
	deadline := time.Now().Add(timeout)
	var results []string
	for len(results) < count && time.Now().Before(deadline) {
		pkt, _, err := client.Receive()
		if err != nil {
			t.Fatalf("Client.Receive error: %v", err)
		}
		if pkt != nil {
			results = append(results, string(pkt.Data))
		} else {
			time.Sleep(100 * time.Millisecond)
		}
	}
	return results
}

// ============================================================
// Mock-based boundary tests (FakeGraphAPIServer)
// ============================================================

// TestOneDriveBoundary_BurstSend_10Messages 突发发送 10 条消息
func TestOneDriveBoundary_BurstSend_10Messages(t *testing.T) {
	server, client, addr := oneDriveBoundarySetup(t)

	const msgCount = 10

	// 突发发送
	for i := 0; i < msgCount; i++ {
		msg := fmt.Sprintf("burst-%d", i)
		pkts := NewSimplexPacketWithMaxSize([]byte(msg), SimplexPacketTypeDATA, MaxOneDriveMessageSize)
		if _, err := client.Send(pkts, addr); err != nil {
			t.Fatalf("Send[%d] error: %v", i, err)
		}
	}

	// 收集结果
	results := oneDrivePollAllFromServer(t, server, 30*time.Second, msgCount)
	t.Logf("Sent: %d, Received: %d", msgCount, len(results))

	if len(results) < msgCount {
		t.Fatalf("Expected %d messages, got %d", msgCount, len(results))
	}

	received := make(map[string]bool)
	for _, r := range results {
		received[r] = true
	}
	for i := 0; i < msgCount; i++ {
		if !received[fmt.Sprintf("burst-%d", i)] {
			t.Errorf("Missing message burst-%d", i)
		}
	}
	t.Log("PASS: all burst messages received")
}

// TestOneDriveBoundary_LargePayload_50KB 传输 50KB 数据校验完整性
func TestOneDriveBoundary_LargePayload_50KB(t *testing.T) {
	server, client, addr := oneDriveBoundarySetup(t)

	payload := make([]byte, 50*1024)
	rand.Read(payload)
	expectedHash := sha256.Sum256(payload)

	pkts := NewSimplexPacketWithMaxSize(payload, SimplexPacketTypeDATA, MaxOneDriveMessageSize)
	if _, err := client.Send(pkts, addr); err != nil {
		t.Fatalf("Send error: %v", err)
	}
	t.Logf("Sent %d bytes, fragmented into %d packets", len(payload), len(pkts.Packets))

	// 收集所有分片
	var assembled []byte
	deadline := time.Now().Add(30 * time.Second)
	for time.Now().Before(deadline) {
		pkt, _, err := server.Receive()
		if err != nil {
			t.Fatalf("Receive error: %v", err)
		}
		if pkt != nil {
			assembled = append(assembled, pkt.Data...)
			if len(assembled) >= len(payload) {
				break
			}
		} else {
			time.Sleep(100 * time.Millisecond)
		}
	}

	gotHash := sha256.Sum256(assembled)
	if expectedHash != gotHash {
		t.Fatalf("SHA256 mismatch: payload corrupted (expected %d bytes, got %d)", len(payload), len(assembled))
	}
	t.Logf("PASS: %d bytes transferred with zero corruption", len(payload))
}

// TestOneDriveBoundary_OrderGuarantee_Sequential 顺序保证
func TestOneDriveBoundary_OrderGuarantee_Sequential(t *testing.T) {
	server, client, addr := oneDriveBoundarySetup(t)

	const msgCount = 10
	for i := 0; i < msgCount; i++ {
		msg := fmt.Sprintf("seq-%03d", i)
		pkts := NewSimplexPacketWithMaxSize([]byte(msg), SimplexPacketTypeDATA, MaxOneDriveMessageSize)
		if _, err := client.Send(pkts, addr); err != nil {
			t.Fatalf("Send[%d] error: %v", i, err)
		}
	}

	results := oneDrivePollAllFromServer(t, server, 30*time.Second, msgCount)
	t.Logf("Received: %v", results)

	if len(results) < msgCount {
		t.Fatalf("Expected %d messages, got %d", msgCount, len(results))
	}

	for i := 0; i < msgCount; i++ {
		expected := fmt.Sprintf("seq-%03d", i)
		if results[i] != expected {
			t.Fatalf("Order mismatch at %d: got %q, want %q", i, results[i], expected)
		}
	}
	t.Log("PASS: all messages received in correct order")
}

// TestOneDriveBoundary_BidirectionalConcurrent 并发双向通信
func TestOneDriveBoundary_BidirectionalConcurrent(t *testing.T) {
	server, client, addr := oneDriveBoundarySetup(t)

	const rounds = 5

	// Client → Server
	for i := 0; i < rounds; i++ {
		msg := fmt.Sprintf("c2s-%d", i)
		pkts := NewSimplexPacketWithMaxSize([]byte(msg), SimplexPacketTypeDATA, MaxOneDriveMessageSize)
		if _, err := client.Send(pkts, addr); err != nil {
			t.Fatalf("Client.Send[%d] error: %v", i, err)
		}
	}

	// 等待 server 收到第一条消息以获取 recvAddr
	var recvAddr *SimplexAddr
	deadline := time.Now().Add(15 * time.Second)
	serverGot := make(map[string]bool)
	for time.Now().Before(deadline) {
		pkt, a, _ := server.Receive()
		if pkt != nil {
			serverGot[string(pkt.Data)] = true
			if recvAddr == nil {
				recvAddr = a
			}
			if len(serverGot) >= rounds {
				break
			}
		} else {
			time.Sleep(50 * time.Millisecond)
		}
	}

	if recvAddr == nil {
		t.Fatal("Server never received any message")
	}

	// Server → Client
	for i := 0; i < rounds; i++ {
		msg := fmt.Sprintf("s2c-%d", i)
		pkts := NewSimplexPacketWithMaxSize([]byte(msg), SimplexPacketTypeDATA, MaxOneDriveMessageSize)
		if _, err := server.Send(pkts, recvAddr); err != nil {
			t.Fatalf("Server.Send[%d] error: %v", i, err)
		}
	}

	clientGot := make(map[string]bool)
	deadline = time.Now().Add(15 * time.Second)
	for time.Now().Before(deadline) {
		pkt, _, _ := client.Receive()
		if pkt != nil {
			clientGot[string(pkt.Data)] = true
			if len(clientGot) >= rounds {
				break
			}
		} else {
			time.Sleep(50 * time.Millisecond)
		}
	}

	t.Logf("Server received (%d/%d): %v", len(serverGot), rounds, serverGot)
	t.Logf("Client received (%d/%d): %v", len(clientGot), rounds, clientGot)

	if len(serverGot) < rounds {
		t.Errorf("Server only got %d/%d", len(serverGot), rounds)
	}
	if len(clientGot) < rounds {
		t.Errorf("Client only got %d/%d", len(clientGot), rounds)
	}
	t.Log("PASS: bidirectional concurrent communication")
}

// TestOneDriveBoundary_MultiClient_ConcurrentWrite 多客户端并发写入
func TestOneDriveBoundary_MultiClient_ConcurrentWrite(t *testing.T) {
	fake := NewFakeGraphAPIServer()
	fake.Install(t)
	prefix := "boundary-mc-" + randomString(6)

	server, err := NewOneDriveServer("onedrive", oneDriveServerURL(prefix))
	if err != nil {
		t.Fatalf("NewOneDriveServer: %v", err)
	}
	defer server.Close()

	const numClients = 3
	const msgsPerClient = 3

	// 创建所有客户端并发送消息
	clients := make([]*OneDriveClient, numClients)
	for c := 0; c < numClients; c++ {
		clientURL := fmt.Sprintf("%s&client_id=cli%d", oneDriveClientURL(prefix), c)
		cAddr, _ := ResolveOneDriveAddr("onedrive", clientURL)
		cli, err := NewOneDriveClient(cAddr)
		if err != nil {
			t.Fatalf("NewOneDriveClient[%d]: %v", c, err)
		}
		clients[c] = cli

		for j := 0; j < msgsPerClient; j++ {
			msg := fmt.Sprintf("client%d-msg%d", c, j)
			pkts := NewSimplexPacketWithMaxSize([]byte(msg), SimplexPacketTypeDATA, MaxOneDriveMessageSize)
			if _, err := cli.Send(pkts, cAddr); err != nil {
				t.Errorf("Client[%d].Send[%d]: %v", c, j, err)
			}
		}
	}
	defer func() {
		for _, cli := range clients {
			cli.Close()
		}
	}()

	totalExpected := numClients * msgsPerClient
	results := oneDrivePollAllFromServer(t, server, 30*time.Second, totalExpected)
	t.Logf("Total received: %d/%d", len(results), totalExpected)

	received := make(map[string]bool)
	for _, r := range results {
		received[r] = true
	}

	missing := 0
	for c := 0; c < numClients; c++ {
		for j := 0; j < msgsPerClient; j++ {
			key := fmt.Sprintf("client%d-msg%d", c, j)
			if !received[key] {
				t.Logf("  missing: %s", key)
				missing++
			}
		}
	}

	if missing > 0 {
		t.Fatalf("Missing %d/%d messages", missing, totalExpected)
	}
	t.Logf("PASS: all %d messages received from %d concurrent clients", totalExpected, numClients)
}

// TestOneDriveBoundary_LargePayload_SHA256_Integrity 大数据 + SHA256 校验
func TestOneDriveBoundary_LargePayload_SHA256_Integrity(t *testing.T) {
	server, client, addr := oneDriveBoundarySetup(t)

	// 发送多个不同大小的包
	sizes := []int{1024, 10240, 32768}
	for _, size := range sizes {
		data := make([]byte, size)
		rand.Read(data)
		expected := sha256.Sum256(data)

		pkts := NewSimplexPacketWithMaxSize(data, SimplexPacketTypeDATA, MaxOneDriveMessageSize)
		if _, err := client.Send(pkts, addr); err != nil {
			t.Fatalf("Send[%d] error: %v", size, err)
		}

		var assembled []byte
		deadline := time.Now().Add(15 * time.Second)
		for time.Now().Before(deadline) {
			pkt, _, _ := server.Receive()
			if pkt != nil {
				assembled = append(assembled, pkt.Data...)
				if len(assembled) >= size {
					break
				}
			} else {
				time.Sleep(50 * time.Millisecond)
			}
		}

		got := sha256.Sum256(assembled)
		if expected != got {
			t.Fatalf("SHA256 mismatch for %d bytes", size)
		}
		t.Logf("  %d bytes: SHA256 OK", size)
	}
	t.Log("PASS: all sizes passed SHA256 integrity check")
}

// TestOneDriveBoundary_LargePayload_100KB 传输 100KB 数据校验完整性
func TestOneDriveBoundary_LargePayload_100KB(t *testing.T) {
	server, client, addr := oneDriveBoundarySetup(t)

	payload := make([]byte, 100*1024)
	rand.Read(payload)
	expectedHash := sha256.Sum256(payload)

	pkts := NewSimplexPacketWithMaxSize(payload, SimplexPacketTypeDATA, MaxOneDriveMessageSize)
	if _, err := client.Send(pkts, addr); err != nil {
		t.Fatalf("Send error: %v", err)
	}
	t.Logf("Sent %d bytes, fragmented into %d packets", len(payload), len(pkts.Packets))

	var assembled []byte
	deadline := time.Now().Add(30 * time.Second)
	for time.Now().Before(deadline) {
		pkt, _, err := server.Receive()
		if err != nil {
			t.Fatalf("Receive error: %v", err)
		}
		if pkt != nil {
			assembled = append(assembled, pkt.Data...)
			if len(assembled) >= len(payload) {
				break
			}
		} else {
			time.Sleep(100 * time.Millisecond)
		}
	}

	if len(assembled) < len(payload) {
		t.Fatalf("Timeout: only received %d/%d bytes", len(assembled), len(payload))
	}

	gotHash := sha256.Sum256(assembled[:len(payload)])
	if expectedHash != gotHash {
		t.Fatalf("SHA256 mismatch: 100KB payload corrupted (expected %d bytes, got %d)", len(payload), len(assembled))
	}
	t.Logf("PASS: %d bytes transferred with zero corruption", len(payload))
}

// TestOneDriveBoundary_LargePayload_1MB 传输 1MB 数据校验完整性
func TestOneDriveBoundary_LargePayload_1MB(t *testing.T) {
	server, client, addr := oneDriveBoundarySetup(t)

	dataSize := 1024 * 1024 // 1MB
	payload := make([]byte, dataSize)
	rand.Read(payload)
	expectedHash := sha256.Sum256(payload)

	pkts := NewSimplexPacketWithMaxSize(payload, SimplexPacketTypeDATA, MaxOneDriveMessageSize)
	if _, err := client.Send(pkts, addr); err != nil {
		t.Fatalf("Send error: %v", err)
	}
	t.Logf("Sent %d bytes (1MB), fragmented into %d packets", len(payload), len(pkts.Packets))

	var assembled []byte
	deadline := time.Now().Add(60 * time.Second)
	for time.Now().Before(deadline) {
		pkt, _, err := server.Receive()
		if err != nil {
			t.Fatalf("Receive error: %v", err)
		}
		if pkt != nil {
			assembled = append(assembled, pkt.Data...)
			if len(assembled) >= dataSize {
				break
			}
		} else {
			time.Sleep(100 * time.Millisecond)
		}
	}

	if len(assembled) < dataSize {
		t.Fatalf("Timeout: only received %d/%d bytes", len(assembled), dataSize)
	}

	gotHash := sha256.Sum256(assembled[:dataSize])
	if expectedHash != gotHash {
		t.Fatalf("SHA256 mismatch: 1MB payload corrupted")
	}
	t.Logf("PASS: 1MB (%d bytes) transferred with SHA256 integrity", dataSize)
}

// TestOneDriveBoundary_LargePayload_1MB_Bidirectional 1MB 双向传输
func TestOneDriveBoundary_LargePayload_1MB_Bidirectional(t *testing.T) {
	server, client, addr := oneDriveBoundarySetup(t)

	dataSize := 1024 * 1024
	c2sData := make([]byte, dataSize)
	rand.Read(c2sData)
	c2sHash := sha256.Sum256(c2sData)

	// Client → Server: 1MB
	pkts := NewSimplexPacketWithMaxSize(c2sData, SimplexPacketTypeDATA, MaxOneDriveMessageSize)
	if _, err := client.Send(pkts, addr); err != nil {
		t.Fatalf("Client.Send error: %v", err)
	}
	t.Logf("Client sent 1MB (%d packets)", len(pkts.Packets))

	var c2sAssembled []byte
	var recvAddr *SimplexAddr
	deadline := time.Now().Add(60 * time.Second)
	for time.Now().Before(deadline) {
		pkt, a, err := server.Receive()
		if err != nil {
			t.Fatalf("Server.Receive error: %v", err)
		}
		if pkt != nil {
			c2sAssembled = append(c2sAssembled, pkt.Data...)
			if recvAddr == nil {
				recvAddr = a
			}
			if len(c2sAssembled) >= dataSize {
				break
			}
		} else {
			time.Sleep(100 * time.Millisecond)
		}
	}

	if len(c2sAssembled) < dataSize {
		t.Fatalf("C2S timeout: only %d/%d bytes", len(c2sAssembled), dataSize)
	}
	gotC2S := sha256.Sum256(c2sAssembled[:dataSize])
	if c2sHash != gotC2S {
		t.Fatalf("C2S SHA256 mismatch")
	}
	t.Log("C2S 1MB: SHA256 OK")

	// Server → Client: 1MB
	s2cData := make([]byte, dataSize)
	rand.Read(s2cData)
	s2cHash := sha256.Sum256(s2cData)

	respPkts := NewSimplexPacketWithMaxSize(s2cData, SimplexPacketTypeDATA, MaxOneDriveMessageSize)
	if _, err := server.Send(respPkts, recvAddr); err != nil {
		t.Fatalf("Server.Send error: %v", err)
	}
	t.Logf("Server sent 1MB (%d packets)", len(respPkts.Packets))

	var s2cAssembled []byte
	deadline = time.Now().Add(60 * time.Second)
	for time.Now().Before(deadline) {
		pkt, _, err := client.Receive()
		if err != nil {
			t.Fatalf("Client.Receive error: %v", err)
		}
		if pkt != nil {
			s2cAssembled = append(s2cAssembled, pkt.Data...)
			if len(s2cAssembled) >= dataSize {
				break
			}
		} else {
			time.Sleep(100 * time.Millisecond)
		}
	}

	if len(s2cAssembled) < dataSize {
		t.Fatalf("S2C timeout: only %d/%d bytes", len(s2cAssembled), dataSize)
	}
	gotS2C := sha256.Sum256(s2cAssembled[:dataSize])
	if s2cHash != gotS2C {
		t.Fatalf("S2C SHA256 mismatch")
	}
	t.Log("PASS: 1MB bidirectional transfer with SHA256 integrity")
}

// TestOneDriveBoundary_ServerReceive_IteratesClients 验证 Receive 遍历所有客户端
func TestOneDriveBoundary_ServerReceive_IteratesClients(t *testing.T) {
	fake := NewFakeGraphAPIServer()
	fake.Install(t)
	prefix := "boundary-iter-" + randomString(6)

	server, err := NewOneDriveServer("onedrive", oneDriveServerURL(prefix))
	if err != nil {
		t.Fatalf("NewOneDriveServer: %v", err)
	}
	defer server.Close()

	// 创建 2 个客户端各发 1 条
	for i := 0; i < 2; i++ {
		clientURL := fmt.Sprintf("%s&client_id=iter%d", oneDriveClientURL(prefix), i)
		cAddr, _ := ResolveOneDriveAddr("onedrive", clientURL)
		cli, err := NewOneDriveClient(cAddr)
		if err != nil {
			t.Fatalf("NewOneDriveClient[%d]: %v", i, err)
		}
		msg := fmt.Sprintf("client-%d", i)
		pkts := NewSimplexPacketWithMaxSize([]byte(msg), SimplexPacketTypeDATA, MaxOneDriveMessageSize)
		cli.Send(pkts, cAddr)
		defer cli.Close()
	}

	results := oneDrivePollAllFromServer(t, server, 15*time.Second, 2)
	if len(results) < 2 {
		t.Fatalf("Expected 2 messages from 2 clients, got %d", len(results))
	}
	t.Logf("PASS: server received from multiple clients: %v", results)
}

// TestOneDriveBoundary_CloseWhilePolling 关闭时 polling 正常退出
func TestOneDriveBoundary_CloseWhilePolling(t *testing.T) {
	fake := NewFakeGraphAPIServer()
	fake.Install(t)
	prefix := "boundary-close-" + randomString(6)

	server, err := NewOneDriveServer("onedrive", oneDriveServerURL(prefix))
	if err != nil {
		t.Fatalf("NewOneDriveServer: %v", err)
	}

	// 让 polling 运行一段时间后关闭
	time.Sleep(200 * time.Millisecond)
	if err := server.Close(); err != nil {
		t.Fatalf("Server.Close error: %v", err)
	}

	// Receive 应该返回 closed 错误
	pkt, _, err := server.Receive()
	if pkt != nil {
		t.Errorf("Expected nil packet after close, got data")
	}
	// err 可能是 ErrClosedPipe 或 nil（取决于 select 时序）
	t.Logf("PASS: server closed cleanly, Receive returned: pkt=%v err=%v", pkt, err)
}

// ============================================================
// Real API boundary tests (requires SHAREPOINT_REAL=1)
// ============================================================

// TestOneDriveBoundary_Real_BurstSend_10Messages 突发发送 (真实 API)
func TestOneDriveBoundary_Real_BurstSend_10Messages(t *testing.T) {
	server, client, addr := oneDriveBoundarySetupReal(t)

	const msgCount = 10
	t.Logf("Phase 1: burst-sending %d messages", msgCount)
	for i := 0; i < msgCount; i++ {
		msg := fmt.Sprintf("burst-%d", i)
		pkts := NewSimplexPacketWithMaxSize([]byte(msg), SimplexPacketTypeDATA, MaxOneDriveMessageSize)
		if _, err := client.Send(pkts, addr); err != nil {
			t.Fatalf("Send[%d] error: %v", i, err)
		}
	}

	t.Log("Phase 2: collecting messages...")
	results := oneDrivePollAllFromServer(t, server, 90*time.Second, msgCount)
	t.Logf("Sent: %d, Received: %d", msgCount, len(results))

	received := make(map[string]bool)
	for _, r := range results {
		received[r] = true
		t.Logf("  received: %q", r)
	}

	if len(results) < msgCount {
		t.Fatalf("Expected %d messages, got %d", msgCount, len(results))
	}
	t.Log("PASS: all burst messages received — no data loss")
}

// TestOneDriveBoundary_Real_LargePayload_50KB 大包传输 (真实 API)
func TestOneDriveBoundary_Real_LargePayload_50KB(t *testing.T) {
	server, client, addr := oneDriveBoundarySetupReal(t)

	payload := make([]byte, 50*1024)
	rand.Read(payload)
	expectedHash := sha256.Sum256(payload)

	pkts := NewSimplexPacketWithMaxSize(payload, SimplexPacketTypeDATA, MaxOneDriveMessageSize)
	t.Logf("Phase 1: sending %d bytes, fragmented into %d packets", len(payload), len(pkts.Packets))
	if _, err := client.Send(pkts, addr); err != nil {
		t.Fatalf("Send error: %v", err)
	}

	t.Log("Phase 2: receiving...")
	var assembled []byte
	deadline := time.Now().Add(90 * time.Second)
	for time.Now().Before(deadline) {
		pkt, _, err := server.Receive()
		if err != nil {
			t.Fatalf("Receive error: %v", err)
		}
		if pkt != nil {
			assembled = append(assembled, pkt.Data...)
			if len(assembled) >= len(payload) {
				break
			}
		} else {
			time.Sleep(500 * time.Millisecond)
		}
	}

	gotHash := sha256.Sum256(assembled)
	if !bytes.Equal(expectedHash[:], gotHash[:]) {
		t.Fatalf("SHA256 mismatch: expected %d bytes, got %d", len(payload), len(assembled))
	}
	t.Logf("PASS: %d bytes transferred with zero corruption", len(payload))
}

// TestOneDriveBoundary_Real_OrderGuarantee_Sequential 顺序保证 (真实 API)
func TestOneDriveBoundary_Real_OrderGuarantee_Sequential(t *testing.T) {
	server, client, addr := oneDriveBoundarySetupReal(t)

	const msgCount = 10
	t.Logf("Phase 1: sending %d messages sequentially", msgCount)
	for i := 0; i < msgCount; i++ {
		msg := fmt.Sprintf("seq-%03d", i)
		pkts := NewSimplexPacketWithMaxSize([]byte(msg), SimplexPacketTypeDATA, MaxOneDriveMessageSize)
		if _, err := client.Send(pkts, addr); err != nil {
			t.Fatalf("Send[%d] error: %v", i, err)
		}
	}

	t.Log("Phase 2: collecting in order...")
	results := oneDrivePollAllFromServer(t, server, 90*time.Second, msgCount)
	t.Logf("Received: %v", results)

	if len(results) < msgCount {
		t.Fatalf("Expected %d, got %d", msgCount, len(results))
	}
	for i := 0; i < msgCount; i++ {
		expected := fmt.Sprintf("seq-%03d", i)
		if results[i] != expected {
			t.Fatalf("Order mismatch at %d: got %q, want %q", i, results[i], expected)
		}
	}
	t.Log("PASS: all messages received in correct order")
}

// TestOneDriveBoundary_Real_BidirectionalConcurrent 双向并发 (真实 API)
func TestOneDriveBoundary_Real_BidirectionalConcurrent(t *testing.T) {
	server, client, addr := oneDriveBoundarySetupReal(t)

	const rounds = 5

	// Client → Server
	t.Logf("Phase 1: %d rounds of concurrent bidirectional", rounds)
	for i := 0; i < rounds; i++ {
		msg := fmt.Sprintf("c2s-%d", i)
		pkts := NewSimplexPacketWithMaxSize([]byte(msg), SimplexPacketTypeDATA, MaxOneDriveMessageSize)
		client.Send(pkts, addr)
	}

	// 收集 server 侧
	var recvAddr *SimplexAddr
	serverGot := make(map[string]bool)
	deadline := time.Now().Add(90 * time.Second)
	for time.Now().Before(deadline) && len(serverGot) < rounds {
		pkt, a, _ := server.Receive()
		if pkt != nil {
			serverGot[string(pkt.Data)] = true
			if recvAddr == nil {
				recvAddr = a
			}
		} else {
			time.Sleep(500 * time.Millisecond)
		}
	}

	if recvAddr == nil {
		t.Fatal("Server never received any message")
	}

	// Server → Client
	for i := 0; i < rounds; i++ {
		msg := fmt.Sprintf("s2c-%d", i)
		pkts := NewSimplexPacketWithMaxSize([]byte(msg), SimplexPacketTypeDATA, MaxOneDriveMessageSize)
		server.Send(pkts, recvAddr)
	}

	clientGot := make(map[string]bool)
	deadline = time.Now().Add(90 * time.Second)
	for time.Now().Before(deadline) && len(clientGot) < rounds {
		pkt, _, _ := client.Receive()
		if pkt != nil {
			clientGot[string(pkt.Data)] = true
		} else {
			time.Sleep(500 * time.Millisecond)
		}
	}

	t.Logf("Server received (%d/%d): %v", len(serverGot), rounds, serverGot)
	t.Logf("Client received (%d/%d): %v", len(clientGot), rounds, clientGot)

	if len(serverGot) < rounds || len(clientGot) < rounds {
		t.Fatalf("Incomplete: server=%d/%d client=%d/%d", len(serverGot), rounds, len(clientGot), rounds)
	}
	t.Log("PASS: bidirectional concurrent communication over real API")
}

// TestOneDriveBoundary_Real_MultiClient_ConcurrentWrite 多客户端并发写入 (真实 API)
func TestOneDriveBoundary_Real_MultiClient_ConcurrentWrite(t *testing.T) {
	skipIfNotReal(t)

	prefix := "rem-bd-mc-" + randomString(6)
	server, err := NewOneDriveServer("onedrive", oneDriveRealServerURL(prefix))
	if err != nil {
		t.Fatalf("NewOneDriveServer: %v", err)
	}
	defer server.Close()

	const numClients = 3
	const msgsPerClient = 3

	t.Logf("Phase 1: %d clients each send %d messages concurrently", numClients, msgsPerClient)
	// 创建客户端并发送消息（不在 goroutine 中 close，让 monitoring 有时间 tickWrite）
	clients := make([]*OneDriveClient, numClients)
	for c := 0; c < numClients; c++ {
		cAddr, _ := ResolveOneDriveAddr("onedrive", oneDriveRealClientURL(prefix))
		cli, err := NewOneDriveClient(cAddr)
		if err != nil {
			t.Fatalf("NewOneDriveClient[%d]: %v", c, err)
		}
		clients[c] = cli

		for j := 0; j < msgsPerClient; j++ {
			msg := fmt.Sprintf("client%d-msg%d", c, j)
			pkts := NewSimplexPacketWithMaxSize([]byte(msg), SimplexPacketTypeDATA, MaxOneDriveMessageSize)
			cli.Send(pkts, cAddr)
		}
	}
	defer func() {
		for _, cli := range clients {
			cli.Close()
		}
	}()
	t.Log("Phase 1: all sends complete")

	totalExpected := numClients * msgsPerClient
	results := oneDrivePollAllFromServer(t, server, 120*time.Second, totalExpected)
	t.Logf("Total received: %d/%d", len(results), totalExpected)

	received := make(map[string]bool)
	for _, r := range results {
		received[r] = true
		t.Logf("  received: %q", r)
	}

	if len(received) < totalExpected {
		t.Fatalf("Expected %d unique messages, got %d", totalExpected, len(received))
	}
	t.Logf("PASS: all %d messages received from %d concurrent clients", totalExpected, numClients)
}
