//go:build graph && !tinygo

package runner

import (
	"crypto/sha256"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	_ "github.com/chainreactors/rem/protocol/tunnel/simplex"
)

// ============================================================
// Boundary & Resilience Integration Tests
//
// Full-stack tests (agent → ARQ → simplex → transport) that verify
// recovery, idle resume, burst traffic, large payloads, and concurrency.
//
// These tests use SharePoint as the transport backend but test generic
// resilience properties that apply to any simplex transport.
//
// Usage:
//   SHAREPOINT_REAL=1 go test -v -tags graph -run "TestE2E_Boundary_" \
//     ./runner/ -count=1 -timeout=600s
// ============================================================

func skipIfNotSharePointReal(t *testing.T) {
	t.Helper()
	if os.Getenv("SHAREPOINT_REAL") != "1" {
		t.Skip("Skipping: requires SHAREPOINT_REAL=1")
	}
}

// TestE2E_Boundary_NetworkOutage kills the server, waits 60s, restarts,
// and verifies HTTP via SOCKS5 recovers.
func TestE2E_Boundary_NetworkOutage(t *testing.T) {
	skipIfNotSharePointReal(t)

	const target = "http://www.baidu.com"
	listName := fmt.Sprintf("rem-outage-%d", time.Now().UnixNano()%100000)
	serverURL := spTestURL(listName)

	// Start server1
	t.Log("Phase 1: starting server1...")
	serverProc1, serverStdout1 := startSPServer(t, serverURL)
	resolvedURL := waitForSharePointLink(t, serverStdout1, 120*time.Second)
	t.Logf("server1 resolved URL: %s", resolvedURL)
	time.Sleep(5 * time.Second)

	// Start client
	socksPort := freePort(t)
	socksAddr := fmt.Sprintf("127.0.0.1:%d", socksPort)
	alias := fmt.Sprintf("outage-%d", atomic.AddUint32(&testCounter, 1))
	clientProc := startSPClient(t, resolvedURL, socksPort, alias)
	t.Cleanup(func() { clientProc.Process.Kill(); clientProc.Wait() })

	waitForTCP(t, socksAddr, 180*time.Second)
	time.Sleep(10 * time.Second) // wait for both client and server login over slow SharePoint

	// Phase 1: baseline
	t.Log("Phase 1: baseline HTTP GET")
	httpViaSOCKS5WithRetry(t, socksAddr, target, "baseline", 180*time.Second)

	// Phase 2: kill server, wait 60s
	t.Log("Phase 2: killing server, waiting 60s...")
	serverProc1.Process.Kill()
	serverProc1.Wait()
	time.Sleep(60 * time.Second)

	// Phase 3: restart server with same resolved URL
	t.Log("Phase 3: restarting server...")
	serverProc2, serverStdout2 := startSPServer(t, resolvedURL)
	t.Cleanup(func() { serverProc2.Process.Kill(); serverProc2.Wait() })
	waitForSharePointLink(t, serverStdout2, 60*time.Second)

	// Phase 4: verify recovery
	t.Log("Phase 4: verifying HTTP recovery...")
	httpViaSOCKS5WithRetry(t, socksAddr, target, "after-outage", 180*time.Second)

	t.Log("PASS: network outage recovery — HTTP works after 60s server downtime")
}

// TestE2E_Boundary_IdleResume idles 90s then verifies HTTP still works.
func TestE2E_Boundary_IdleResume(t *testing.T) {
	skipIfNotSharePointReal(t)

	const target = "http://www.baidu.com"

	socksAddr := setupSharePointE2E(t)

	// Baseline
	t.Log("Phase 1: baseline HTTP GET")
	httpViaSOCKS5WithRetry(t, socksAddr, target, "baseline", 180*time.Second)

	// Idle 90s
	t.Log("Phase 2: idling for 90 seconds...")
	time.Sleep(90 * time.Second)

	// Verify
	t.Log("Phase 3: verifying HTTP after idle...")
	httpViaSOCKS5WithRetry(t, socksAddr, target, "after-idle", 60*time.Second)

	t.Log("PASS: idle resume — HTTP works after 90s idle")
}

// TestE2E_Boundary_BurstTraffic sends sequential HTTP GETs with interval.
// SharePoint uses 3s polling, so we space requests to avoid overwhelming
// the single simplex channel. Each request needs ~2 poll rounds (6-12s).
func TestE2E_Boundary_BurstTraffic(t *testing.T) {
	skipIfNotSharePointReal(t)

	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte("burst-ok"))
	}))
	defer ts.Close()

	socksAddr := setupSharePointE2E(t)

	// Warmup: verify tunnel is working before burst test
	t.Log("Phase 0: warmup HTTP GET")
	httpViaSOCKS5WithRetry(t, socksAddr, ts.URL, "warmup", 180*time.Second)

	const burstCount = 3
	t.Logf("Phase 1: sending %d sequential HTTP GETs", burstCount)
	success := 0
	for i := 0; i < burstCount; i++ {
		client := newSOCKS5Client(t, socksAddr, 90*time.Second)
		resp, err := client.Get(ts.URL)
		if err != nil {
			t.Logf("  burst #%d: error: %v", i, err)
			client.CloseIdleConnections()
			continue
		}
		body, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		client.CloseIdleConnections()
		if string(body) == "burst-ok" {
			success++
			t.Logf("  burst #%d: OK", i)
		} else {
			t.Logf("  burst #%d: unexpected body: %q", i, body)
		}
	}

	t.Logf("Burst results: %d/%d success", success, burstCount)
	if success < 2 {
		t.Fatalf("Expected at least 2/%d success, got %d", burstCount, success)
	}

	t.Log("PASS: burst traffic — sequential HTTP GETs succeed")
}

// TestE2E_Boundary_LargePayload sends a 100KB body and verifies SHA256.
// SharePoint can transfer ~65KB per poll cycle (5s), so 100KB needs 2+ cycles.
// We use a separate warmup endpoint with small response to avoid timeout.
func TestE2E_Boundary_LargePayload(t *testing.T) {
	skipIfNotSharePointReal(t)

	payloadSize := 100 * 1024
	payload := make([]byte, payloadSize)
	for i := range payload {
		payload[i] = byte(i % 256)
	}
	expectedHash := fmt.Sprintf("%x", sha256.Sum256(payload))

	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/warmup" {
			w.Write([]byte("ok"))
			return
		}
		w.Write(payload)
	}))
	defer ts.Close()

	socksAddr := setupSharePointE2E(t)

	t.Log("Phase 0: warmup HTTP GET (small response)")
	httpViaSOCKS5WithRetry(t, socksAddr, ts.URL+"/warmup", "warmup", 180*time.Second)

	t.Logf("Phase 1: requesting %d byte payload", payloadSize)
	body := largePayloadViaSOCKS5(t, socksAddr, ts.URL+"/large", 300*time.Second)

	gotHash := fmt.Sprintf("%x", sha256.Sum256(body))
	t.Logf("Expected SHA256: %s", expectedHash)
	t.Logf("Got SHA256:      %s", gotHash)
	t.Logf("Body size: expected=%d, got=%d", payloadSize, len(body))

	if gotHash != expectedHash {
		t.Fatalf("SHA256 mismatch: payload corrupted")
	}

	t.Log("PASS: large payload — 100KB transferred with SHA256 integrity")
}

// TestE2E_Boundary_LargePayload_1MB sends a 1MB body and verifies SHA256.
// At ~65KB per SharePoint poll cycle (5s), 1MB needs ~16 cycles ≈ 80-160s.
func TestE2E_Boundary_LargePayload_1MB(t *testing.T) {
	skipIfNotSharePointReal(t)

	payloadSize := 1024 * 1024
	payload := make([]byte, payloadSize)
	for i := range payload {
		payload[i] = byte(i % 256)
	}
	expectedHash := fmt.Sprintf("%x", sha256.Sum256(payload))

	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/warmup" {
			w.Write([]byte("ok"))
			return
		}
		w.Write(payload)
	}))
	defer ts.Close()

	socksAddr := setupSharePointE2E(t)

	t.Log("Phase 0: warmup HTTP GET (small response)")
	httpViaSOCKS5WithRetry(t, socksAddr, ts.URL+"/warmup", "warmup", 180*time.Second)

	t.Logf("Phase 1: requesting %d byte (1MB) payload", payloadSize)
	body := largePayloadViaSOCKS5(t, socksAddr, ts.URL+"/large", 600*time.Second)

	gotHash := fmt.Sprintf("%x", sha256.Sum256(body))
	t.Logf("Expected SHA256: %s", expectedHash)
	t.Logf("Got SHA256:      %s", gotHash)
	t.Logf("Body size: expected=%d, got=%d", payloadSize, len(body))

	if gotHash != expectedHash {
		t.Fatalf("SHA256 mismatch: payload corrupted")
	}

	t.Log("PASS: large payload — 1MB transferred with SHA256 integrity")
}

// largePayloadViaSOCKS5 fetches a URL via SOCKS5 with a long per-request
// timeout suitable for large payloads over slow transports.
func largePayloadViaSOCKS5(t *testing.T, socksAddr, targetURL string, deadline time.Duration) []byte {
	t.Helper()
	limit := time.Now().Add(deadline)
	for {
		// 300s per-request timeout: large payloads need many poll cycles
		client := newSOCKS5Client(t, socksAddr, 300*time.Second)
		resp, err := client.Get(targetURL)
		if err == nil {
			body, err := io.ReadAll(resp.Body)
			resp.Body.Close()
			client.CloseIdleConnections()
			if err == nil && len(body) > 0 {
				t.Logf("  large payload received: %d bytes", len(body))
				return body
			}
			if err != nil {
				t.Logf("  ReadAll error: %v", err)
			}
		} else {
			t.Logf("  GET error: %v", err)
		}
		client.CloseIdleConnections()
		if time.Now().After(limit) {
			t.Fatalf("large payload: deadline exceeded after %v", deadline)
		}
		t.Logf("  retrying large payload request (%v remaining)...", time.Until(limit).Truncate(time.Second))
		time.Sleep(10 * time.Second)
	}
}

// TestE2E_Boundary_ConcurrentRequests sends 2 concurrent HTTP GETs.
// SharePoint simplex has limited concurrency due to 3s polling interval,
// so we keep concurrency low to match transport capability.
func TestE2E_Boundary_ConcurrentRequests(t *testing.T) {
	skipIfNotSharePointReal(t)

	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte("concurrent-ok"))
	}))
	defer ts.Close()

	socksAddr := setupSharePointE2E(t)

	// Warmup: verify tunnel is working before concurrent test
	t.Log("Phase 0: warmup HTTP GET")
	httpViaSOCKS5WithRetry(t, socksAddr, ts.URL, "warmup", 180*time.Second)

	const concurrency = 2
	t.Logf("Phase 1: %d concurrent HTTP GETs", concurrency)
	var wg sync.WaitGroup
	var successCount int32

	for i := 0; i < concurrency; i++ {
		wg.Add(1)
		go func(idx int) {
			defer wg.Done()
			client := newSOCKS5Client(t, socksAddr, 90*time.Second)
			defer client.CloseIdleConnections()

			resp, err := client.Get(ts.URL)
			if err != nil {
				t.Logf("  goroutine #%d: error: %v", idx, err)
				return
			}
			defer resp.Body.Close()

			body, _ := io.ReadAll(resp.Body)
			if string(body) == "concurrent-ok" {
				atomic.AddInt32(&successCount, 1)
				t.Logf("  goroutine #%d: OK", idx)
			} else {
				t.Logf("  goroutine #%d: unexpected body: %q", idx, body)
			}
		}(i)
	}

	wg.Wait()
	t.Logf("Concurrent results: %d/%d success", successCount, concurrency)
	if successCount < 1 {
		t.Fatalf("Expected at least 1/%d success, got %d", concurrency, successCount)
	}

	t.Log("PASS: concurrent requests — parallel HTTP GETs succeed")
}

// TestE2E_Boundary_ServerRestart kills the server, restarts it with the same
// list_id+data_col, and verifies HTTP recovery through the still-running client.
func TestE2E_Boundary_ServerRestart(t *testing.T) {
	skipIfNotSharePointReal(t)

	const target = "http://www.baidu.com"
	listName := fmt.Sprintf("rem-srvrestart-%d", time.Now().UnixNano()%100000)
	serverURL := spTestURL(listName)

	// Phase 0: start server1
	t.Log("Phase 0: starting server1...")
	serverProc1, serverStdout1 := startSPServer(t, serverURL)
	resolvedURL := waitForSharePointLink(t, serverStdout1, 120*time.Second)
	t.Logf("server1 resolved URL: %s", resolvedURL)
	time.Sleep(5 * time.Second)

	// Start client (stays alive throughout)
	socksPort := freePort(t)
	socksAddr := fmt.Sprintf("127.0.0.1:%d", socksPort)
	alias := fmt.Sprintf("srvrestart-%d", atomic.AddUint32(&testCounter, 1))
	clientProc := startSPClient(t, resolvedURL, socksPort, alias)
	t.Cleanup(func() { clientProc.Process.Kill(); clientProc.Wait() })

	waitForTCP(t, socksAddr, 180*time.Second)
	time.Sleep(10 * time.Second)

	// Phase 1: baseline
	t.Log("Phase 1: baseline HTTP GET")
	httpViaSOCKS5WithRetry(t, socksAddr, target, "baseline", 180*time.Second)
	t.Log("Phase 1: PASS")

	// Phase 2: kill server, restart with same resolved URL
	t.Log("Phase 2: killing server1, restarting server2...")
	serverProc1.Process.Kill()
	serverProc1.Wait()
	time.Sleep(2 * time.Second)

	serverProc2, serverStdout2 := startSPServer(t, resolvedURL)
	t.Cleanup(func() { serverProc2.Process.Kill(); serverProc2.Wait() })
	waitForSharePointLink(t, serverStdout2, 60*time.Second)

	// Phase 3: verify recovery
	t.Log("Phase 3: verifying HTTP works after server restart...")
	httpViaSOCKS5WithRetry(t, socksAddr, target, "after-server-restart", 180*time.Second)
	t.Log("Phase 3: PASS")

	t.Log("PASS: server restart — HTTP recovers after server-only restart")
}

// TestE2E_Boundary_ClientRestart kills the client, starts a new client on a
// fresh SOCKS5 port, and verifies HTTP works through the still-running server.
func TestE2E_Boundary_ClientRestart(t *testing.T) {
	skipIfNotSharePointReal(t)

	const target = "http://www.baidu.com"
	listName := fmt.Sprintf("rem-clirestart-%d", time.Now().UnixNano()%100000)
	serverURL := spTestURL(listName)

	// Phase 0: start server (stays alive throughout)
	t.Log("Phase 0: starting server...")
	serverProc, serverStdout := startSPServer(t, serverURL)
	t.Cleanup(func() { serverProc.Process.Kill(); serverProc.Wait() })
	resolvedURL := waitForSharePointLink(t, serverStdout, 120*time.Second)
	t.Logf("server resolved URL: %s", resolvedURL)
	time.Sleep(5 * time.Second)

	// Start client1
	socksPort1 := freePort(t)
	socksAddr1 := fmt.Sprintf("127.0.0.1:%d", socksPort1)
	alias1 := fmt.Sprintf("clirestart-%d-a", atomic.AddUint32(&testCounter, 1))
	clientProc1 := startSPClient(t, resolvedURL, socksPort1, alias1)

	waitForTCP(t, socksAddr1, 180*time.Second)
	time.Sleep(10 * time.Second)

	// Phase 1: baseline
	t.Log("Phase 1: baseline HTTP GET via client1")
	httpViaSOCKS5WithRetry(t, socksAddr1, target, "baseline", 180*time.Second)
	t.Log("Phase 1: PASS")

	// Phase 2: kill client1, start client2 on new SOCKS5 port
	t.Log("Phase 2: killing client1, starting client2...")
	clientProc1.Process.Kill()
	clientProc1.Wait()
	time.Sleep(2 * time.Second)

	socksPort2 := freePort(t)
	socksAddr2 := fmt.Sprintf("127.0.0.1:%d", socksPort2)
	alias2 := fmt.Sprintf("clirestart-%d-b", atomic.AddUint32(&testCounter, 1))
	clientProc2 := startSPClient(t, resolvedURL, socksPort2, alias2)
	t.Cleanup(func() { clientProc2.Process.Kill(); clientProc2.Wait() })

	waitForTCP(t, socksAddr2, 180*time.Second)
	time.Sleep(10 * time.Second)

	// Phase 3: verify recovery
	t.Log("Phase 3: verifying HTTP works via client2...")
	httpViaSOCKS5WithRetry(t, socksAddr2, target, "after-client-restart", 180*time.Second)
	t.Log("Phase 3: PASS")

	t.Log("PASS: client restart — HTTP recovers after client-only restart")
}

// TestE2E_Boundary_NetworkOutage30s kills the server for 30 seconds, restarts
// it, and verifies HTTP recovery through the still-running client.
func TestE2E_Boundary_NetworkOutage30s(t *testing.T) {
	skipIfNotSharePointReal(t)

	const target = "http://www.baidu.com"
	listName := fmt.Sprintf("rem-outage30-%d", time.Now().UnixNano()%100000)
	serverURL := spTestURL(listName)

	// Phase 0: start server1
	t.Log("Phase 0: starting server1...")
	serverProc1, serverStdout1 := startSPServer(t, serverURL)
	resolvedURL := waitForSharePointLink(t, serverStdout1, 120*time.Second)
	t.Logf("server1 resolved URL: %s", resolvedURL)
	time.Sleep(5 * time.Second)

	// Start client (stays alive throughout)
	socksPort := freePort(t)
	socksAddr := fmt.Sprintf("127.0.0.1:%d", socksPort)
	alias := fmt.Sprintf("outage30-%d", atomic.AddUint32(&testCounter, 1))
	clientProc := startSPClient(t, resolvedURL, socksPort, alias)
	t.Cleanup(func() { clientProc.Process.Kill(); clientProc.Wait() })

	waitForTCP(t, socksAddr, 180*time.Second)
	time.Sleep(10 * time.Second)

	// Phase 1: baseline
	t.Log("Phase 1: baseline HTTP GET")
	httpViaSOCKS5WithRetry(t, socksAddr, target, "baseline", 180*time.Second)
	t.Log("Phase 1: PASS")

	// Phase 2: kill server, wait 30s
	t.Log("Phase 2: killing server, waiting 30s...")
	serverProc1.Process.Kill()
	serverProc1.Wait()
	time.Sleep(30 * time.Second)

	// Phase 3: restart server
	t.Log("Phase 3: restarting server...")
	serverProc2, serverStdout2 := startSPServer(t, resolvedURL)
	t.Cleanup(func() { serverProc2.Process.Kill(); serverProc2.Wait() })
	waitForSharePointLink(t, serverStdout2, 60*time.Second)

	// Phase 4: verify recovery
	t.Log("Phase 4: verifying HTTP recovery after 30s outage...")
	httpViaSOCKS5WithRetry(t, socksAddr, target, "after-30s-outage", 180*time.Second)
	t.Log("Phase 4: PASS")

	t.Log("PASS: 30s network outage — HTTP recovers after 30s server downtime")
}
