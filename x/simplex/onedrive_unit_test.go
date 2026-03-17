//go:build graph
// +build graph

package simplex

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"net/url"
	"os"
	"strings"
	"sync"
	"testing"
	"time"
)

// ============================================================
// OneDrive Unit Tests — 使用 mockOneDriveOps 隔离网络层
// 专注验证核心数据收发逻辑
// ============================================================

// mockOneDriveOps implements FileStorageOps for OneDrive unit tests
type mockOneDriveOps struct {
	mu    sync.Mutex
	files map[string][]byte
}

func newMockOneDriveOps() *mockOneDriveOps {
	return &mockOneDriveOps{files: make(map[string][]byte)}
}

func (m *mockOneDriveOps) ReadFile(path string) ([]byte, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	data, ok := m.files[path]
	if !ok {
		return nil, fmt.Errorf("file not found %s: %w", path, os.ErrNotExist)
	}
	return append([]byte(nil), data...), nil
}

func (m *mockOneDriveOps) WriteFile(path string, data []byte) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.files[path] = append([]byte(nil), data...)
	return nil
}

func (m *mockOneDriveOps) DeleteFile(path string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	delete(m.files, path)
	return nil
}

func (m *mockOneDriveOps) ListFiles(folderPath string) ([]string, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	prefix := folderPath
	if !strings.HasSuffix(prefix, "/") {
		prefix += "/"
	}
	var result []string
	seen := make(map[string]bool)
	for path := range m.files {
		if !strings.HasPrefix(path, prefix) {
			continue
		}
		name := path[len(prefix):]
		if strings.Contains(name, "/") {
			continue
		}
		if seen[name] {
			continue
		}
		seen[name] = true
		result = append(result, name)
	}
	return result, nil
}

func (m *mockOneDriveOps) FileExists(path string) (bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	_, ok := m.files[path]
	return ok, nil
}

func (m *mockOneDriveOps) hasFile(path string) bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	_, ok := m.files[path]
	return ok
}

func (m *mockOneDriveOps) getFile(path string) []byte {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.files[path]
}

func (m *mockOneDriveOps) putFile(path string, data []byte) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.files[path] = append([]byte(nil), data...)
}

func (m *mockOneDriveOps) fileCount() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return len(m.files)
}

// --- 基础序列化/反序列化单元测试 ---

func TestOneDriveUnit_PacketMarshalRoundTrip(t *testing.T) {
	original := []byte("hello onedrive")
	pkts := NewSimplexPacketWithMaxSize(original, SimplexPacketTypeDATA, MaxOneDriveMessageSize)
	data := pkts.Marshal()

	parsed, err := ParseSimplexPackets(data)
	if err != nil {
		t.Fatalf("ParseSimplexPackets error: %v", err)
	}

	var reconstructed []byte
	for _, pkt := range parsed.Packets {
		reconstructed = append(reconstructed, pkt.Data...)
	}

	if !bytes.Equal(original, reconstructed) {
		t.Errorf("roundtrip failed: got %q, want %q", reconstructed, original)
	}
}

func TestOneDriveUnit_ProcessFileData(t *testing.T) {
	original := []byte("test data for ParseSimplexPackets")
	pkts := NewSimplexPacketWithMaxSize(original, SimplexPacketTypeDATA, MaxOneDriveMessageSize)
	fileContent := pkts.Marshal()

	result, err := ParseSimplexPackets(fileContent)
	if err != nil {
		t.Fatalf("ParseSimplexPackets error: %v", err)
	}

	if result == nil || len(result.Packets) == 0 {
		t.Fatal("expected non-empty packets")
	}

	var reconstructed []byte
	for _, pkt := range result.Packets {
		reconstructed = append(reconstructed, pkt.Data...)
	}
	if !bytes.Equal(original, reconstructed) {
		t.Errorf("ParseSimplexPackets roundtrip failed: got %q, want %q", reconstructed, original)
	}
}

// --- Client Send 单元测试 ---

func TestOneDriveUnit_ClientSend_QueuesToPending(t *testing.T) {
	addr := &SimplexAddr{
		maxBodySize: MaxOneDriveMessageSize,
	}

	ctx, cancel := newTestContext()
	defer cancel()

	cfg := oneDriveFileTransportConfig()
	cfg.Interval = 50 * time.Millisecond
	cfg.MaxBodySize = MaxOneDriveMessageSize

	ftc := NewFileTransportClient(nil, cfg, NewSimplexBuffer(addr), "s.txt", "r.txt", addr, ctx, cancel)

	data := []byte("send test")
	pkts := NewSimplexPacketWithMaxSize(data, SimplexPacketTypeDATA, MaxOneDriveMessageSize)

	n, err := ftc.Send(pkts, addr)
	if err != nil {
		t.Fatalf("Send error: %v", err)
	}
	if n == 0 {
		t.Error("expected non-zero bytes sent")
	}

	ftc.pendingMu.Lock()
	hasPending := len(ftc.pendingData) > 0
	ftc.pendingMu.Unlock()

	if !hasPending {
		t.Error("expected pendingData to be non-empty after Send")
	}
}

func TestOneDriveUnit_ClientSend_AppendMultiple(t *testing.T) {
	addr := &SimplexAddr{
		maxBodySize: MaxOneDriveMessageSize,
	}

	ctx, cancel := newTestContext()
	defer cancel()

	cfg := oneDriveFileTransportConfig()
	cfg.Interval = 50 * time.Millisecond
	cfg.MaxBodySize = MaxOneDriveMessageSize

	ftc := NewFileTransportClient(nil, cfg, NewSimplexBuffer(addr), "s.txt", "r.txt", addr, ctx, cancel)

	// Send three messages rapidly
	for i := 0; i < 3; i++ {
		data := []byte(fmt.Sprintf("msg-%d", i))
		pkts := NewSimplexPacketWithMaxSize(data, SimplexPacketTypeDATA, MaxOneDriveMessageSize)
		ftc.Send(pkts, addr)
	}

	ftc.pendingMu.Lock()
	pendingCopy := append([]byte(nil), ftc.pendingData...)
	ftc.pendingMu.Unlock()

	// Parse all accumulated data — should contain all 3 messages
	parsed, err := ParseSimplexPackets(pendingCopy)
	if err != nil {
		t.Fatalf("ParseSimplexPackets error: %v", err)
	}

	if len(parsed.Packets) != 3 {
		t.Errorf("expected 3 packets in pending, got %d", len(parsed.Packets))
	}

	for i, pkt := range parsed.Packets {
		expected := fmt.Sprintf("msg-%d", i)
		if string(pkt.Data) != expected {
			t.Errorf("packet %d: got %q, want %q", i, string(pkt.Data), expected)
		}
	}
}

func TestOneDriveUnit_ClientSend_NilPackets(t *testing.T) {
	ctx, cancel := newTestContext()
	defer cancel()

	cfg := oneDriveFileTransportConfig()
	cfg.Interval = 50 * time.Millisecond
	cfg.MaxBodySize = MaxOneDriveMessageSize

	ftc := NewFileTransportClient(nil, cfg, nil, "", "", nil, ctx, cancel)

	n, err := ftc.Send(nil, nil)
	if err != nil {
		t.Fatalf("Send nil error: %v", err)
	}
	if n != 0 {
		t.Errorf("expected 0 bytes, got %d", n)
	}
}

// --- Server Buffer 隔离单元测试 ---

func TestOneDriveUnit_ServerBufferSeparation(t *testing.T) {
	addr := &SimplexAddr{
		maxBodySize: MaxOneDriveMessageSize,
	}

	state := &fileClientState{
		inBuffer:  NewSimplexBuffer(addr),
		outBuffer: NewSimplexBuffer(addr),
		addr:      addr,
	}

	// 写入 inBuffer (模拟客户端发来的数据)
	inPkt := NewSimplexPacket(SimplexPacketTypeDATA, []byte("from-client"))
	state.inBuffer.PutPacket(inPkt)

	// 写入 outBuffer (模拟服务端要发给客户端的数据)
	outPkt := NewSimplexPacket(SimplexPacketTypeDATA, []byte("from-server"))
	state.outBuffer.PutPacket(outPkt)

	// 从 inBuffer 读取，应该只得到 "from-client"
	gotIn, _ := state.inBuffer.GetPacket()
	if gotIn == nil || string(gotIn.Data) != "from-client" {
		t.Errorf("inBuffer: expected 'from-client', got %v", gotIn)
	}

	// inBuffer 应该空了
	gotIn2, _ := state.inBuffer.GetPacket()
	if gotIn2 != nil {
		t.Errorf("inBuffer should be empty, got %q", string(gotIn2.Data))
	}

	// 从 outBuffer 读取，应该只得到 "from-server"
	gotOut, _ := state.outBuffer.GetPacket()
	if gotOut == nil || string(gotOut.Data) != "from-server" {
		t.Errorf("outBuffer: expected 'from-server', got %v", gotOut)
	}
}

// --- ParseSimplexPackets → buffer 单元测试 ---

func TestOneDriveUnit_ProcessSendFile(t *testing.T) {
	addr := &SimplexAddr{maxBodySize: MaxOneDriveMessageSize}

	buffer := NewSimplexBuffer(addr)
	original := []byte("incoming data")
	pkts := NewSimplexPacketWithMaxSize(original, SimplexPacketTypeDATA, MaxOneDriveMessageSize)
	fileContent := pkts.Marshal()

	parsed, err := ParseSimplexPackets(fileContent)
	if err != nil {
		t.Fatalf("ParseSimplexPackets error: %v", err)
	}
	for _, pkt := range parsed.Packets {
		buffer.PutPacket(pkt)
	}

	pkt, _ := buffer.GetPacket()
	if pkt == nil {
		t.Fatal("expected packet in buffer")
	}
	if string(pkt.Data) != "incoming data" {
		t.Errorf("got %q, want %q", string(pkt.Data), "incoming data")
	}
}

// --- mockOneDriveOps 驱动的集成单元测试 ---

func TestOneDriveUnit_ClientMonitoring_SendCycle(t *testing.T) {
	mock := newMockOneDriveOps()
	addr := &SimplexAddr{maxBodySize: MaxOneDriveMessageSize}

	ctx, cancel := newTestContext()

	cfg := oneDriveFileTransportConfig()
	cfg.Interval = 50 * time.Millisecond
	cfg.MaxBodySize = MaxOneDriveMessageSize

	buffer := NewSimplexBuffer(addr)
	ftc := NewFileTransportClient(mock, cfg, buffer, "dir/test_send.txt", "dir/test_recv.txt", addr, ctx, cancel)

	// 设置待发送数据
	data := []byte("monitoring test")
	pkts := NewSimplexPacketWithMaxSize(data, SimplexPacketTypeDATA, MaxOneDriveMessageSize)
	ftc.Send(pkts, addr)

	go ftc.monitoring()
	defer cancel()

	deadline := time.Now().Add(2 * time.Second)
	for !mock.hasFile("dir/test_send.txt") && time.Now().Before(deadline) {
		time.Sleep(20 * time.Millisecond)
	}

	if !mock.hasFile("dir/test_send.txt") {
		t.Fatal("monitoring did not write send file")
	}

	content := mock.getFile("dir/test_send.txt")
	parsed, err := ParseSimplexPackets(content)
	if err != nil {
		t.Fatalf("parse error: %v", err)
	}
	if len(parsed.Packets) != 1 || string(parsed.Packets[0].Data) != "monitoring test" {
		t.Errorf("unexpected file content")
	}

	ftc.pendingMu.Lock()
	isEmpty := len(ftc.pendingData) == 0
	ftc.pendingMu.Unlock()
	if !isEmpty {
		t.Error("pendingData should be empty after successful write")
	}
}

func TestOneDriveUnit_ClientMonitoring_RecvCycle(t *testing.T) {
	mock := newMockOneDriveOps()
	addr := &SimplexAddr{maxBodySize: MaxOneDriveMessageSize}

	ctx, cancel := newTestContext()

	cfg := oneDriveFileTransportConfig()
	cfg.Interval = 50 * time.Millisecond
	cfg.MaxBodySize = MaxOneDriveMessageSize

	buffer := NewSimplexBuffer(addr)
	ftc := NewFileTransportClient(mock, cfg, buffer, "dir/test_send.txt", "dir/test_recv.txt", addr, ctx, cancel)

	// 预先放置一个 recv 文件（模拟服务端写入）
	recvData := []byte("response from server")
	recvPkts := NewSimplexPacketWithMaxSize(recvData, SimplexPacketTypeDATA, MaxOneDriveMessageSize)
	mock.putFile("dir/test_recv.txt", recvPkts.Marshal())

	go ftc.monitoring()
	defer cancel()

	deadline := time.Now().Add(2 * time.Second)
	var pkt *SimplexPacket
	for time.Now().Before(deadline) {
		pkt, _ = buffer.GetPacket()
		if pkt != nil {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}

	if pkt == nil {
		t.Fatal("monitoring did not process recv file into buffer")
	}
	if string(pkt.Data) != "response from server" {
		t.Errorf("got %q, want %q", string(pkt.Data), "response from server")
	}

	if mock.hasFile("dir/test_recv.txt") {
		t.Error("recv file should have been deleted after processing")
	}
}

func TestOneDriveUnit_ServerHandleClient_SendRecvCycle(t *testing.T) {
	mock := newMockOneDriveOps()
	addr := oneDriveTestAddr()

	clientAddr := generateAddrFromPath("testcli", addr)
	state := &fileClientState{
		inBuffer:     NewSimplexBuffer(clientAddr),
		outBuffer:    NewSimplexBuffer(clientAddr),
		addr:         clientAddr,
		lastActivity: time.Now(),
	}

	cfg := oneDriveFileTransportConfig()
	cfg.Interval = 50 * time.Millisecond
	cfg.MaxBodySize = MaxOneDriveMessageSize

	ctx, cancel := newTestContext()
	defer cancel()
	srv := NewFileTransportServer(mock, "testdir/", cfg, addr, ctx, cancel)

	clientID := "testcli"

	// 模拟客户端写入 send 文件
	clientData := []byte("client payload")
	clientPkts := NewSimplexPacketWithMaxSize(clientData, SimplexPacketTypeDATA, MaxOneDriveMessageSize)
	mock.putFile(fmt.Sprintf("testdir/%s_send.txt", clientID), clientPkts.Marshal())

	go srv.handleClient(ctx, clientID, state)

	deadline := time.Now().Add(2 * time.Second)
	var pkt *SimplexPacket
	for time.Now().Before(deadline) {
		pkt, _ = state.inBuffer.GetPacket()
		if pkt != nil {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}

	if pkt == nil {
		t.Fatal("handleClient did not process send file into inBuffer")
	}
	if string(pkt.Data) != "client payload" {
		t.Errorf("got %q, want %q", string(pkt.Data), "client payload")
	}

	if mock.hasFile(fmt.Sprintf("testdir/%s_send.txt", clientID)) {
		t.Error("send file should have been deleted")
	}

	// 向 outBuffer 放入数据，验证 handleClient 写入 recv 文件
	serverPkt := NewSimplexPacket(SimplexPacketTypeDATA, []byte("server response"))
	state.outBuffer.PutPacket(serverPkt)

	recvPath := fmt.Sprintf("testdir/%s_recv.txt", clientID)
	deadline = time.Now().Add(2 * time.Second)
	for !mock.hasFile(recvPath) && time.Now().Before(deadline) {
		time.Sleep(20 * time.Millisecond)
	}

	if !mock.hasFile(recvPath) {
		t.Fatal("handleClient did not write recv file from outBuffer")
	}

	recvContent := mock.getFile(recvPath)
	recvParsed, err := ParseSimplexPackets(recvContent)
	if err != nil {
		t.Fatalf("parse recv file error: %v", err)
	}
	if len(recvParsed.Packets) != 1 || string(recvParsed.Packets[0].Data) != "server response" {
		t.Errorf("unexpected recv file content")
	}
}

func TestOneDriveUnit_ServerScanForNewClients(t *testing.T) {
	mock := newMockOneDriveOps()
	u, _ := url.Parse("onedrive:///testdir?tenant=t&client=c&secret=s&site=site-id")
	addr := &SimplexAddr{
		URL:         u,
		maxBodySize: MaxOneDriveMessageSize,
		options:     u.Query(),
	}

	ctx, cancel := newTestContext()

	cfg := oneDriveFileTransportConfig()
	cfg.Interval = 50 * time.Millisecond
	cfg.MaxBodySize = MaxOneDriveMessageSize

	srv := NewFileTransportServer(mock, "testdir/", cfg, addr, ctx, cancel)
	defer cancel()

	// 模拟两个客户端的 send 文件
	mock.putFile("testdir/clientA_send.txt", []byte("dummy"))
	mock.putFile("testdir/clientB_send.txt", []byte("dummy"))

	srv.scanForNewClients()

	count := 0
	srv.clients.Range(func(_, _ interface{}) bool {
		count++
		return true
	})
	if count != 2 {
		t.Errorf("expected 2 clients, got %d", count)
	}

	if _, ok := srv.clients.Load("clientA"); !ok {
		t.Error("clientA not found")
	}
	if _, ok := srv.clients.Load("clientB"); !ok {
		t.Error("clientB not found")
	}

	// 再次 scan 不应重复创建
	srv.scanForNewClients()
	count2 := 0
	srv.clients.Range(func(_, _ interface{}) bool {
		count2++
		return true
	})
	if count2 != 2 {
		t.Errorf("repeated scan should not create duplicates, got %d clients", count2)
	}

	// 新文件应该被发现
	mock.putFile("testdir/clientC_send.txt", []byte("dummy"))
	srv.scanForNewClients()
	count3 := 0
	srv.clients.Range(func(_, _ interface{}) bool {
		count3++
		return true
	})
	if count3 != 3 {
		t.Errorf("expected 3 clients after adding clientC, got %d", count3)
	}
}

func TestOneDriveUnit_ServerReceive(t *testing.T) {
	addr := &SimplexAddr{maxBodySize: MaxOneDriveMessageSize}

	ctx, cancel := newTestContext()

	cfg := oneDriveFileTransportConfig()
	cfg.Interval = 50 * time.Millisecond
	cfg.MaxBodySize = MaxOneDriveMessageSize

	srv := NewFileTransportServer(nil, "", cfg, addr, ctx, cancel)
	defer cancel()

	clientAddr := &SimplexAddr{maxBodySize: MaxOneDriveMessageSize}
	state := &fileClientState{
		inBuffer:  NewSimplexBuffer(clientAddr),
		outBuffer: NewSimplexBuffer(clientAddr),
		addr:      clientAddr,
	}
	srv.clients.Store("test", state)

	// inBuffer 为空时 Receive 返回 nil
	pkt, _, err := srv.Receive()
	if err != nil {
		t.Fatalf("Receive error: %v", err)
	}
	if pkt != nil {
		t.Error("expected nil packet from empty inBuffer")
	}

	// 往 outBuffer 放数据 — Receive 不应读到
	state.outBuffer.PutPacket(NewSimplexPacket(SimplexPacketTypeDATA, []byte("out-data")))
	pkt, _, _ = srv.Receive()
	if pkt != nil {
		t.Errorf("Receive should not read from outBuffer, got %q", string(pkt.Data))
	}

	// 往 inBuffer 放数据 — Receive 应该读到
	state.inBuffer.PutPacket(NewSimplexPacket(SimplexPacketTypeDATA, []byte("in-data")))
	pkt, recvAddr, _ := srv.Receive()
	if pkt == nil || string(pkt.Data) != "in-data" {
		t.Errorf("Receive from inBuffer: expected 'in-data', got %v", pkt)
	}
	if recvAddr != clientAddr {
		t.Error("Receive should return client addr")
	}
}

func TestOneDriveUnit_ServerSend(t *testing.T) {
	ctx, cancel := newTestContext()
	defer cancel()

	u, _ := url.Parse("onedrive:///testdir?tenant=t&client=c&secret=s&site=site-id")
	baseAddr := &SimplexAddr{URL: u, maxBodySize: MaxOneDriveMessageSize, options: u.Query()}

	cfg := oneDriveFileTransportConfig()
	cfg.Interval = 50 * time.Millisecond
	cfg.MaxBodySize = MaxOneDriveMessageSize

	srv := NewFileTransportServer(nil, "", cfg, baseAddr, ctx, cancel)

	clientAddr := generateAddrFromPath("test-client", baseAddr)
	state := &fileClientState{
		inBuffer:  NewSimplexBuffer(clientAddr),
		outBuffer: NewSimplexBuffer(clientAddr),
		addr:      clientAddr,
	}
	srv.clients.Store(clientAddr.Path, state)

	pkts := NewSimplexPacketWithMaxSize([]byte("server-msg"), SimplexPacketTypeDATA, MaxOneDriveMessageSize)
	n, err := srv.Send(pkts, clientAddr)
	if err != nil {
		t.Fatalf("Send error: %v", err)
	}
	if n == 0 {
		t.Error("expected non-zero bytes sent")
	}

	// outBuffer 应该有数据
	pkt, _ := state.outBuffer.GetPacket()
	if pkt == nil || string(pkt.Data) != "server-msg" {
		t.Errorf("outBuffer: expected 'server-msg', got %v", pkt)
	}

	// inBuffer 不应有数据
	inPkt, _ := state.inBuffer.GetPacket()
	if inPkt != nil {
		t.Errorf("inBuffer should be empty, got %q", string(inPkt.Data))
	}
}

func TestOneDriveUnit_FullDataFlow_MockDriven(t *testing.T) {
	mock := newMockOneDriveOps()
	addr := &SimplexAddr{maxBodySize: MaxOneDriveMessageSize}
	prefix := "flow"
	dir := "testdir"
	sendFile := fmt.Sprintf("%s/%s_send.txt", dir, prefix)
	recvFile := fmt.Sprintf("%s/%s_recv.txt", dir, prefix)

	// === Phase 1: Client → File ===
	clientData := []byte("request from client")
	clientPkts := NewSimplexPacketWithMaxSize(clientData, SimplexPacketTypeDATA, MaxOneDriveMessageSize)
	fileContent := clientPkts.Marshal()
	mock.putFile(sendFile, fileContent)

	// === Phase 2: File → Server inBuffer ===
	state := &fileClientState{
		inBuffer:  NewSimplexBuffer(addr),
		outBuffer: NewSimplexBuffer(addr),
		addr:      addr,
	}

	content := mock.getFile(sendFile)
	if parsed, err := ParseSimplexPackets(content); err == nil {
		for _, pkt := range parsed.Packets {
			state.inBuffer.PutPacket(pkt)
		}
	}
	mock.DeleteFile(sendFile)

	pkt, _ := state.inBuffer.GetPacket()
	if pkt == nil || string(pkt.Data) != "request from client" {
		t.Fatalf("Phase 2 failed: expected 'request from client', got %v", pkt)
	}

	// === Phase 3: Server → outBuffer → File ===
	serverPkt := NewSimplexPacket(SimplexPacketTypeDATA, []byte("response from server"))
	state.outBuffer.PutPacket(serverPkt)

	outPkt, _ := state.outBuffer.GetPacket()
	outPkts := &SimplexPackets{Packets: []*SimplexPacket{outPkt}}
	mock.WriteFile(recvFile, outPkts.Marshal())

	if !mock.hasFile(recvFile) {
		t.Fatal("Phase 3 failed: recv file not created")
	}

	// === Phase 4: File → Client buffer ===
	clientBuffer := NewSimplexBuffer(addr)
	recvContent := mock.getFile(recvFile)
	recvParsed, err := ParseSimplexPackets(recvContent)
	if err != nil {
		t.Fatalf("Phase 4 parse error: %v", err)
	}
	for _, pkt := range recvParsed.Packets {
		clientBuffer.PutPacket(pkt)
	}
	mock.DeleteFile(recvFile)

	respPkt, _ := clientBuffer.GetPacket()
	if respPkt == nil || string(respPkt.Data) != "response from server" {
		t.Fatalf("Phase 4 failed: expected 'response from server', got %v", respPkt)
	}

	t.Log("Full data flow passed: Client → File → Server → File → Client")
}

func TestOneDriveUnit_ClientSend_PendingRetryOnFileExists(t *testing.T) {
	mock := newMockOneDriveOps()
	addr := &SimplexAddr{maxBodySize: MaxOneDriveMessageSize}

	ctx, cancel := newTestContext()

	cfg := oneDriveFileTransportConfig()
	cfg.Interval = 50 * time.Millisecond
	cfg.MaxBodySize = MaxOneDriveMessageSize

	buffer := NewSimplexBuffer(addr)
	ftc := NewFileTransportClient(mock, cfg, buffer, "dir/test_send.txt", "dir/test_recv.txt", addr, ctx, cancel)

	// 先放一个 send 文件（模拟服务端还没读取）
	mock.putFile("dir/test_send.txt", []byte("old data"))

	// Send 新数据
	pkts := NewSimplexPacketWithMaxSize([]byte("new data"), SimplexPacketTypeDATA, MaxOneDriveMessageSize)
	ftc.Send(pkts, addr)

	go ftc.monitoring()
	defer cancel()

	// monitoring 应该发现 send 文件存在，不覆盖，放回 pendingData
	time.Sleep(200 * time.Millisecond)

	ftc.pendingMu.Lock()
	hasPending := len(ftc.pendingData) > 0
	ftc.pendingMu.Unlock()

	if !hasPending {
		t.Error("pendingData should still have data when send file already exists")
	}

	if string(mock.getFile("dir/test_send.txt")) != "old data" {
		t.Error("send file should not be overwritten")
	}

	// 删除旧文件，等 monitoring 写入新数据
	mock.DeleteFile("dir/test_send.txt")
	deadline := time.Now().Add(2 * time.Second)
	for !mock.hasFile("dir/test_send.txt") && time.Now().Before(deadline) {
		time.Sleep(20 * time.Millisecond)
	}

	if !mock.hasFile("dir/test_send.txt") {
		t.Fatal("monitoring did not write new data after old file was deleted")
	}

	content := mock.getFile("dir/test_send.txt")
	parsed, err := ParseSimplexPackets(content)
	if err != nil {
		t.Fatalf("parse error: %v", err)
	}
	if len(parsed.Packets) != 1 || string(parsed.Packets[0].Data) != "new data" {
		t.Errorf("unexpected content: got %v", parsed.Packets)
	}
}

// --- Packet loss / error propagation tests ---

type failingOneDriveOps struct {
	*mockOneDriveOps
	mu             sync.Mutex
	readFailCount  int
	writeFailCount int
	deleteFailFunc func(path string) error
}

func newFailingOneDriveOps() *failingOneDriveOps {
	return &failingOneDriveOps{
		mockOneDriveOps: newMockOneDriveOps(),
	}
}

func (f *failingOneDriveOps) ReadFile(path string) ([]byte, error) {
	f.mu.Lock()
	if f.readFailCount > 0 {
		f.readFailCount--
		f.mu.Unlock()
		return nil, fmt.Errorf("simulated read failure")
	}
	f.mu.Unlock()
	return f.mockOneDriveOps.ReadFile(path)
}

func (f *failingOneDriveOps) WriteFile(path string, data []byte) error {
	f.mu.Lock()
	if f.writeFailCount > 0 {
		f.writeFailCount--
		f.mu.Unlock()
		return fmt.Errorf("simulated write failure")
	}
	f.mu.Unlock()
	return f.mockOneDriveOps.WriteFile(path, data)
}

func (f *failingOneDriveOps) DeleteFile(path string) error {
	f.mu.Lock()
	fn := f.deleteFailFunc
	f.mu.Unlock()
	if fn != nil {
		return fn(path)
	}
	return f.mockOneDriveOps.DeleteFile(path)
}

func (f *failingOneDriveOps) ListFiles(prefix string) ([]string, error) {
	return f.mockOneDriveOps.ListFiles(prefix)
}

func (f *failingOneDriveOps) FileExists(path string) (bool, error) {
	return f.mockOneDriveOps.FileExists(path)
}

func TestOneDrive_PacketLoss_ServerPullFails(t *testing.T) {
	mock := newFailingOneDriveOps()
	addr := oneDriveTestAddr()

	clientAddr := generateAddrFromPath("pulltest", addr)
	state := &fileClientState{
		inBuffer:     NewSimplexBuffer(clientAddr),
		outBuffer:    NewSimplexBuffer(clientAddr),
		addr:         clientAddr,
		lastActivity: time.Now(),
	}

	cfg := oneDriveFileTransportConfig()
	cfg.Interval = 50 * time.Millisecond
	cfg.MaxBodySize = MaxOneDriveMessageSize

	ctx, cancel := newTestContext()
	defer cancel()
	srv := NewFileTransportServer(mock, "testdir/", cfg, addr, ctx, cancel)

	clientID := "pulltest"
	clientData := []byte("retry data")
	clientPkts := NewSimplexPacketWithMaxSize(clientData, SimplexPacketTypeDATA, MaxOneDriveMessageSize)
	mock.putFile(fmt.Sprintf("testdir/%s_send.txt", clientID), clientPkts.Marshal())

	// First 3 reads will fail
	mock.mu.Lock()
	mock.readFailCount = 3
	mock.mu.Unlock()

	go srv.handleClient(ctx, clientID, state)

	deadline := time.Now().Add(5 * time.Second)
	var pkt *SimplexPacket
	for time.Now().Before(deadline) {
		pkt, _ = state.inBuffer.GetPacket()
		if pkt != nil {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}

	if pkt == nil {
		t.Fatal("Server did not receive data after ReadFile failures")
	}
	if string(pkt.Data) != "retry data" {
		t.Errorf("got %q, want %q", string(pkt.Data), "retry data")
	}
}

func TestOneDrive_PacketLoss_ServerDeleteFails(t *testing.T) {
	mock := newFailingOneDriveOps()
	addr := oneDriveTestAddr()

	clientAddr := generateAddrFromPath("deltest", addr)
	state := &fileClientState{
		inBuffer:     NewSimplexBuffer(clientAddr),
		outBuffer:    NewSimplexBuffer(clientAddr),
		addr:         clientAddr,
		lastActivity: time.Now(),
	}

	mock.mu.Lock()
	mock.deleteFailFunc = func(path string) error {
		return fmt.Errorf("simulated delete failure")
	}
	mock.mu.Unlock()

	cfg := oneDriveFileTransportConfig()
	cfg.Interval = 50 * time.Millisecond
	cfg.MaxBodySize = MaxOneDriveMessageSize

	ctx, cancel := newTestContext()
	defer cancel()
	srv := NewFileTransportServer(mock, "testdir/", cfg, addr, ctx, cancel)

	clientID := "deltest"
	clientData := []byte("persistent data")
	clientPkts := NewSimplexPacketWithMaxSize(clientData, SimplexPacketTypeDATA, MaxOneDriveMessageSize)
	mock.putFile(fmt.Sprintf("testdir/%s_send.txt", clientID), clientPkts.Marshal())

	go srv.handleClient(ctx, clientID, state)

	readCount := 0
	deadline := time.Now().Add(3 * time.Second)
	for readCount < 2 && time.Now().Before(deadline) {
		pkt, _ := state.inBuffer.GetPacket()
		if pkt != nil {
			readCount++
			if string(pkt.Data) != "persistent data" {
				t.Errorf("unexpected data: %q", string(pkt.Data))
			}
		}
		time.Sleep(20 * time.Millisecond)
	}

	if readCount < 2 {
		t.Errorf("Expected at least 2 reads due to failed delete, got %d", readCount)
	}
}

func TestOneDrive_PacketLoss_ClientDeleteFails(t *testing.T) {
	baseMock := newMockOneDriveOps()
	mock := &oneDriveDeleteFailOps{
		mockOneDriveOps: baseMock,
		deleteFailFunc: func(path string) error {
			return fmt.Errorf("simulated delete failure")
		},
	}
	addr := &SimplexAddr{maxBodySize: MaxOneDriveMessageSize}

	ctx, cancel := newTestContext()

	cfg := oneDriveFileTransportConfig()
	cfg.Interval = 50 * time.Millisecond
	cfg.MaxBodySize = MaxOneDriveMessageSize

	buffer := NewSimplexBuffer(addr)
	ftc := NewFileTransportClient(mock, cfg, buffer, "dir/delcli_send.txt", "dir/delcli_recv.txt", addr, ctx, cancel)

	recvData := []byte("server response")
	recvPkts := NewSimplexPacketWithMaxSize(recvData, SimplexPacketTypeDATA, MaxOneDriveMessageSize)
	baseMock.putFile("dir/delcli_recv.txt", recvPkts.Marshal())

	go ftc.monitoring()
	defer cancel()

	readCount := 0
	deadline := time.Now().Add(2 * time.Second)
	for readCount < 2 && time.Now().Before(deadline) {
		pkt, _ := buffer.GetPacket()
		if pkt != nil {
			readCount++
			if string(pkt.Data) != "server response" {
				t.Errorf("unexpected data: %q", string(pkt.Data))
			}
		}
		time.Sleep(20 * time.Millisecond)
	}

	if readCount < 2 {
		t.Errorf("Expected at least 2 reads due to failed delete, got %d", readCount)
	}
}

// oneDriveDeleteFailOps wraps mockOneDriveOps but fails on DeleteFile
type oneDriveDeleteFailOps struct {
	*mockOneDriveOps
	deleteFailFunc func(path string) error
}

func (m *oneDriveDeleteFailOps) ReadFile(path string) ([]byte, error) {
	return m.mockOneDriveOps.ReadFile(path)
}

func (m *oneDriveDeleteFailOps) WriteFile(path string, data []byte) error {
	return m.mockOneDriveOps.WriteFile(path, data)
}

func (m *oneDriveDeleteFailOps) DeleteFile(path string) error {
	if m.deleteFailFunc != nil {
		return m.deleteFailFunc(path)
	}
	return m.mockOneDriveOps.DeleteFile(path)
}

func (m *oneDriveDeleteFailOps) ListFiles(prefix string) ([]string, error) {
	return m.mockOneDriveOps.ListFiles(prefix)
}

func (m *oneDriveDeleteFailOps) FileExists(path string) (bool, error) {
	return m.mockOneDriveOps.FileExists(path)
}

func TestOneDrive_Client_ErrorPropagation(t *testing.T) {
	mock := newFailingOneDriveOps()
	addr := &SimplexAddr{maxBodySize: MaxOneDriveMessageSize}

	mock.mu.Lock()
	mock.writeFailCount = 100
	mock.mu.Unlock()

	ctx, cancel := newTestContext()

	cfg := oneDriveFileTransportConfig()
	cfg.Interval = 10 * time.Millisecond
	cfg.MaxBodySize = MaxOneDriveMessageSize

	buffer := NewSimplexBuffer(addr)
	ftc := NewFileTransportClient(mock, cfg, buffer, "dir/errprop_send.txt", "dir/errprop_recv.txt", addr, ctx, cancel)

	pkts := NewSimplexPacketWithMaxSize([]byte("will fail"), SimplexPacketTypeDATA, MaxOneDriveMessageSize)
	ftc.Send(pkts, addr)

	go ftc.monitoring()

	select {
	case <-ctx.Done():
		// Expected
	case <-time.After(5 * time.Second):
		cancel()
		t.Fatal("Timeout: monitoring did not cancel context after persistent failures")
	}

	_, _, err := ftc.Receive()
	if err == nil {
		t.Error("Expected error from Receive after context cancelled")
	}

	_, err = ftc.Send(pkts, addr)
	if err == nil {
		t.Error("Expected error from Send after context cancelled")
	}
}

// E2E tests using FakeGraphAPIServer
func TestOneDrive_Restart_NewClientID(t *testing.T) {
	fake := NewFakeGraphAPIServer()
	fake.Install(t)

	prefix := "restart-new-" + randomString(6)

	server, err := NewOneDriveServer("onedrive", oneDriveServerURL(prefix))
	if err != nil {
		t.Fatalf("NewOneDriveServer error: %v", err)
	}
	defer server.Close()

	// Client 1
	clientURL1 := fmt.Sprintf("%s&client_id=cli1", oneDriveClientURL(prefix))
	addr1, _ := ResolveOneDriveAddr("onedrive", clientURL1)
	client1, err := NewOneDriveClient(addr1)
	if err != nil {
		t.Fatalf("NewOneDriveClient error: %v", err)
	}

	pkts1 := NewSimplexPacketWithMaxSize([]byte("from client1"), SimplexPacketTypeDATA, MaxOneDriveMessageSize)
	client1.Send(pkts1, addr1)

	pkt1, _ := oneDrivePollServerUntilReceived(t, server)
	if string(pkt1.Data) != "from client1" {
		t.Errorf("got %q, want %q", string(pkt1.Data), "from client1")
	}

	client1.Close()

	// Client 2 (new clientID)
	clientURL2 := fmt.Sprintf("%s&client_id=cli2", oneDriveClientURL(prefix))
	addr2, _ := ResolveOneDriveAddr("onedrive", clientURL2)
	client2, err := NewOneDriveClient(addr2)
	if err != nil {
		t.Fatalf("NewOneDriveClient error: %v", err)
	}
	defer client2.Close()

	pkts2 := NewSimplexPacketWithMaxSize([]byte("from client2"), SimplexPacketTypeDATA, MaxOneDriveMessageSize)
	client2.Send(pkts2, addr2)

	pkt2, _ := oneDrivePollServerUntilReceived(t, server)
	if string(pkt2.Data) != "from client2" {
		t.Errorf("got %q, want %q", string(pkt2.Data), "from client2")
	}
}

func TestOneDrive_Restart_SameClientID(t *testing.T) {
	fake := NewFakeGraphAPIServer()
	fake.Install(t)

	prefix := "restart-same-" + randomString(6)

	server, err := NewOneDriveServer("onedrive", oneDriveServerURL(prefix))
	if err != nil {
		t.Fatalf("NewOneDriveServer error: %v", err)
	}
	defer server.Close()

	fixedID := "persistent-cli"

	clientURL := fmt.Sprintf("%s&client_id=%s", oneDriveClientURL(prefix), fixedID)
	addr1, _ := ResolveOneDriveAddr("onedrive", clientURL)
	client1, err := NewOneDriveClient(addr1)
	if err != nil {
		t.Fatalf("NewOneDriveClient error: %v", err)
	}

	pkts1 := NewSimplexPacketWithMaxSize([]byte("session1"), SimplexPacketTypeDATA, MaxOneDriveMessageSize)
	client1.Send(pkts1, addr1)

	pkt1, _ := oneDrivePollServerUntilReceived(t, server)
	if string(pkt1.Data) != "session1" {
		t.Errorf("got %q, want %q", string(pkt1.Data), "session1")
	}

	client1.Close()

	// Wait for idle timeout cleanup (50ms interval × 150 = 7.5s, add margin)
	idleTimeout := 50*time.Millisecond*time.Duration(oneDriveHandlerIdleMultiplier) + 2*time.Second
	t.Logf("Waiting %v for handler idle cleanup...", idleTimeout)
	time.Sleep(idleTimeout)

	// Verify handler was cleaned up — access via fts.clients
	_, exists := server.fts.clients.Load(fixedID)
	if exists {
		t.Fatal("Expected handler to be cleaned up after idle timeout")
	}

	// Client 2 (same clientID)
	addr2, _ := ResolveOneDriveAddr("onedrive", clientURL)
	client2, err := NewOneDriveClient(addr2)
	if err != nil {
		t.Fatalf("NewOneDriveClient error: %v", err)
	}
	defer client2.Close()

	pkts2 := NewSimplexPacketWithMaxSize([]byte("session2"), SimplexPacketTypeDATA, MaxOneDriveMessageSize)
	client2.Send(pkts2, addr2)

	pkt2, _ := oneDrivePollServerUntilReceived(t, server)
	if string(pkt2.Data) != "session2" {
		t.Errorf("got %q, want %q", string(pkt2.Data), "session2")
	}
}

// --- helper ---

func newTestContext() (context.Context, context.CancelFunc) {
	return context.WithCancel(context.Background())
}

func oneDriveTestAddr() *SimplexAddr {
	u, _ := url.Parse("onedrive:///testdir")
	return &SimplexAddr{URL: u, maxBodySize: MaxOneDriveMessageSize, options: u.Query()}
}

// ============================================================
// Zombie State Verification Tests
// ============================================================

type oneDriveRecvFailOps struct {
	*mockOneDriveOps
	failPath string
}

func (m *oneDriveRecvFailOps) ReadFile(path string) ([]byte, error) {
	if path == m.failPath {
		return nil, fmt.Errorf("simulated recv pull failure for %s", path)
	}
	return m.mockOneDriveOps.ReadFile(path)
}

func (m *oneDriveRecvFailOps) WriteFile(path string, data []byte) error {
	return m.mockOneDriveOps.WriteFile(path, data)
}

func (m *oneDriveRecvFailOps) DeleteFile(path string) error {
	return m.mockOneDriveOps.DeleteFile(path)
}

func (m *oneDriveRecvFailOps) ListFiles(prefix string) ([]string, error) {
	return m.mockOneDriveOps.ListFiles(prefix)
}

func (m *oneDriveRecvFailOps) FileExists(path string) (bool, error) {
	return m.mockOneDriveOps.FileExists(path)
}

func TestZombie_Client_StaleSendFile_NeverCancels(t *testing.T) {
	mock := newMockOneDriveOps()
	addr := &SimplexAddr{maxBodySize: MaxOneDriveMessageSize}

	mock.putFile("dir/test_send.txt", []byte("old data from previous upload"))

	ctx, cancel := newTestContext()
	defer cancel()

	cfg := oneDriveFileTransportConfig()
	cfg.Interval = 20 * time.Millisecond
	cfg.MaxBodySize = MaxOneDriveMessageSize

	ftc := NewFileTransportClient(mock, cfg, NewSimplexBuffer(addr), "dir/test_send.txt", "dir/test_recv.txt", addr, ctx, cancel)

	pkts := NewSimplexPacketWithMaxSize([]byte("new data to send"), SimplexPacketTypeDATA, MaxOneDriveMessageSize)
	ftc.Send(pkts, addr)

	go ftc.monitoring()

	select {
	case <-ctx.Done():
		// Expected
	case <-time.After(2 * time.Second):
		t.Fatal("TIMEOUT: monitoring did not cancel ctx after stale sendFile — fix not working")
	}

	t.Log("FIXED: Zombie #1 — stale sendFile now detected and ctx cancelled via sendStallCount")
}

func TestZombie_Server_OutBufferDataLoss_OnPushFileFail(t *testing.T) {
	mock := newFailingOneDriveOps()

	mock.mu.Lock()
	mock.writeFailCount = 100
	mock.mu.Unlock()

	addr := oneDriveTestAddr()

	clientAddr := generateAddrFromPath("zombie2", addr)
	state := &fileClientState{
		inBuffer:     NewSimplexBuffer(clientAddr),
		outBuffer:    NewSimplexBuffer(clientAddr),
		addr:         clientAddr,
		lastActivity: time.Now(),
	}

	cfg := oneDriveFileTransportConfig()
	cfg.Interval = 20 * time.Millisecond
	cfg.MaxBodySize = MaxOneDriveMessageSize

	ctx, ctxCancel := context.WithCancel(context.Background())
	srv := NewFileTransportServer(mock, "testdir/", cfg, addr, ctx, ctxCancel)

	clientID := "zombie2"
	sendFile := fmt.Sprintf("testdir/%s_send.txt", clientID)

	mock.putFile(sendFile, []byte("keepalive"))

	for i := 0; i < 3; i++ {
		pkt := NewSimplexPacket(SimplexPacketTypeDATA, []byte(fmt.Sprintf("data-%d", i)))
		state.outBuffer.PutPacket(pkt)
	}

	go srv.handleClient(ctx, clientID, state)

	time.Sleep(200 * time.Millisecond)

	ctxCancel()
	time.Sleep(50 * time.Millisecond)

	var recovered int
	for {
		pkt, _ := state.outBuffer.GetPacket()
		if pkt == nil {
			break
		}
		recovered++
	}
	if recovered == 0 {
		t.Fatal("FAIL: outBuffer is empty — data was lost despite put-back fix")
	}

	t.Logf("FIXED: Zombie #2 — %d packets preserved in outBuffer after PushFile failures (put-back working)", recovered)
}

func TestZombie_Client_RecvSilentFailure_NeverCancels(t *testing.T) {
	baseMock := newMockOneDriveOps()
	mock := &oneDriveRecvFailOps{
		mockOneDriveOps: baseMock,
		failPath:        "dir/test_recv.txt",
	}

	addr := &SimplexAddr{maxBodySize: MaxOneDriveMessageSize}

	ctx, cancel := newTestContext()
	defer cancel()

	cfg := oneDriveFileTransportConfig()
	cfg.Interval = 20 * time.Millisecond
	cfg.MaxBodySize = MaxOneDriveMessageSize

	ftc := NewFileTransportClient(mock, cfg, NewSimplexBuffer(addr), "dir/test_send.txt", "dir/test_recv.txt", addr, ctx, cancel)

	go ftc.monitoring()

	select {
	case <-ctx.Done():
		// Expected
	case <-time.After(2 * time.Second):
		t.Fatal("TIMEOUT: monitoring did not cancel ctx after persistent recv failures — fix not working")
	}

	_, err := mock.ReadFile("dir/test_recv.txt")
	if errors.Is(err, os.ErrNotExist) {
		t.Fatal("recvFailOps should NOT return os.ErrNotExist — it simulates a real API error")
	}

	t.Log("FIXED: Zombie #3 — recv-direction non-404 errors now tracked and ctx cancelled via recvFailures")
}
