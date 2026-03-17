//go:build graph
// +build graph

package simplex

import (
	"fmt"
	"testing"
	"time"
)

// ============================================================
// OneDrive Real API Tests — 使用真实 Microsoft Graph API
//
// 复用 SharePoint 的 Azure AD 凭据 (TestTenantID, TestClientID, etc.)
//
// 运行示例:
//   go test -v -tags graph -run "TestOneDriveReal" ./x/simplex/ -timeout 120s
// ============================================================

// oneDriveRealServerURL builds a real-API OneDrive server URL
func oneDriveRealServerURL(prefix string) string {
	return fmt.Sprintf("onedrive:///?tenant=%s&client=%s&secret=%s&site=%s&prefix=%s&interval=3000",
		TestTenantID, TestClientID, TestClientSecret, TestSiteID, prefix)
}

// oneDriveRealClientURL builds a real-API OneDrive client URL
func oneDriveRealClientURL(prefix string) string {
	return fmt.Sprintf("onedrive:///?tenant=%s&client=%s&secret=%s&site=%s&prefix=%s&interval=3000",
		TestTenantID, TestClientID, TestClientSecret, TestSiteID, prefix)
}

// TestOneDriveReal_E2E_FullRoundTrip 使用真实 Graph API 进行完整双向通信测试
func TestOneDriveReal_E2E_FullRoundTrip(t *testing.T) {
	prefix := "rem-test-" + randomString(6)

	t.Logf("Starting real OneDrive E2E test with prefix: %s", prefix)

	server, err := NewOneDriveServer("onedrive", oneDriveRealServerURL(prefix))
	if err != nil {
		t.Fatalf("NewOneDriveServer error: %v", err)
	}
	defer server.Close()
	t.Logf("Server created, polling directory: %s", prefix)

	addr, err := ResolveOneDriveAddr("onedrive", oneDriveRealClientURL(prefix))
	if err != nil {
		t.Fatalf("ResolveOneDriveAddr error: %v", err)
	}

	client, err := NewOneDriveClient(addr)
	if err != nil {
		t.Fatalf("NewOneDriveClient error: %v", err)
	}
	defer client.Close()
	t.Logf("Client created successfully")

	// === Phase 1: Client → Server ===
	clientMsg := fmt.Sprintf(`{"type":"test","ts":%d,"msg":"hello from onedrive client"}`, time.Now().Unix())
	clientPkts := NewSimplexPacketWithMaxSize([]byte(clientMsg), SimplexPacketTypeDATA, MaxOneDriveMessageSize)

	n, err := client.Send(clientPkts, addr)
	if err != nil {
		t.Fatalf("Client.Send error: %v", err)
	}
	t.Logf("Client sent %d bytes", n)

	// 等待服务端接收（真实 API 延迟更高，超时 60s）
	timeout := time.After(60 * time.Second)
	var serverRecvAddr *SimplexAddr
	received := false

	for !received {
		select {
		case <-timeout:
			t.Fatal("Timeout waiting for server to receive data from real OneDrive API")
		default:
			pkt, recvAddr, err := server.Receive()
			if err != nil {
				t.Logf("Server.Receive error (may be transient): %v", err)
				time.Sleep(2 * time.Second)
				continue
			}
			if pkt != nil {
				t.Logf("Server received: %q from %s", string(pkt.Data), recvAddr.Path)
				if string(pkt.Data) != clientMsg {
					t.Errorf("Server received %q, want %q", string(pkt.Data), clientMsg)
				}
				serverRecvAddr = recvAddr
				received = true
			} else {
				time.Sleep(1 * time.Second)
			}
		}
	}

	// === Phase 2: Server → Client ===
	serverMsg := fmt.Sprintf(`{"type":"response","ts":%d,"msg":"hello from onedrive server"}`, time.Now().Unix())
	serverPkts := NewSimplexPacketWithMaxSize([]byte(serverMsg), SimplexPacketTypeDATA, MaxOneDriveMessageSize)

	n, err = server.Send(serverPkts, serverRecvAddr)
	if err != nil {
		t.Fatalf("Server.Send error: %v", err)
	}
	t.Logf("Server sent %d bytes", n)

	// 等待客户端接收
	clientTimeout := time.After(60 * time.Second)
	clientReceived := false

	for !clientReceived {
		select {
		case <-clientTimeout:
			t.Fatal("Timeout waiting for client to receive response from real OneDrive API")
		default:
			respPkt, _, err := client.Receive()
			if err != nil {
				t.Logf("Client.Receive error (may be transient): %v", err)
				time.Sleep(2 * time.Second)
				continue
			}
			if respPkt != nil {
				t.Logf("Client received: %q", string(respPkt.Data))
				if string(respPkt.Data) != serverMsg {
					t.Errorf("Client received %q, want %q", string(respPkt.Data), serverMsg)
				}
				clientReceived = true
			} else {
				time.Sleep(1 * time.Second)
			}
		}
	}

	t.Log("Real OneDrive E2E roundtrip test PASSED")
}

// TestOneDriveReal_E2E_MultipleMessages 使用真实 API 发送多条消息
func TestOneDriveReal_E2E_MultipleMessages(t *testing.T) {
	prefix := "rem-multi-" + randomString(6)

	t.Logf("Starting real OneDrive multi-message test with prefix: %s", prefix)

	server, err := NewOneDriveServer("onedrive", oneDriveRealServerURL(prefix))
	if err != nil {
		t.Fatalf("NewOneDriveServer error: %v", err)
	}
	defer server.Close()

	addr, err := ResolveOneDriveAddr("onedrive", oneDriveRealClientURL(prefix))
	if err != nil {
		t.Fatalf("ResolveOneDriveAddr error: %v", err)
	}

	client, err := NewOneDriveClient(addr)
	if err != nil {
		t.Fatalf("NewOneDriveClient error: %v", err)
	}
	defer client.Close()

	// 发送 3 条消息，逐条发送并等待接收
	for i := 1; i <= 3; i++ {
		msg := fmt.Sprintf("real-api-msg-%d", i)
		pkts := NewSimplexPacketWithMaxSize([]byte(msg), SimplexPacketTypeDATA, MaxOneDriveMessageSize)
		_, err := client.Send(pkts, addr)
		if err != nil {
			t.Fatalf("Send[%d] error: %v", i, err)
		}
		t.Logf("Sent message %d: %s", i, msg)

		// 等待服务端接收
		timeout := time.After(30 * time.Second)
		received := false
		for !received {
			select {
			case <-timeout:
				t.Fatalf("Timeout waiting for message %d", i)
			default:
				pkt, _, err := server.Receive()
				if err != nil {
					t.Fatalf("Receive error: %v", err)
				}
				if pkt != nil {
					t.Logf("Server received message %d: %q", i, string(pkt.Data))
					if string(pkt.Data) != msg {
						t.Errorf("Message %d: got %q, want %q", i, string(pkt.Data), msg)
					}
					received = true
				} else {
					time.Sleep(1 * time.Second)
				}
			}
		}
	}

	t.Log("Real OneDrive multi-message test PASSED")
}
