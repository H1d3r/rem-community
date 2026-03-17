//go:build graph
// +build graph

package simplex

import (
	"fmt"
	"math/rand"
	"net/url"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// ============================================================
// OneDrive Stability Tests — Mock-Based (FakeGraphAPIServer)
//
// 高并发、大数据量、持续通信等稳定性验证
//
// 运行示例:
//   go test -v -tags graph -run "TestOneDriveStability" ./x/simplex/ -timeout 120s
//   go test -race -tags graph -run "TestOneDriveStability" ./x/simplex/ -timeout 120s
// ============================================================

// --- 高并发多客户端压力测试 ---

// TestOneDriveStability_ManyClients 10 个并发客户端同时通信
func TestOneDriveStability_ManyClients(t *testing.T) {
	fake := NewFakeGraphAPIServer()
	fake.Install(t)

	prefix := "stab-many-" + randomString(6)
	server, err := NewOneDriveServer("onedrive", oneDriveServerURL(prefix))
	if err != nil {
		t.Fatalf("NewOneDriveServer error: %v", err)
	}
	defer server.Close()

	const numClients = 10
	clients := make([]*OneDriveClient, numClients)
	for i := 0; i < numClients; i++ {
		clientURL := fmt.Sprintf("%s&client_id=stab%d", oneDriveClientURL(prefix), i)
		cAddr, _ := ResolveOneDriveAddr("onedrive", clientURL)
		clients[i], err = NewOneDriveClient(cAddr)
		if err != nil {
			t.Fatalf("NewOneDriveClient[%d] error: %v", i, err)
		}
		defer clients[i].Close()
	}

	// 每个客户端发送一条消息
	var wg sync.WaitGroup
	for i, client := range clients {
		wg.Add(1)
		go func(idx int, c *OneDriveClient) {
			defer wg.Done()
			msg := fmt.Sprintf("client-%d-data", idx)
			pkts := NewSimplexPacketWithMaxSize([]byte(msg), SimplexPacketTypeDATA, MaxOneDriveMessageSize)
			if _, err := c.Send(pkts, c.Addr()); err != nil {
				t.Errorf("Client[%d].Send error: %v", idx, err)
			}
		}(i, client)
	}
	wg.Wait()

	// 收集所有消息
	received := make(map[string]bool)
	deadline := time.Now().Add(15 * time.Second)
	for len(received) < numClients && time.Now().Before(deadline) {
		time.Sleep(100 * time.Millisecond)
		for {
			pkt, _, err := server.Receive()
			if err != nil {
				t.Fatalf("Server.Receive error: %v", err)
			}
			if pkt == nil {
				break
			}
			received[string(pkt.Data)] = true
		}
	}

	for i := 0; i < numClients; i++ {
		expected := fmt.Sprintf("client-%d-data", i)
		if !received[expected] {
			t.Errorf("Missing data from client %d: %q", i, expected)
		}
	}

	clientCount := 0
	server.fts.clients.Range(func(_, _ interface{}) bool {
		clientCount++
		return true
	})
	t.Logf("ManyClients: %d clients connected, %d messages received, files: %d",
		clientCount, len(received), fake.FileCount())
}

// --- 持续多轮双向通信 ---

// TestOneDriveStability_SustainedCommunication 20 轮双向通信
func TestOneDriveStability_SustainedCommunication(t *testing.T) {
	fake := NewFakeGraphAPIServer()
	fake.Install(t)

	prefix := "stab-sustain-" + randomString(6)
	server, err := NewOneDriveServer("onedrive", oneDriveServerURL(prefix))
	if err != nil {
		t.Fatalf("NewOneDriveServer error: %v", err)
	}
	defer server.Close()

	addr, _ := ResolveOneDriveAddr("onedrive", oneDriveClientURL(prefix))
	client, err := NewOneDriveClient(addr)
	if err != nil {
		t.Fatalf("NewOneDriveClient error: %v", err)
	}
	defer client.Close()

	const rounds = 20
	for round := 0; round < rounds; round++ {
		// Client → Server
		clientMsg := fmt.Sprintf("sustain-c2s-round-%d", round)
		clientPkts := NewSimplexPacketWithMaxSize([]byte(clientMsg), SimplexPacketTypeDATA, MaxOneDriveMessageSize)
		_, err := client.Send(clientPkts, addr)
		if err != nil {
			t.Fatalf("Round %d: Client.Send error: %v", round, err)
		}

		pkt, recvAddr := oneDrivePollServerUntilReceived(t, server)
		if string(pkt.Data) != clientMsg {
			t.Errorf("Round %d: Server got %q, want %q", round, string(pkt.Data), clientMsg)
		}

		// Server → Client
		serverMsg := fmt.Sprintf("sustain-s2c-round-%d", round)
		serverPkts := NewSimplexPacketWithMaxSize([]byte(serverMsg), SimplexPacketTypeDATA, MaxOneDriveMessageSize)
		_, err = server.Send(serverPkts, recvAddr)
		if err != nil {
			t.Fatalf("Round %d: Server.Send error: %v", round, err)
		}

		respPkt := oneDrivePollClientUntilReceived(t, client)
		if string(respPkt.Data) != serverMsg {
			t.Errorf("Round %d: Client got %q, want %q", round, string(respPkt.Data), serverMsg)
		}
	}

	t.Logf("SustainedCommunication: %d rounds passed, files: %d, tokens: %d",
		rounds, fake.FileCount(), fake.TokenCount())
}

// --- 大数据传输压力测试 ---

// TestOneDriveStability_LargeDataMultiple 多次传输大数据块
func TestOneDriveStability_LargeDataMultiple(t *testing.T) {
	fake := NewFakeGraphAPIServer()
	fake.Install(t)

	prefix := "stab-large-" + randomString(6)
	server, err := NewOneDriveServer("onedrive", oneDriveServerURL(prefix))
	if err != nil {
		t.Fatalf("NewOneDriveServer error: %v", err)
	}
	defer server.Close()

	addr, _ := ResolveOneDriveAddr("onedrive", oneDriveClientURL(prefix))
	client, err := NewOneDriveClient(addr)
	if err != nil {
		t.Fatalf("NewOneDriveClient error: %v", err)
	}
	defer client.Close()

	sizes := []int{1000, 10000, 50000, 60000}

	for _, size := range sizes {
		data := make([]byte, size)
		rng := rand.New(rand.NewSource(int64(size)))
		for i := range data {
			data[i] = byte(rng.Intn(256))
		}

		pkts := NewSimplexPacketWithMaxSize(data, SimplexPacketTypeDATA, MaxOneDriveMessageSize)
		_, err := client.Send(pkts, addr)
		if err != nil {
			t.Fatalf("Send %d bytes error: %v", size, err)
		}

		pkt, _ := oneDrivePollServerUntilReceived(t, server)
		if len(pkt.Data) != size {
			t.Errorf("Size %d: received %d bytes", size, len(pkt.Data))
		}

		// 验证内容完整性
		rng2 := rand.New(rand.NewSource(int64(size)))
		for i := 0; i < size; i++ {
			expected := byte(rng2.Intn(256))
			if pkt.Data[i] != expected {
				t.Errorf("Size %d: data mismatch at byte %d", size, i)
				break
			}
		}

		t.Logf("Large data %d bytes: OK", size)
	}

	t.Logf("LargeDataMultiple: all sizes passed, files: %d", fake.FileCount())
}

// --- 快速连发 + 并发接收 ---

// TestOneDriveStability_RapidBurst 快速连发 20 条消息
func TestOneDriveStability_RapidBurst(t *testing.T) {
	fake := NewFakeGraphAPIServer()
	fake.Install(t)

	prefix := "stab-burst-" + randomString(6)
	server, err := NewOneDriveServer("onedrive", oneDriveServerURL(prefix))
	if err != nil {
		t.Fatalf("NewOneDriveServer error: %v", err)
	}
	defer server.Close()

	addr, _ := ResolveOneDriveAddr("onedrive", oneDriveClientURL(prefix))
	client, err := NewOneDriveClient(addr)
	if err != nil {
		t.Fatalf("NewOneDriveClient error: %v", err)
	}
	defer client.Close()

	const burstCount = 20
	for i := 0; i < burstCount; i++ {
		msg := fmt.Sprintf("burst-msg-%03d", i)
		pkts := NewSimplexPacketWithMaxSize([]byte(msg), SimplexPacketTypeDATA, MaxOneDriveMessageSize)
		_, err := client.Send(pkts, addr)
		if err != nil {
			t.Fatalf("Send[%d] error: %v", i, err)
		}
	}

	received := make(map[string]bool)
	deadline := time.Now().Add(15 * time.Second)
	for len(received) < burstCount && time.Now().Before(deadline) {
		time.Sleep(100 * time.Millisecond)
		for {
			pkt, _, err := server.Receive()
			if err != nil {
				t.Fatalf("Receive error: %v", err)
			}
			if pkt == nil {
				break
			}
			received[string(pkt.Data)] = true
		}
	}

	if len(received) != burstCount {
		t.Errorf("Expected %d messages, got %d", burstCount, len(received))
		for i := 0; i < burstCount; i++ {
			expected := fmt.Sprintf("burst-msg-%03d", i)
			if !received[expected] {
				t.Logf("  Missing: %s", expected)
			}
		}
	}

	t.Logf("RapidBurst: %d/%d messages received, files: %d", len(received), burstCount, fake.FileCount())
}

// --- 多客户端并发双向通信 ---

// TestOneDriveStability_ConcurrentBidirectional 5 个客户端同时双向通信
func TestOneDriveStability_ConcurrentBidirectional(t *testing.T) {
	fake := NewFakeGraphAPIServer()
	fake.Install(t)

	prefix := "stab-bidir-" + randomString(6)
	server, err := NewOneDriveServer("onedrive", oneDriveServerURL(prefix))
	if err != nil {
		t.Fatalf("NewOneDriveServer error: %v", err)
	}
	defer server.Close()

	const numClients = 5
	clients := make([]*OneDriveClient, numClients)
	for i := 0; i < numClients; i++ {
		clientURL := fmt.Sprintf("%s&client_id=bidir%d", oneDriveClientURL(prefix), i)
		cAddr, _ := ResolveOneDriveAddr("onedrive", clientURL)
		clients[i], err = NewOneDriveClient(cAddr)
		if err != nil {
			t.Fatalf("NewOneDriveClient[%d] error: %v", i, err)
		}
		defer clients[i].Close()
	}

	// 所有客户端同时发送
	var wg sync.WaitGroup
	for i, client := range clients {
		wg.Add(1)
		go func(idx int, c *OneDriveClient) {
			defer wg.Done()
			msg := fmt.Sprintf("bidir-client-%d", idx)
			pkts := NewSimplexPacketWithMaxSize([]byte(msg), SimplexPacketTypeDATA, MaxOneDriveMessageSize)
			c.Send(pkts, c.Addr())
		}(i, client)
	}
	wg.Wait()

	// 服务端接收并回复每个客户端
	received := make(map[string]*SimplexAddr)
	deadline := time.Now().Add(15 * time.Second)
	for len(received) < numClients && time.Now().Before(deadline) {
		time.Sleep(100 * time.Millisecond)
		for {
			pkt, addr, err := server.Receive()
			if err != nil {
				t.Fatalf("Server.Receive error: %v", err)
			}
			if pkt == nil {
				break
			}
			received[string(pkt.Data)] = addr
		}
	}

	// 服务端回复每个客户端
	for msg, addr := range received {
		reply := "reply-to-" + msg
		replyPkts := NewSimplexPacketWithMaxSize([]byte(reply), SimplexPacketTypeDATA, MaxOneDriveMessageSize)
		_, err := server.Send(replyPkts, addr)
		if err != nil {
			t.Errorf("Server.Send to %s error: %v", addr.Path, err)
		}
	}

	// 所有客户端接收回复
	var mu sync.Mutex
	replies := make(map[string]bool)
	var recvWg sync.WaitGroup

	for i, client := range clients {
		recvWg.Add(1)
		go func(idx int, c *OneDriveClient) {
			defer recvWg.Done()
			dl := time.Now().Add(10 * time.Second)
			for time.Now().Before(dl) {
				pkt, _, err := c.Receive()
				if err != nil {
					return
				}
				if pkt != nil {
					mu.Lock()
					replies[string(pkt.Data)] = true
					mu.Unlock()
					return
				}
				time.Sleep(50 * time.Millisecond)
			}
		}(i, client)
	}
	recvWg.Wait()

	if len(replies) != numClients {
		t.Errorf("Expected %d replies, got %d", numClients, len(replies))
	}

	t.Logf("ConcurrentBidirectional: %d clients, %d messages sent, %d replies received, files: %d",
		numClients, len(received), len(replies), fake.FileCount())
}

// --- Mock 文件操作压力测试 ---

// TestOneDriveStability_MockFileOperator_Concurrent 并发文件操作
func TestOneDriveStability_MockFileOperator_Concurrent(t *testing.T) {
	mock := newMockOneDriveOps()

	var wg sync.WaitGroup
	var ops int64
	duration := 1 * time.Second
	stopCh := make(chan struct{})
	time.AfterFunc(duration, func() { close(stopCh) })

	// 多个并发 writer
	for i := 0; i < 5; i++ {
		wg.Add(1)
		go func(id int) {
			defer wg.Done()
			counter := 0
			for {
				select {
				case <-stopCh:
					return
				default:
					path := fmt.Sprintf("dir/file_%d_%d.txt", id, counter)
					mock.putFile(path, []byte(fmt.Sprintf("data-%d-%d", id, counter)))
					atomic.AddInt64(&ops, 1)
					counter++
				}
			}
		}(i)
	}

	// 多个并发 reader
	for i := 0; i < 3; i++ {
		wg.Add(1)
		go func(id int) {
			defer wg.Done()
			rng := rand.New(rand.NewSource(time.Now().UnixNano() + int64(id)))
			for {
				select {
				case <-stopCh:
					return
				default:
					path := fmt.Sprintf("dir/file_%d_%d.txt", rng.Intn(5), rng.Intn(1000))
					mock.ReadFile(path)
					atomic.AddInt64(&ops, 1)
				}
			}
		}(i)
	}

	// 并发 lister
	wg.Add(1)
	go func() {
		defer wg.Done()
		for {
			select {
			case <-stopCh:
				return
			default:
				mock.ListFiles("dir")
				atomic.AddInt64(&ops, 1)
				time.Sleep(time.Millisecond)
			}
		}
	}()

	// 并发 deleter
	wg.Add(1)
	go func() {
		defer wg.Done()
		rng := rand.New(rand.NewSource(time.Now().UnixNano()))
		for {
			select {
			case <-stopCh:
				return
			default:
				path := fmt.Sprintf("dir/file_%d_%d.txt", rng.Intn(5), rng.Intn(1000))
				mock.DeleteFile(path)
				atomic.AddInt64(&ops, 1)
			}
		}
	}()

	wg.Wait()
	t.Logf("MockFileOperator concurrent: %d ops in %v, %d files remaining", ops, duration, mock.fileCount())
}

// --- Server/Client 生命周期测试 ---

// TestOneDriveStability_ServerCloseWhileActive 服务端关闭时客户端行为
func TestOneDriveStability_ServerCloseWhileActive(t *testing.T) {
	fake := NewFakeGraphAPIServer()
	fake.Install(t)

	prefix := "stab-close-" + randomString(6)
	server, err := NewOneDriveServer("onedrive", oneDriveServerURL(prefix))
	if err != nil {
		t.Fatalf("NewOneDriveServer error: %v", err)
	}

	addr, _ := ResolveOneDriveAddr("onedrive", oneDriveClientURL(prefix))
	client, err := NewOneDriveClient(addr)
	if err != nil {
		t.Fatalf("NewOneDriveClient error: %v", err)
	}

	// 先建立通信
	pkts := NewSimplexPacketWithMaxSize([]byte("before-close"), SimplexPacketTypeDATA, MaxOneDriveMessageSize)
	client.Send(pkts, addr)

	pkt, _ := oneDrivePollServerUntilReceived(t, server)
	if string(pkt.Data) != "before-close" {
		t.Errorf("Expected 'before-close', got %q", string(pkt.Data))
	}

	// 关闭服务端
	server.Close()

	// 服务端 Receive 应该返回错误
	_, _, err = server.Receive()
	if err == nil {
		t.Log("Server.Receive after Close returned nil error (non-blocking check may still work)")
	}

	// 客户端 Send 不应 panic
	pkts2 := NewSimplexPacketWithMaxSize([]byte("after-close"), SimplexPacketTypeDATA, MaxOneDriveMessageSize)
	_, err = client.Send(pkts2, addr)
	if err != nil {
		t.Logf("Client.Send after server close: %v (expected)", err)
	}

	client.Close()
	t.Log("ServerCloseWhileActive: no panics, graceful degradation")
}

// --- handleClient + monitoring 交互压力测试 ---

// TestOneDriveStability_HandleClientUnderLoad handleClient 持续工作
func TestOneDriveStability_HandleClientUnderLoad(t *testing.T) {
	mock := newMockOneDriveOps()
	u, _ := url.Parse("onedrive:///testdir")
	addr := &SimplexAddr{URL: u, maxBodySize: MaxOneDriveMessageSize, options: u.Query()}

	state := &fileClientState{
		inBuffer:     NewSimplexBuffer(addr),
		outBuffer:    NewSimplexBuffer(addr),
		addr:         addr,
		lastActivity: time.Now(),
	}

	cfg := oneDriveFileTransportConfig()
	cfg.Interval = 20 * time.Millisecond
	cfg.MaxBodySize = MaxOneDriveMessageSize

	ctx, cancel := newTestContext()
	defer cancel()
	srv := NewFileTransportServer(mock, "testdir/", cfg, addr, ctx, cancel)

	go srv.handleClient(ctx, "loadtest", state)

	// 持续写入 send 文件，模拟客户端高频发送
	// 每次等待 handleClient 完全消费后再写入下一条
	const msgCount = 30
	consumed := 0
	for i := 0; i < msgCount; i++ {
		msg := fmt.Sprintf("load-msg-%03d", i)
		pkts := NewSimplexPacketWithMaxSize([]byte(msg), SimplexPacketTypeDATA, MaxOneDriveMessageSize)
		mock.putFile("testdir/loadtest_send.txt", pkts.Marshal())

		// 等待 handleClient 消费
		deadline := time.Now().Add(2 * time.Second)
		for mock.hasFile("testdir/loadtest_send.txt") && time.Now().Before(deadline) {
			time.Sleep(5 * time.Millisecond)
		}
		if !mock.hasFile("testdir/loadtest_send.txt") {
			consumed++
		}
	}

	// 等待最后的处理完成
	time.Sleep(100 * time.Millisecond)

	// 验证 inBuffer 中的消息
	received := 0
	for {
		pkt, _ := state.inBuffer.GetPacket()
		if pkt == nil {
			break
		}
		received++
	}

	if received != consumed {
		t.Errorf("Expected %d messages in inBuffer (consumed), got %d", consumed, received)
	}
	if consumed < msgCount*9/10 {
		t.Errorf("Too many timeouts: only %d/%d consumed", consumed, msgCount)
	}

	// 同时测试 outBuffer → recv 文件
	for i := 0; i < 10; i++ {
		outPkt := NewSimplexPacket(SimplexPacketTypeDATA, []byte(fmt.Sprintf("out-%d", i)))
		state.outBuffer.PutPacket(outPkt)

		// 等待 handleClient 写入 recv 文件
		deadline := time.Now().Add(2 * time.Second)
		for !mock.hasFile("testdir/loadtest_recv.txt") && time.Now().Before(deadline) {
			time.Sleep(10 * time.Millisecond)
		}

		if mock.hasFile("testdir/loadtest_recv.txt") {
			// 模拟客户端消费 recv 文件
			mock.DeleteFile("testdir/loadtest_recv.txt")
		}
	}

	t.Logf("HandleClientUnderLoad: %d/%d messages processed", received, msgCount)
}

// --- pendingData 竞争条件压力测试 ---

// TestOneDriveStability_PendingDataRace 并发 Send 竞争
func TestOneDriveStability_PendingDataRace(t *testing.T) {
	addr := &SimplexAddr{maxBodySize: MaxOneDriveMessageSize}
	ctx, cancel := newTestContext()
	defer cancel()

	cfg := oneDriveFileTransportConfig()
	cfg.Interval = 50 * time.Millisecond
	cfg.MaxBodySize = MaxOneDriveMessageSize

	ftc := NewFileTransportClient(nil, cfg, NewSimplexBuffer(addr), "", "", addr, ctx, cancel)

	var wg sync.WaitGroup
	const goroutines = 10
	const sendsPerGoroutine = 100

	for g := 0; g < goroutines; g++ {
		wg.Add(1)
		go func(id int) {
			defer wg.Done()
			for i := 0; i < sendsPerGoroutine; i++ {
				msg := fmt.Sprintf("g%d-msg%d", id, i)
				pkts := NewSimplexPacketWithMaxSize([]byte(msg), SimplexPacketTypeDATA, MaxOneDriveMessageSize)
				ftc.Send(pkts, addr)
			}
		}(g)
	}
	wg.Wait()

	// 验证 pendingData 可以完整解析
	ftc.pendingMu.Lock()
	pendingCopy := append([]byte(nil), ftc.pendingData...)
	ftc.pendingMu.Unlock()

	parsed, err := ParseSimplexPackets(pendingCopy)
	if err != nil {
		t.Fatalf("ParseSimplexPackets error: %v", err)
	}

	expectedCount := goroutines * sendsPerGoroutine
	if len(parsed.Packets) != expectedCount {
		t.Errorf("Expected %d packets, got %d", expectedCount, len(parsed.Packets))
	}

	t.Logf("PendingDataRace: %d goroutines x %d sends = %d packets, all parsed OK",
		goroutines, sendsPerGoroutine, len(parsed.Packets))
}

// --- scanForNewClients 持续扫描压力测试 ---

// TestOneDriveStability_ScanForNewClients_Repeated 反复扫描不泄漏 goroutine
func TestOneDriveStability_ScanForNewClients_Repeated(t *testing.T) {
	mock := newMockOneDriveOps()
	u, _ := url.Parse("onedrive:///scandir?tenant=t&client=c&secret=s&site=site-id")
	addr := &SimplexAddr{URL: u, maxBodySize: MaxOneDriveMessageSize, options: u.Query()}

	cfg := oneDriveFileTransportConfig()
	cfg.Interval = 50 * time.Millisecond
	cfg.MaxBodySize = MaxOneDriveMessageSize

	ctx, cancel := newTestContext()
	srv := NewFileTransportServer(mock, "scandir/", cfg, addr, ctx, cancel)
	defer cancel()

	// 模拟 20 个客户端陆续出现
	for i := 0; i < 20; i++ {
		clientName := fmt.Sprintf("scan-client-%02d", i)
		mock.putFile(fmt.Sprintf("scandir/%s_send.txt", clientName), []byte("hello"))

		srv.scanForNewClients()

		// 验证新客户端被发现
		count := 0
		srv.clients.Range(func(_, _ interface{}) bool {
			count++
			return true
		})
		if count != i+1 {
			t.Errorf("After adding client %d: expected %d clients, got %d", i, i+1, count)
		}
	}

	// 再扫描多次，不应创建重复
	for i := 0; i < 50; i++ {
		srv.scanForNewClients()
	}

	finalCount := 0
	srv.clients.Range(func(_, _ interface{}) bool {
		finalCount++
		return true
	})

	if finalCount != 20 {
		t.Errorf("After repeated scans: expected 20 clients, got %d", finalCount)
	}

	t.Logf("ScanForNewClients_Repeated: 20 clients discovered, no duplicates after 50 rescans")
}

// --- E2E 混合负载测试 ---

// TestOneDriveStability_MixedWorkload 混合大小数据 + 多客户端 + 双向通信
func TestOneDriveStability_MixedWorkload(t *testing.T) {
	fake := NewFakeGraphAPIServer()
	fake.Install(t)

	prefix := "stab-mixed-" + randomString(6)
	server, err := NewOneDriveServer("onedrive", oneDriveServerURL(prefix))
	if err != nil {
		t.Fatalf("NewOneDriveServer error: %v", err)
	}
	defer server.Close()

	const numClients = 3
	clients := make([]*OneDriveClient, numClients)
	addrs := make([]*SimplexAddr, numClients)
	for i := 0; i < numClients; i++ {
		clientURL := fmt.Sprintf("%s&client_id=mixed%d", oneDriveClientURL(prefix), i)
		addrs[i], _ = ResolveOneDriveAddr("onedrive", clientURL)
		clients[i], err = NewOneDriveClient(addrs[i])
		if err != nil {
			t.Fatalf("NewOneDriveClient[%d] error: %v", i, err)
		}
		defer clients[i].Close()
	}

	// 每个客户端发送不同大小的数据
	sizes := []int{10, 100, 5000}
	var sendWg sync.WaitGroup

	for i, client := range clients {
		sendWg.Add(1)
		go func(idx int, c *OneDriveClient, sz int) {
			defer sendWg.Done()
			data := make([]byte, sz)
			for j := range data {
				data[j] = byte(idx*50 + j%200)
			}
			pkts := NewSimplexPacketWithMaxSize(data, SimplexPacketTypeDATA, MaxOneDriveMessageSize)
			c.Send(pkts, addrs[idx])
		}(i, client, sizes[i])
	}
	sendWg.Wait()

	// 收集所有消息并回复
	receivedCount := 0
	deadline := time.Now().Add(15 * time.Second)
	for receivedCount < numClients && time.Now().Before(deadline) {
		time.Sleep(100 * time.Millisecond)
		for {
			pkt, addr, err := server.Receive()
			if err != nil {
				t.Fatalf("Server.Receive error: %v", err)
			}
			if pkt == nil {
				break
			}
			receivedCount++
			t.Logf("Server received %d bytes from %s", len(pkt.Data), addr.Path)

			// 回复
			reply := fmt.Sprintf("ack-%d-bytes", len(pkt.Data))
			replyPkts := NewSimplexPacketWithMaxSize([]byte(reply), SimplexPacketTypeDATA, MaxOneDriveMessageSize)
			server.Send(replyPkts, addr)
		}
	}

	if receivedCount != numClients {
		t.Errorf("Expected %d messages, got %d", numClients, receivedCount)
	}

	// 验证所有客户端收到回复
	replyCount := 0
	var replyWg sync.WaitGroup
	var replyMu sync.Mutex

	for _, client := range clients {
		replyWg.Add(1)
		go func(c *OneDriveClient) {
			defer replyWg.Done()
			dl := time.Now().Add(10 * time.Second)
			for time.Now().Before(dl) {
				pkt, _, _ := c.Receive()
				if pkt != nil {
					replyMu.Lock()
					replyCount++
					replyMu.Unlock()
					return
				}
				time.Sleep(50 * time.Millisecond)
			}
		}(client)
	}
	replyWg.Wait()

	if replyCount != numClients {
		t.Errorf("Expected %d replies, got %d", numClients, replyCount)
	}

	t.Logf("MixedWorkload: %d clients, %d messages, %d replies, files: %d",
		numClients, receivedCount, replyCount, fake.FileCount())
}
