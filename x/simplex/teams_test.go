//go:build teams
// +build teams

package simplex

import (
	"bytes"
	"encoding/base64"
	"fmt"
	"net/http"
	"net/url"
	"testing"
)

var (
	AuthToken  = "Bearer eyJ0eXAiOiJKV1QiLCJub25jZSI6IjZQYTFZZVdNVFFHeVU5TWZOSjdGcXRQUFlXRTJiMkhkNFZBOEhWUm5Rc3MiLCJhbGciOiJSUzI1NiIsIng1dCI6Il9qTndqZVNudlRUSzhYRWRyNVFVUGtCUkxMbyIsImtpZCI6Il9qTndqZVNudlRUSzhYRWRyNVFVUGtCUkxMbyJ9.eyJhdWQiOiJodHRwczovL2ljMy50ZWFtcy5vZmZpY2UuY29tIiwiaXNzIjoiaHR0cHM6Ly9zdHMud2luZG93cy5uZXQvY2I4MjAyMDAtZDBkNi00NTRiLTgwYmMtM2YyMjBkMTZkNGYwLyIsImlhdCI6MTc1MzAyNzM5MiwibmJmIjoxNzUzMDI3MzkyLCJleHAiOjE3NTMxMTQwOTIsImFjY3QiOjAsImFjciI6IjEiLCJhaW8iOiJBVVFBdS84WkFBQUFLcUFjM0N4V2xWSzlST0xmYlpuUzVvWTkxNFRTcjl1dXNZQW43dW9CWGErZFNBbmhIZHY0MHRjcXduaHAxRVdiNkxYUVo5Wjk3MDh6aVhZUzhvNjlkdz09IiwiYW1yIjpbInB3ZCJdLCJhcHBpZCI6IjVlM2NlNmMwLTJiMWYtNDI4NS04ZDRiLTc1ZWU3ODc4NzM0NiIsImFwcGlkYWNyIjoiMCIsImdpdmVuX25hbWUiOiJpaSIsImlkdHlwIjoidXNlciIsImlwYWRkciI6Ijg5LjIxMy4xNzkuMTMzIiwibmFtZSI6ImlpIiwib2lkIjoiMDA3ZjlkMDAtNDZkZS00NjU5LWFlM2EtY2RhNjMwYTE0OTA4IiwicHVpZCI6IjEwMDMyMDA0RDM4MTMzRDYiLCJyaCI6IjEuQVh3QUFBS0N5OWJRUzBXQXZEOGlEUmJVOEZUd3FqbWxnY2RJcFBnQ2t3RWdsYm03QUNWOEFBLiIsInNjcCI6IlRlYW1zLkFjY2Vzc0FzVXNlci5BbGwiLCJzaWQiOiIwMDZkZTRlOS1jY2FiLTllMmYtNzdlOC02ZTI3OTExNTlkZjIiLCJzdWIiOiJKYm5mRFVmMTZZT1Q4SzZzOUlSM0ZTUTNFN196N1I3bHluTFI2ajBKd3pjIiwidGlkIjoiY2I4MjAyMDAtZDBkNi00NTRiLTgwYmMtM2YyMjBkMTZkNGYwIiwidW5pcXVlX25hbWUiOiJpaUB2Ny50bTkuc2l0ZSIsInVwbiI6ImlpQHY3LnRtOS5zaXRlIiwidXRpIjoiSkRfb2tVcHNQMGFXTTQ2Y0dFQXJBQSIsInZlciI6IjEuMCIsInhtc19jYyI6WyJDUDEiXSwieG1zX2Z0ZCI6IkF4YjhtZGJwUXNNX1R0YWM5VzB0Mng1WUtjM2pEOWNTV01wZHk2aWtBMVlCZFhOdWIzSjBhQzFrYzIxeiIsInhtc19pZHJlbCI6IjEgMiIsInhtc19zc20iOiIxIn0.LiKmaFCM4NY8LORZWNpRnhP2P78cleZeKNDaNEvUU3IojNB30WvqsNlCAEZaa3GintW9gJW3qn8R7SpEg7RCwoajo2hFiTalIVOgUhuXDg264KnWAf3twLRGTMOs0iTnIt3XEW423n2kMkJeee7TueqqqZMGthIm6Z3LPXOzWaCvftjY5sVV2JPedOIFx7709M5NwXawDoYcFeyOIR6f28NV-UPIM4rYXpqWf-bQH76e-lcG1Y97Ux9i9NDMgGsEo7tijJ4cHbBk7Wp3-y5Tdsw5ApQPAAsuNK0o2nrW7VHDhzR66GAsDxNp2N6cs-Jn6dNTviLTdftX6B7ATG2Fqw"
	WebHookURL = "https://prod-48.westus.logic.azure.com:443/workflows/7a26f333c45a4703908cedbe1331658c/triggers/manual/paths/invoke?api-version=2016-06-01&sp=%2Ftriggers%2Fmanual%2Frun&sv=1.0&sig=G6V1PQOAzCHEnr68e6dAx7_sBR-PeAr9gKEOcbIIC6I"
	ChatURL    = "https://teams.microsoft.com/api/chatsvc/amer/v1/users/ME/conversations/19:uni01_dywvpc3vflwqdth7g2goyxisz3d5w2mcz3ii26eqfvuyqtnlx56a@thread.v2/messages"
	ServerURL  = "http://127.0.0.1:8123"
)

func NewPackets(data []byte) *SimplexPackets {
	pkts := &SimplexPackets{}
	for len(data) > 0 {
		size := len(data)
		if size > MaxTeamsMessageSize {
			size = MaxTeamsMessageSize
		}
		pkt := NewSimplexPacket(SimplexPacketTypeDATA, data)
		pkts.Append(pkt)
		data = data[size:]
	}
	return pkts
}

func Test_createChatThread(t *testing.T) {
	thread, err := createChatThread(
		"8:orgid:007f9d00-46de-4659-ae3a-cda630a14908",
		"8:live:.cid.343e94fe9c4bd24a",
		AuthToken,
		&http.Client{},
	)
	fmt.Println(thread, err)
}

func TestTeamsClientSend(t *testing.T) {
	// Create client with real webhook URL
	options := url.Values{}
	options.Set("webhook", WebHookURL)
	addr, err := ResolveTeamsAddr("teams", "simplex+teams://nonenonenonenone:@127.0.0.1/?wrapper=raw&webhook="+url.QueryEscape(WebHookURL))

	client, err := NewTeamsClient(addr)
	if err != nil {
		t.Fatalf("NewTeamsClient() error = %v", err)
	}
	defer client.Close()

	// Test data
	testData := []byte("test command data")

	// Create SimplexPackets
	pkts := NewPackets(testData)

	// Send data
	sent, err := client.Send(pkts, addr)
	if err != nil {
		t.Fatalf("Client.Send() error = %v", err)
	}

	if sent != len(testData) {
		t.Errorf("Client.Send() sent %d bytes, expected %d", sent, len(testData))
	}

	t.Logf("Successfully sent %d bytes via Teams webhook", sent)
}

func TestTeamsServerSend(t *testing.T) {
	// Skip if no auth token or chat URL
	if AuthToken == "" || ChatURL == "" {
		t.Skip("Skipping server test - no auth token or chat URL configured")
	}

	// Create server with real chat URL and token
	address := fmt.Sprintf("teams://test?chat=%s&token=%s",
		url.QueryEscape(ChatURL), url.QueryEscape(AuthToken))

	server, err := NewTeamsServer("teams", address)
	if err != nil {
		t.Fatalf("NewTeamsServer() error = %v", err)
	}
	defer server.Close()

	// Create test address
	testAddr := &SimplexAddr{
		id:          "/test-client",
		maxBodySize: MaxTeamsMessageSize,
	}

	// Test data
	testData := []byte("server response data")

	// Create SimplexPackets
	pkts := NewPackets(testData)

	// Send data
	sent, err := server.Send(pkts, testAddr)
	if err != nil {
		t.Fatalf("Server.Send() error = %v", err)
	}

	if sent != len(testData) {
		t.Errorf("Server.Send() sent %d bytes, expected %d", sent, len(testData))
	}

	t.Logf("Successfully sent %d bytes via Teams chat API", sent)
}

func TestDataEncodingIntegrity(t *testing.T) {
	tests := []struct {
		name string
		data []byte
	}{
		{"simple text", []byte("hello world")},
		{"binary data", []byte{0x00, 0x01, 0x02, 0xFF, 0xFE, 0xFD}},
		{"json data", []byte(`{"cmd":"exec","args":["whoami"]}`)},
		{"large data", make([]byte, 1024)},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			// Fill large data with pattern
			if len(tt.data) == 1024 {
				for i := range tt.data {
					tt.data[i] = byte(i % 256)
				}
			}

			// Encode like client does
			encoded := base64.URLEncoding.EncodeToString(tt.data)

			// Decode back
			decoded, err := base64.URLEncoding.DecodeString(encoded)
			if err != nil {
				t.Fatalf("Failed to decode: %v", err)
			}

			// Verify integrity
			if !bytes.Equal(tt.data, decoded) {
				t.Errorf("Data integrity check failed")
				t.Logf("Original: %x", tt.data[:min(len(tt.data), 32)])
				t.Logf("Decoded:  %x", decoded[:min(len(decoded), 32)])
			}
		})
	}
}

func TestTeamsE2ECommunication(t *testing.T) {
	if WebHookURL == "" || ChatURL == "" || AuthToken == "" {
		t.Skip("Skipping E2E test - missing required URLs or tokens")
	}

	// Create server
	serverAddr := fmt.Sprintf("teams://test?chat=%s&token=%s",
		url.QueryEscape(ChatURL), url.QueryEscape(AuthToken))

	server, err := NewTeamsServer("teams", serverAddr)
	if err != nil {
		t.Fatalf("NewTeamsServer() error = %v", err)
	}
	defer server.Close()

	// Create client
	clientOptions := url.Values{}
	clientOptions.Set("webhook", WebHookURL)

	clientAddr := &SimplexAddr{
		id:          "/test-client",
		maxBodySize: MaxTeamsMessageSize,
		options:     clientOptions,
	}

	client, err := NewTeamsClient(clientAddr)
	if err != nil {
		t.Fatalf("NewTeamsClient() error = %v", err)
	}
	defer client.Close()

	// Test client -> server communication
	clientData := []byte(`{"type":"heartbeat","timestamp":1234567890}`)
	clientPkts := NewPackets(clientData)

	sent, err := client.Send(clientPkts, clientAddr)
	if err != nil {
		t.Fatalf("Client send failed: %v", err)
	}
	t.Logf("Client sent %d bytes", sent)

	// Test server -> client communication
	serverData := []byte(`{"type":"command","cmd":"whoami"}`)
	serverPkts := NewPackets(serverData)

	sent, err = server.Send(serverPkts, clientAddr)
	if err != nil {
		t.Fatalf("Server send failed: %v", err)
	}
	t.Logf("Server sent %d bytes", sent)

	t.Log("E2E communication test completed successfully")
}

func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}
