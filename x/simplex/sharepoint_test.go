//go:build graph
// +build graph

package simplex

import (
	"fmt"
	"os"
	"testing"
	"time"
)

// 测试用的SharePoint API配置 - 从环境变量读取或使用默认值
var (
	TestTenantID     = getEnvOrDefault("SHAREPOINT_TENANT_ID", "4194e4a5-cad1-4e7b-aeed-6928128121cc")
	TestClientID     = getEnvOrDefault("SHAREPOINT_CLIENT_ID", "ee056d17-bd75-4864-93d6-e502dfa61136")
	TestClientSecret = getEnvOrDefault("SHAREPOINT_CLIENT_SECRET", "REPLACE_WITH_YOUR_SECRET")
	TestSiteID       = getEnvOrDefault("SHAREPOINT_SITE_ID", "cloudnz395.sharepoint.com,6a5a7eb0-e088-40bc-8c17-c5993303883a,d27ad5c4-403c-4731-961a-6f56cd07c98e")
)

func getEnvOrDefault(envKey, defaultValue string) string {
	if value := os.Getenv(envKey); value != "" {
		return value
	}
	return defaultValue
}

func TestResolveSharePointAddr(t *testing.T) {
	address := fmt.Sprintf("sharepoint://test?tenant=%s&client=%s&secret=%s&site=%s&interval=3000",
		TestTenantID, TestClientID, TestClientSecret, TestSiteID)

	addr, err := ResolveSharePointAddr("sharepoint", address)
	if err != nil {
		t.Fatalf("ResolveSharePointAddr() error = %v", err)
	}

	if addr.Network() != "sharepoint" {
		t.Errorf("Expected network 'sharepoint', got '%s'", addr.Network())
	}

	if addr.Interval() != 3*time.Second {
		t.Errorf("Expected interval 3s, got %v", addr.Interval())
	}

	if getOption(addr, "tenant") != TestTenantID {
		t.Errorf("Expected tenant '%s', got '%s'", TestTenantID, getOption(addr, "tenant"))
	}
}

func TestSharePointE2ECommunication_RealAPI(t *testing.T) {
	// 使用固定的 ListName 进行测试
	listName := "test-e2e-" + randomString(8)

	// 首先创建服务端，它会创建 List
	serverAddr := fmt.Sprintf("sharepoint:///%s?tenant=%s&client=%s&secret=%s&site=%s",
		listName, TestTenantID, TestClientID, TestClientSecret, TestSiteID)

	server, err := NewSharePointServer("sharepoint", serverAddr)
	if err != nil {
		t.Fatalf("NewSharePointServer() error = %v", err)
	}
	defer server.Close()

	// 获取服务端创建的 ListID
	listID := server.addr.options.Get("list_id")
	if listID == "" {
		t.Fatalf("Server failed to create List and update list_id")
	}
	dataCol := server.addr.options.Get("data_col")
	t.Logf("Server created SharePoint List with ID: %s", listID)

	// 创建客户端，使用服务端创建的 ListID
	clientAddr, err := ResolveSharePointAddr("sharepoint", fmt.Sprintf("sharepoint:///%s?tenant=%s&client=%s&secret=%s&site=%s&list_id=%s&data_col=%s",
		listName, TestTenantID, TestClientID, TestClientSecret, TestSiteID, listID, dataCol))
	if err != nil {
		t.Fatalf("ResolveSharePointAddr() error = %v", err)
	}

	client, err := NewSharePointClient(clientAddr)
	if err != nil {
		t.Fatalf("NewSharePointClient() error = %v", err)
	}
	defer client.Close()

	// Test 1: 客户端发送 -> 服务端接收
	clientData := []byte(fmt.Sprintf(`{"type":"test","timestamp":%d,"message":"Hello from client"}`, time.Now().Unix()))
	clientPkts := NewSimplexPacketWithMaxSize(clientData, SimplexPacketTypeDATA, MaxSharePointMessageSize)

	sent, err := client.Send(clientPkts, clientAddr)
	if err != nil {
		t.Fatalf("Client send failed: %v", err)
	}
	t.Logf("Client sent %d bytes", sent)

	// 等待服务端接收
	timeout := time.After(30 * time.Second)
	received := false

	for !received {
		select {
		case <-timeout:
			t.Fatal("Timeout waiting for server to receive data")
		default:
			pkt, addr, err := server.Receive()
			if err != nil {
				t.Logf("Server receive error (may be expected): %v", err)
				time.Sleep(1 * time.Second)
				continue
			}
			if pkt != nil {
				t.Logf("Server received data from %s: %s", addr.String(), string(pkt.Data))
				received = true

				// Test 2: 服务端发送 -> 客户端接收
				serverData := []byte(fmt.Sprintf(`{"type":"response","timestamp":%d,"message":"Hello from server"}`, time.Now().Unix()))
				serverPkts := NewSimplexPacketWithMaxSize(serverData, SimplexPacketTypeDATA, MaxSharePointMessageSize)

				sent, err := server.Send(serverPkts, addr)
				if err != nil {
					t.Fatalf("Server send failed: %v", err)
				}
				t.Logf("Server sent %d bytes", sent)

				// 客户端尝试接收响应
				clientTimeout := time.After(30 * time.Second)
				clientReceived := false

				for !clientReceived {
					select {
					case <-clientTimeout:
						t.Fatal("Timeout waiting for client to receive response")
					default:
						respPkt, respAddr, err := client.Receive()
						if err != nil {
							t.Logf("Client receive error (may be expected): %v", err)
							time.Sleep(1 * time.Second)
							continue
						}
						if respPkt != nil {
							t.Logf("Client received response from %s: %s", respAddr.String(), string(respPkt.Data))
							clientReceived = true
						} else {
							time.Sleep(1 * time.Second)
						}
					}
				}
			} else {
				time.Sleep(1 * time.Second)
			}
		}
	}

	t.Log("E2E communication test completed successfully")
}

func TestSharePointDuplicateDataHandling(t *testing.T) {
	// 使用固定的 ListName 进行测试
	listName := "test-duplicate-" + randomString(8)

	// 首先创建服务端，它会创建 List
	serverAddr := fmt.Sprintf("sharepoint:///%s?tenant=%s&client=%s&secret=%s&site=%s",
		listName, TestTenantID, TestClientID, TestClientSecret, TestSiteID)

	server, err := NewSharePointServer("sharepoint", serverAddr)
	if err != nil {
		t.Fatalf("NewSharePointServer() error = %v", err)
	}
	defer server.Close()

	// 获取服务端创建的 ListID
	listID := server.addr.options.Get("list_id")
	if listID == "" {
		t.Fatalf("Server failed to create List and update list_id")
	}
	dataCol := server.addr.options.Get("data_col")
	t.Logf("Server created SharePoint List with ID: %s", listID)

	// 创建客户端，使用服务端创建的 ListID
	clientAddr, err := ResolveSharePointAddr("sharepoint", fmt.Sprintf("sharepoint:///%s?tenant=%s&client=%s&secret=%s&site=%s&list_id=%s&data_col=%s",
		listName, TestTenantID, TestClientID, TestClientSecret, TestSiteID, listID, dataCol))
	if err != nil {
		t.Fatalf("ResolveSharePointAddr() error = %v", err)
	}

	client, err := NewSharePointClient(clientAddr)
	if err != nil {
		t.Fatalf("NewSharePointClient() error = %v", err)
	}
	defer client.Close()

	// 发送多个数据包
	for i := 0; i < 3; i++ {
		data := []byte(fmt.Sprintf("test data %d", i))
		pkts := NewSimplexPacketWithMaxSize(data, SimplexPacketTypeDATA, MaxSharePointMessageSize)
		_, err := client.Send(pkts, clientAddr)
		if err != nil {
			t.Fatalf("Send failed: %v", err)
		}
		t.Logf("Sent packet %d", i)
	}

	// 记录收到的数据包
	receivedData := make(map[string]bool)
	timeout := time.After(30 * time.Second)
	done := false

	for !done {
		select {
		case <-timeout:
			t.Fatal("Timeout waiting for data")
		default:
			pkt, addr, err := server.Receive()
			if err != nil {
				t.Fatalf("Receive error: %v", err)
			}
			if pkt != nil {
				data := string(pkt.Data)
				if receivedData[data] {
					t.Errorf("Received duplicate data: %s", data)
				}
				receivedData[data] = true
				t.Logf("Received data from %s: %s", addr.String(), data)

				if len(receivedData) == 3 {
					done = true
				}
			} else {
				time.Sleep(1 * time.Second)
			}
		}
	}

	t.Log("All data received without duplicates")
}

func TestSharePointSequentialReading(t *testing.T) {
	// 使用固定的 ListName 进行测试
	listName := "test-sequential-" + randomString(8)

	// 首先创建服务端，它会创建 List
	serverAddr := fmt.Sprintf("sharepoint:///%s?tenant=%s&client=%s&secret=%s&site=%s",
		listName, TestTenantID, TestClientID, TestClientSecret, TestSiteID)

	server, err := NewSharePointServer("sharepoint", serverAddr)
	if err != nil {
		t.Fatalf("NewSharePointServer() error = %v", err)
	}
	defer server.Close()

	// 获取服务端创建的 ListID
	listID := server.addr.options.Get("list_id")
	if listID == "" {
		t.Fatalf("Server failed to create List and update list_id")
	}
	dataCol := server.addr.options.Get("data_col")
	t.Logf("Server created SharePoint List with ID: %s", listID)

	// 创建客户端，使用服务端创建的 ListID
	clientAddr, err := ResolveSharePointAddr("sharepoint", fmt.Sprintf("sharepoint:///%s?tenant=%s&client=%s&secret=%s&site=%s&list_id=%s&data_col=%s",
		listName, TestTenantID, TestClientID, TestClientSecret, TestSiteID, listID, dataCol))
	if err != nil {
		t.Fatalf("ResolveSharePointAddr() error = %v", err)
	}

	client, err := NewSharePointClient(clientAddr)
	if err != nil {
		t.Fatalf("NewSharePointClient() error = %v", err)
	}
	defer client.Close()

	// 发送多个数据包，每次发送后等待服务端接收
	for i := 1; i <= 3; i++ {
		data := []byte(fmt.Sprintf("test data %d", i))
		pkts := NewSimplexPacketWithMaxSize(data, SimplexPacketTypeDATA, MaxSharePointMessageSize)
		_, err := client.Send(pkts, clientAddr)
		if err != nil {
			t.Fatalf("Send failed: %v", err)
		}
		t.Logf("Sent packet %d", i)

		// 等待服务端接收这个数据包
		timeout := time.After(10 * time.Second)
		received := false
		for !received {
			select {
			case <-timeout:
				t.Fatalf("Timeout waiting for packet %d", i)
			default:
				pkt, addr, err := server.Receive()
				if err != nil {
					t.Fatalf("Receive error: %v", err)
				}
				if pkt != nil {
					// 验证数据是按顺序接收的
					expectedData := fmt.Sprintf("test data %d", i)
					if string(pkt.Data) != expectedData {
						t.Errorf("Expected data %q, got %q", expectedData, string(pkt.Data))
					}
					t.Logf("Received data from %s: %s", addr.String(), string(pkt.Data))
					received = true
				} else {
					time.Sleep(100 * time.Millisecond)
				}
			}
		}
	}

	t.Log("All data received in sequence")
}
