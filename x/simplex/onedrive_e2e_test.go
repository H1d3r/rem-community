//go:build graph
// +build graph

package simplex

import (
	"fmt"
	"testing"
	"time"
)

// ============================================================
// OneDrive E2E Tests — 使用 FakeGraphAPIServer
//
// 运行示例:
//   go test -v -tags graph -run "TestOneDrive" ./x/simplex/
// ============================================================

// oneDriveServerURL builds the OneDrive server creation URL (mock mode)
func oneDriveServerURL(prefix string) string {
	return fmt.Sprintf("onedrive:///?tenant=t&client=c&secret=s&site=site-id&prefix=%s&interval=50", prefix)
}

// oneDriveClientURL builds the OneDrive client connection URL (mock mode)
func oneDriveClientURL(prefix string) string {
	return fmt.Sprintf("onedrive:///?tenant=t&client=c&secret=s&site=site-id&prefix=%s&interval=50", prefix)
}

func TestOneDrive_ResolveAddr(t *testing.T) {
	address := "onedrive:///mydir?tenant=t&client=c&secret=s&site=site-id&interval=3000"
	addr, err := ResolveOneDriveAddr("onedrive", address)
	if err != nil {
		t.Fatalf("ResolveOneDriveAddr() error = %v", err)
	}

	if addr.Network() != "onedrive" {
		t.Errorf("Expected network 'onedrive', got '%s'", addr.Network())
	}

	if addr.Interval() != 3*time.Second {
		t.Errorf("Expected interval 3s, got %v", addr.Interval())
	}

	if addr.ID() != "mydir" {
		t.Errorf("Expected id 'mydir', got '%s'", addr.ID())
	}
}

func TestOneDrive_ResolveAddr_DefaultInterval(t *testing.T) {
	address := "onedrive:///mydir?tenant=t&client=c&secret=s&site=site-id"
	addr, err := ResolveOneDriveAddr("onedrive", address)
	if err != nil {
		t.Fatalf("ResolveOneDriveAddr() error = %v", err)
	}

	if addr.Interval() != time.Duration(DefaultOneDriveInterval)*time.Millisecond {
		t.Errorf("Expected default interval %dms, got %v", DefaultOneDriveInterval, addr.Interval())
	}
}

func TestOneDrive_ResolveAddr_RandomPath(t *testing.T) {
	address := "onedrive:///?tenant=t&client=c&secret=s&site=site-id"
	addr, err := ResolveOneDriveAddr("onedrive", address)
	if err != nil {
		t.Fatalf("ResolveOneDriveAddr() error = %v", err)
	}

	if addr.ID() == "" {
		t.Error("Expected non-empty random id when path is empty")
	}
	if len(addr.ID()) != 8 {
		t.Errorf("Expected 8-char random id, got %d chars: %s", len(addr.ID()), addr.ID())
	}
}

func TestOneDrive_MissingCredentials(t *testing.T) {
	fake := NewFakeGraphAPIServer()
	fake.Install(t)

	_, err := NewOneDriveServer("onedrive", "onedrive:///?tenant=t&client=c&secret=s&interval=50")
	if err == nil {
		t.Fatal("Expected error for missing site credential")
	}
}

// TestOneDrive_E2E_FullRoundTrip Client → Server → Client full bidirectional communication
func TestOneDrive_E2E_FullRoundTrip(t *testing.T) {
	fake := NewFakeGraphAPIServer()
	fake.Install(t)

	prefix := "e2e-roundtrip-" + randomString(6)

	server, err := NewOneDriveServer("onedrive", oneDriveServerURL(prefix))
	if err != nil {
		t.Fatalf("NewOneDriveServer error: %v", err)
	}
	defer server.Close()

	addr, err := ResolveOneDriveAddr("onedrive", oneDriveClientURL(prefix))
	if err != nil {
		t.Fatalf("ResolveOneDriveAddr error: %v", err)
	}

	client, err := NewOneDriveClient(addr)
	if err != nil {
		t.Fatalf("NewOneDriveClient error: %v", err)
	}
	defer client.Close()

	t.Logf("Client created successfully")

	// Client → Server
	sendData := []byte("hello from onedrive client")
	sendPkts := NewSimplexPacketWithMaxSize(sendData, SimplexPacketTypeDATA, MaxOneDriveMessageSize)
	n, err := client.Send(sendPkts, addr)
	if err != nil {
		t.Fatalf("Client.Send error: %v", err)
	}
	t.Logf("Client sent %d bytes", n)

	// Wait for client monitoring to push the file, then server to discover and process
	pkt, recvAddr := oneDrivePollServerUntilReceived(t, server)
	if string(pkt.Data) != "hello from onedrive client" {
		t.Errorf("Server received %q, want %q", string(pkt.Data), "hello from onedrive client")
	}
	t.Logf("Server received: %q from %s", string(pkt.Data), recvAddr.Path)

	// Server → Client
	respData := []byte("hello from onedrive server")
	respPkts := NewSimplexPacketWithMaxSize(respData, SimplexPacketTypeDATA, MaxOneDriveMessageSize)
	n, err = server.Send(respPkts, recvAddr)
	if err != nil {
		t.Fatalf("Server.Send error: %v", err)
	}
	t.Logf("Server sent %d bytes", n)

	// Wait for handleClient to write recvFile, then client monitoring to read it
	respPkt := oneDrivePollClientUntilReceived(t, client)
	if string(respPkt.Data) != "hello from onedrive server" {
		t.Errorf("Client received %q, want %q", string(respPkt.Data), "hello from onedrive server")
	}
	t.Logf("Client received: %q", string(respPkt.Data))

	t.Logf("OneDrive E2E roundtrip passed, files: %d", fake.FileCount())
}

// TestOneDrive_E2E_MultipleClients multiple clients concurrently
func TestOneDrive_E2E_MultipleClients(t *testing.T) {
	fake := NewFakeGraphAPIServer()
	fake.Install(t)

	prefix := "e2e-multi-" + randomString(6)

	server, err := NewOneDriveServer("onedrive", oneDriveServerURL(prefix))
	if err != nil {
		t.Fatalf("NewOneDriveServer error: %v", err)
	}
	defer server.Close()

	const numClients = 3
	clients := make([]*OneDriveClient, numClients)
	for i := 0; i < numClients; i++ {
		clientURL := fmt.Sprintf("%s&client_id=mc%d", oneDriveClientURL(prefix), i)
		cAddr, _ := ResolveOneDriveAddr("onedrive", clientURL)
		clients[i], err = NewOneDriveClient(cAddr)
		if err != nil {
			t.Fatalf("NewOneDriveClient[%d] error: %v", i, err)
		}
		defer clients[i].Close()
	}

	// Each client sends a unique message
	for i, client := range clients {
		msg := fmt.Sprintf("data from client %d", i)
		pkts := NewSimplexPacketWithMaxSize([]byte(msg), SimplexPacketTypeDATA, MaxOneDriveMessageSize)
		_, err := client.Send(pkts, client.Addr())
		if err != nil {
			t.Fatalf("Client[%d].Send error: %v", i, err)
		}
	}

	// Collect all messages on server side
	received := make(map[string]bool)
	deadline := time.Now().Add(10 * time.Second)
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
		expected := fmt.Sprintf("data from client %d", i)
		if !received[expected] {
			t.Errorf("Missing data: %q", expected)
		}
	}

	clientCount := 0
	server.fts.clients.Range(func(_, _ interface{}) bool {
		clientCount++
		return true
	})
	if clientCount != numClients {
		t.Errorf("Expected %d client states, got %d", numClients, clientCount)
	}

	t.Logf("Multi-client test passed: %d clients, files: %d", numClients, fake.FileCount())
}

// TestOneDrive_E2E_BidirectionalMultiRound multiple rounds of bidirectional communication
func TestOneDrive_E2E_BidirectionalMultiRound(t *testing.T) {
	fake := NewFakeGraphAPIServer()
	fake.Install(t)

	prefix := "e2e-bidir-" + randomString(6)

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

	rounds := 3
	for round := 0; round < rounds; round++ {
		// Client → Server
		clientMsg := fmt.Sprintf("client-round-%d", round)
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
		serverMsg := fmt.Sprintf("server-round-%d", round)
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

	t.Logf("%d-round bidirectional communication passed, files: %d, tokens: %d",
		rounds, fake.FileCount(), fake.TokenCount())
}

// TestOneDrive_E2E_LargeData transfer data approaching the size limit
func TestOneDrive_E2E_LargeData(t *testing.T) {
	fake := NewFakeGraphAPIServer()
	fake.Install(t)

	prefix := "e2e-large-" + randomString(6)

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

	// Create large data
	largeData := make([]byte, 40000)
	for i := range largeData {
		largeData[i] = byte(i % 256)
	}

	pkts := NewSimplexPacketWithMaxSize(largeData, SimplexPacketTypeDATA, MaxOneDriveMessageSize)
	_, err = client.Send(pkts, addr)
	if err != nil {
		t.Fatalf("Send large data error: %v", err)
	}

	pkt, _ := oneDrivePollServerUntilReceived(t, server)
	if len(pkt.Data) != len(largeData) {
		t.Errorf("Large data mismatch: sent %d bytes, received %d bytes", len(largeData), len(pkt.Data))
	}

	// Verify content
	for i := range largeData {
		if pkt.Data[i] != largeData[i] {
			t.Errorf("Data mismatch at byte %d: got %d, want %d", i, pkt.Data[i], largeData[i])
			break
		}
	}

	t.Logf("Large data transfer passed: %d bytes, files: %d", len(largeData), fake.FileCount())
}

// TestOneDrive_E2E_ConsecutiveSends verifies that consecutive sends don't lose data
func TestOneDrive_E2E_ConsecutiveSends(t *testing.T) {
	fake := NewFakeGraphAPIServer()
	fake.Install(t)

	prefix := "e2e-consec-" + randomString(6)

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

	// Send multiple messages rapidly (before monitoring has a chance to process)
	for i := 0; i < 3; i++ {
		msg := fmt.Sprintf("rapid-msg-%d", i)
		pkts := NewSimplexPacketWithMaxSize([]byte(msg), SimplexPacketTypeDATA, MaxOneDriveMessageSize)
		_, err := client.Send(pkts, addr)
		if err != nil {
			t.Fatalf("Send[%d] error: %v", i, err)
		}
	}

	// Wait and collect all messages
	received := make(map[string]bool)
	deadline := time.Now().Add(10 * time.Second)
	for len(received) < 3 && time.Now().Before(deadline) {
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

	for i := 0; i < 3; i++ {
		expected := fmt.Sprintf("rapid-msg-%d", i)
		if !received[expected] {
			t.Errorf("Missing data: %q", expected)
		}
	}

	t.Logf("Consecutive sends test passed: all %d messages received, files: %d", len(received), fake.FileCount())
}

// TestOneDrive_E2E_BufferSeparation verifies that inBuffer and outBuffer are correctly separated
// (regression test for the buffer mixing bug)
func TestOneDrive_E2E_BufferSeparation(t *testing.T) {
	fake := NewFakeGraphAPIServer()
	fake.Install(t)

	prefix := "e2e-bufsep-" + randomString(6)

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

	// Phase 1: Client sends
	pkts := NewSimplexPacketWithMaxSize([]byte("client-data"), SimplexPacketTypeDATA, MaxOneDriveMessageSize)
	client.Send(pkts, addr)

	pkt, recvAddr := oneDrivePollServerUntilReceived(t, server)
	if string(pkt.Data) != "client-data" {
		t.Fatalf("Phase 1: expected %q, got %q", "client-data", string(pkt.Data))
	}

	// Phase 2: Server sends — this should NOT appear in server's own Receive()
	respPkts := NewSimplexPacketWithMaxSize([]byte("server-data"), SimplexPacketTypeDATA, MaxOneDriveMessageSize)
	server.Send(respPkts, recvAddr)

	// Give some time for processing
	time.Sleep(300 * time.Millisecond)

	// Server Receive should NOT get the "server-data" back (that's outgoing, not incoming)
	extraPkt, _, _ := server.Receive()
	if extraPkt != nil {
		t.Errorf("Buffer separation violated: server got its own outgoing data back: %q", string(extraPkt.Data))
	}

	// Client should receive the server's response
	respPkt := oneDrivePollClientUntilReceived(t, client)
	if string(respPkt.Data) != "server-data" {
		t.Errorf("Phase 2: expected %q, got %q", "server-data", string(respPkt.Data))
	}

	t.Logf("Buffer separation test passed, files: %d", fake.FileCount())
}

// --- OneDrive test helpers ---

// oneDrivePollServerUntilReceived waits for the server to receive a packet via polling
func oneDrivePollServerUntilReceived(t *testing.T, server *OneDriveServer) (*SimplexPacket, *SimplexAddr) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		pkt, addr, err := server.Receive()
		if err != nil {
			t.Fatalf("Server.Receive error: %v", err)
		}
		if pkt != nil {
			return pkt, addr
		}
		time.Sleep(100 * time.Millisecond)
	}
	t.Fatal("Timeout waiting for OneDrive server to receive data")
	return nil, nil
}

// oneDrivePollClientUntilReceived waits for the client to receive a packet via polling
func oneDrivePollClientUntilReceived(t *testing.T, client *OneDriveClient) *SimplexPacket {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		pkt, _, err := client.Receive()
		if err != nil {
			t.Fatalf("Client.Receive error: %v", err)
		}
		if pkt != nil {
			return pkt
		}
		time.Sleep(100 * time.Millisecond)
	}
	t.Fatal("Timeout waiting for OneDrive client to receive data")
	return nil
}
