//go:build graph && !tinygo

package runner

import (
	"bufio"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"regexp"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	_ "github.com/chainreactors/rem/protocol/tunnel/simplex"
)

func spGetEnv(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}

func spTestURL(listName string) string {
	tenant := spGetEnv("SHAREPOINT_TENANT_ID", "4194e4a5-cad1-4e7b-aeed-6928128121cc")
	client := spGetEnv("SHAREPOINT_CLIENT_ID", "ee056d17-bd75-4864-93d6-e502dfa61136")
	secret := spGetEnv("SHAREPOINT_CLIENT_SECRET", "REPLACE_WITH_YOUR_SECRET")
	site := spGetEnv("SHAREPOINT_SITE_ID", "cloudnz395.sharepoint.com,6a5a7eb0-e088-40bc-8c17-c5993303883a,d27ad5c4-403c-4731-961a-6f56cd07c98e")
	return fmt.Sprintf("simplex+sharepoint:///%s?tenant=%s&client=%s&secret=%s&site=%s&interval=3000&wrapper=raw",
		listName, tenant, client, secret, site)
}

var ansiStripRe = regexp.MustCompile(`\x1b\[[0-9;]*m`)

// waitForSharePointLink reads server subprocess output until it finds the
// resolved URL containing list_id and data_col.
func waitForSharePointLink(t *testing.T, r io.Reader, timeout time.Duration) string {
	t.Helper()
	result := make(chan string, 1)

	go func() {
		scanner := bufio.NewScanner(r)
		scanner.Buffer(make([]byte, 0, 64*1024), 256*1024) // large buffer for long URLs
		found := false
		for scanner.Scan() {
			line := scanner.Text()
			fmt.Fprintln(os.Stderr, line) // mirror to test output
			if !found {
				clean := ansiStripRe.ReplaceAllString(line, "")
				if idx := strings.Index(clean, "simplex+sharepoint://"); idx >= 0 {
					remainder := clean[idx:]
					// URL ends at first space (strips trailing log timestamp)
					if spaceIdx := strings.IndexByte(remainder, ' '); spaceIdx > 0 {
						remainder = remainder[:spaceIdx]
					}
					remainder = strings.TrimSpace(remainder)
					// Only accept the resolved URL that contains list_id
					// (skip the early console log that doesn't have it yet)
					if strings.Contains(remainder, "list_id=") {
						result <- remainder
						found = true
					}
				}
			}
		}
	}()

	select {
	case u := <-result:
		return u
	case <-time.After(timeout):
		t.Fatal("timeout waiting for SharePoint server to output resolved URL")
		return ""
	}
}

// setupSharePointE2E launches a SharePoint-backed REM server and client
// as separate subprocesses, waits for the SOCKS5 listener, and returns
// its address.
//
// Flow:
//  1. Server subprocess starts, creates a SharePoint list, logs the resolved URL
//  2. Parent parses the resolved URL (contains list_id + data_col)
//  3. Client subprocess connects using the resolved URL in proxy mode
//  4. Parent waits for the SOCKS5 listener to accept connections
func setupSharePointE2E(t *testing.T) string {
	t.Helper()
	alias := fmt.Sprintf("spe2e%d", atomic.AddUint32(&testCounter, 1))
	listName := fmt.Sprintf("rem-e2e-%d", time.Now().UnixNano()%100000)

	serverURL := spTestURL(listName)

	// --- server subprocess ---
	serverProc := exec.Command(os.Args[0], "-test.run=^TestHelperProcess$", "-test.v")
	serverProc.Env = append(os.Environ(),
		"REM_HELPER=1",
		fmt.Sprintf("REM_HELPER_CMD=--debug -s %s -i 127.0.0.1 --no-sub", serverURL),
	)

	// Capture stdout (where chainreactors/logs writes) to extract the resolved URL.
	// Also capture stderr for complete log visibility.
	stdoutPipe, err := serverProc.StdoutPipe()
	if err != nil {
		t.Fatalf("stdout pipe: %v", err)
	}
	serverProc.Stderr = os.Stderr

	if err := serverProc.Start(); err != nil {
		t.Fatalf("start server subprocess: %v", err)
	}
	t.Cleanup(func() {
		serverProc.Process.Kill()
		serverProc.Wait()
	})

	// Parse resolved URL from server log output (contains list_id + data_col)
	resolvedURL := waitForSharePointLink(t, stdoutPipe, 120*time.Second)
	t.Logf("Server resolved URL: %s", resolvedURL)

	// Give the server accept loop time to initialize
	time.Sleep(5 * time.Second)

	// Allocate the SOCKS5 port as late as possible (just before client start)
	// to avoid Windows port exclusion/TIME_WAIT issues during the long server startup.
	socksPort := freePort(t)

	// --- client subprocess ---
	clientProc := exec.Command(os.Args[0], "-test.run=^TestHelperProcess$", "-test.v")
	clientProc.Env = append(os.Environ(),
		"REM_HELPER=1",
		fmt.Sprintf("REM_HELPER_CMD=-c %s -m proxy -l socks5://127.0.0.1:%d -a %s --debug",
			resolvedURL, socksPort, alias),
	)
	clientProc.Stdout = os.Stdout
	clientProc.Stderr = os.Stderr

	if err := clientProc.Start(); err != nil {
		t.Fatalf("start client subprocess: %v", err)
	}
	t.Cleanup(func() {
		clientProc.Process.Kill()
		clientProc.Wait()
	})

	socksAddr := fmt.Sprintf("127.0.0.1:%d", socksPort)
	// SharePoint is slow: generous timeout for the full connection chain
	// (token acquisition → list creation → ARQ handshake → agent login → SOCKS5 bind)
	waitForTCP(t, socksAddr, 180*time.Second)
	// SharePoint is much slower than TCP: the server-side login may still be
	// in-flight when the client's SOCKS5 port opens. Wait long enough for the
	// full bidirectional handshake (login, relay setup) to complete over the
	// 3-second polling interval before callers start sending HTTP requests.
	time.Sleep(10 * time.Second)
	return socksAddr
}

// TestE2E_SharePoint_SOCKS5Proxy verifies that HTTP requests work through
// a SOCKS5 proxy backed by a SharePoint simplex tunnel.
func TestE2E_SharePoint_SOCKS5Proxy(t *testing.T) {
	if os.Getenv("SHAREPOINT_REAL") != "1" {
		t.Skip("Skipping SharePoint integration test (set SHAREPOINT_REAL=1 to run)")
	}

	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte("hello sharepoint rem"))
	}))
	defer ts.Close()

	socksAddr := setupSharePointE2E(t)

	// SharePoint transport is slow (3s polling per leg). Bridge 1 always
	// fails (normal yamux behaviour), and bridge 2 needs time to complete the
	// SOCKS5 relay handshake through the tunnel. Use retry to tolerate this.
	httpViaSOCKS5WithRetry(t, socksAddr, ts.URL, "sharepoint-socks5", 180*time.Second)
	t.Log("SharePoint SOCKS5 proxy test PASSED")
}

// startSPServer starts a rem server subprocess using SharePoint transport.
// Returns the process and a pipe from which the resolved URL can be read.
func startSPServer(t *testing.T, serverURL string) (*exec.Cmd, io.Reader) {
	t.Helper()
	proc := exec.Command(os.Args[0], "-test.run=^TestHelperProcess$", "-test.v")
	proc.Env = append(os.Environ(),
		"REM_HELPER=1",
		fmt.Sprintf("REM_HELPER_CMD=--debug -s %s -i 127.0.0.1 --no-sub", serverURL),
	)
	stdoutPipe, err := proc.StdoutPipe()
	if err != nil {
		t.Fatalf("stdout pipe: %v", err)
	}
	proc.Stderr = os.Stderr
	if err := proc.Start(); err != nil {
		t.Fatalf("start server subprocess: %v", err)
	}
	return proc, stdoutPipe
}

// startSPClient starts a rem client subprocess connecting via SharePoint transport
// with a SOCKS5 listener on the given port. Returns the process.
func startSPClient(t *testing.T, clientURL string, socksPort int, alias string) *exec.Cmd {
	t.Helper()
	proc := exec.Command(os.Args[0], "-test.run=^TestHelperProcess$", "-test.v")
	proc.Env = append(os.Environ(),
		"REM_HELPER=1",
		fmt.Sprintf("REM_HELPER_CMD=-c %s -m proxy -l socks5://127.0.0.1:%d -a %s --debug",
			clientURL, socksPort, alias),
	)
	proc.Stdout = os.Stdout
	proc.Stderr = os.Stderr
	if err := proc.Start(); err != nil {
		t.Fatalf("start client subprocess: %v", err)
	}
	return proc
}

// httpViaSOCKS5 makes an HTTP GET to targetURL through a SOCKS5 proxy and
// returns the response body. It fails the test on error or non-2xx/3xx status.
func httpViaSOCKS5(t *testing.T, socksAddr, targetURL, label string) {
	t.Helper()
	client := newSOCKS5Client(t, socksAddr, 60*time.Second)
	resp, err := client.Get(targetURL)
	if err != nil {
		t.Fatalf("[%s] HTTP GET %s via SOCKS5 %s: %v", label, targetURL, socksAddr, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 400 {
		t.Fatalf("[%s] HTTP %d from %s", label, resp.StatusCode, targetURL)
	}
	t.Logf("[%s] HTTP %d from %s — OK", label, resp.StatusCode, targetURL)
	client.CloseIdleConnections()
}

// httpViaSOCKS5WithRetry retries HTTP GET through SOCKS5 until success or deadline.
// This is needed after a server/client restart where the session re-establishment
// over SharePoint (3s poll interval + handshake rounds) takes unpredictable time.
func httpViaSOCKS5WithRetry(t *testing.T, socksAddr, targetURL, label string, deadline time.Duration) {
	t.Helper()
	limit := time.Now().Add(deadline)
	for {
		client := newSOCKS5Client(t, socksAddr, 30*time.Second)
		resp, err := client.Get(targetURL)
		client.CloseIdleConnections()
		if err == nil && resp.StatusCode < 400 {
			resp.Body.Close()
			t.Logf("[%s] HTTP %d from %s — OK", label, resp.StatusCode, targetURL)
			return
		}
		if resp != nil {
			resp.Body.Close()
		}
		if time.Now().After(limit) {
			if err != nil {
				t.Fatalf("[%s] HTTP GET %s via SOCKS5 %s: %v (deadline exceeded)", label, targetURL, socksAddr, err)
			}
			t.Fatalf("[%s] HTTP %d from %s (deadline exceeded)", label, resp.StatusCode, targetURL)
		}
		t.Logf("[%s] retrying (%v remaining)...", label, time.Until(limit).Truncate(time.Second))
		time.Sleep(5 * time.Second)
	}
}

// TestE2E_SharePoint_RestartResiliency verifies that the full rem stack
// (server + client over SharePoint) recovers after either side is restarted.
//
// Scenario:
//  1. Start server1 + client1. Verify curl to baidu.com works (baseline).
//  2. Kill server1. Start server2 with the same listID + dataCol.
//     Client auto-reconnects (retry loop). Verify curl still works.
//  3. Kill client1. Start client2 on a new SOCKS5 port.
//     Verify curl on new port works.
//
// Usage:
//
//	SHAREPOINT_REAL=1 go test -v -tags graph -run TestE2E_SharePoint_RestartResiliency \
//	  ./runner/ -count=1 -timeout=600s
func TestE2E_SharePoint_RestartResiliency(t *testing.T) {
	if os.Getenv("SHAREPOINT_REAL") != "1" {
		t.Skip("Skipping SharePoint restart resiliency test (set SHAREPOINT_REAL=1 to run)")
	}

	const target = "http://www.baidu.com"
	listName := fmt.Sprintf("rem-resilience-%d", time.Now().UnixNano()%100000)

	// ── Phase 0: Start server1 ──────────────────────────────────────────────
	t.Log("Phase 0: starting server1...")
	serverProc1, serverStdout1 := startSPServer(t, spTestURL(listName))

	resolvedURL := waitForSharePointLink(t, serverStdout1, 120*time.Second)
	t.Logf("server1 resolved URL: %s", resolvedURL)
	time.Sleep(5 * time.Second) // let accept loop initialize

	// ── Phase 0: Start client1 on socksPort1 ────────────────────────────────
	socksPort1 := freePort(t)
	socksAddr1 := fmt.Sprintf("127.0.0.1:%d", socksPort1)
	alias1 := fmt.Sprintf("resilience-%d-a", atomic.AddUint32(&testCounter, 1))
	clientProc1 := startSPClient(t, resolvedURL, socksPort1, alias1)

	waitForTCP(t, socksAddr1, 180*time.Second)
	time.Sleep(3 * time.Second)

	// ── Phase 1: Baseline ────────────────────────────────────────────────────
	t.Log("Phase 1: baseline — curl to baidu.com via SOCKS5")
	httpViaSOCKS5WithRetry(t, socksAddr1, target, "baseline", 180*time.Second)
	t.Log("Phase 1: PASS")

	// ── Phase 2: Server restart ──────────────────────────────────────────────
	t.Log("Phase 2: restarting server...")
	serverProc1.Process.Kill()
	serverProc1.Wait()
	time.Sleep(2 * time.Second)

	// server2 uses the resolved URL (contains list_id + data_col) — resumes via getLastItemID + sends RST
	serverProc2, serverStdout2 := startSPServer(t, resolvedURL)
	t.Cleanup(func() { serverProc2.Process.Kill(); serverProc2.Wait() })

	// Wait for server2 to initialize and log its link
	waitForSharePointLink(t, serverStdout2, 60*time.Second)
	t.Log("Phase 2: server2 started, waiting for client1 to auto-reconnect...")

	// The SOCKS5 port remains bound (client process is still alive), so waitForTCP
	// returns immediately. We retry HTTP until the session re-establishes over SharePoint.
	httpViaSOCKS5WithRetry(t, socksAddr1, target, "after server restart", 180*time.Second)
	t.Log("Phase 2: PASS — curl works after server restart + client auto-reconnect")

	// ── Phase 3: Client restart ──────────────────────────────────────────────
	t.Log("Phase 3: restarting client...")
	clientProc1.Process.Kill()
	clientProc1.Wait()
	time.Sleep(2 * time.Second)

	// client2 connects to the still-running server2, new SOCKS5 port
	socksPort2 := freePort(t)
	socksAddr2 := fmt.Sprintf("127.0.0.1:%d", socksPort2)
	alias2 := fmt.Sprintf("resilience-%d-b", atomic.AddUint32(&testCounter, 1))
	clientProc2 := startSPClient(t, resolvedURL, socksPort2, alias2)
	t.Cleanup(func() { clientProc2.Process.Kill(); clientProc2.Wait() })

	waitForTCP(t, socksAddr2, 180*time.Second)
	time.Sleep(3 * time.Second)

	httpViaSOCKS5WithRetry(t, socksAddr2, target, "after client restart", 180*time.Second)
	t.Log("Phase 3: PASS — curl works after client restart")

	t.Log("TestE2E_SharePoint_RestartResiliency: ALL PHASES PASSED")
}

// TestE2E_SharePoint_ClientRestart_RapidCycle verifies that a SharePoint client
// can be killed and restarted multiple times in rapid succession, and the server
// correctly discovers each new client identity (new random list name) and
// re-establishes communication.
//
// Edge case: On each restart, the client process loses its in-memory
// clientListRegistry, so it generates a new random list name (client-XXXXXXXX).
// The server discovers the new client via the UID field in list items, then
// calls findListByName to locate the new client's response list.
// This test verifies that:
//   1. Rapid restarts don't confuse the server (no stale UID mapping)
//   2. Each new client identity is properly discovered
//   3. The server resumes via getLastItemID + sends RST on restart
//
// Usage:
//
//	SHAREPOINT_REAL=1 go test -v -tags graph -run TestE2E_SharePoint_ClientRestart_RapidCycle \
//	  ./runner/ -count=1 -timeout=600s
func TestE2E_SharePoint_ClientRestart_RapidCycle(t *testing.T) {
	if os.Getenv("SHAREPOINT_REAL") != "1" {
		t.Skip("Skipping SharePoint rapid client restart test (set SHAREPOINT_REAL=1 to run)")
	}

	const target = "http://www.baidu.com"
	listName := fmt.Sprintf("rem-rapid-%d", time.Now().UnixNano()%100000)

	// Start server (stays alive for entire test)
	t.Log("Phase 0: starting server...")
	serverProc, serverStdout := startSPServer(t, spTestURL(listName))
	t.Cleanup(func() { serverProc.Process.Kill(); serverProc.Wait() })

	resolvedURL := waitForSharePointLink(t, serverStdout, 120*time.Second)
	t.Logf("server resolved URL: %s", resolvedURL)
	time.Sleep(5 * time.Second)

	// Phase 1: client1 — baseline
	socksPort1 := freePort(t)
	socksAddr1 := fmt.Sprintf("127.0.0.1:%d", socksPort1)
	alias1 := fmt.Sprintf("rapid-%d-a", atomic.AddUint32(&testCounter, 1))
	clientProc1 := startSPClient(t, resolvedURL, socksPort1, alias1)

	waitForTCP(t, socksAddr1, 180*time.Second)
	time.Sleep(3 * time.Second)

	t.Log("Phase 1: baseline HTTP via client1")
	httpViaSOCKS5WithRetry(t, socksAddr1, target, "rapid-baseline", 180*time.Second)
	t.Log("Phase 1: PASS")

	// Phase 2: kill client1, start client2 immediately (no sleep)
	// Client2 creates a new random list name because clientListRegistry is gone.
	// Server must discover the new UID and findListByName for the new list.
	t.Log("Phase 2: killing client1, starting client2 immediately...")
	clientProc1.Process.Kill()
	clientProc1.Wait()
	// Minimal wait — test rapid restart
	time.Sleep(1 * time.Second)

	socksPort2 := freePort(t)
	socksAddr2 := fmt.Sprintf("127.0.0.1:%d", socksPort2)
	alias2 := fmt.Sprintf("rapid-%d-b", atomic.AddUint32(&testCounter, 1))
	clientProc2 := startSPClient(t, resolvedURL, socksPort2, alias2)

	waitForTCP(t, socksAddr2, 180*time.Second)
	time.Sleep(3 * time.Second)

	t.Log("Phase 2: HTTP via client2 (new identity)")
	httpViaSOCKS5WithRetry(t, socksAddr2, target, "rapid-restart-1", 180*time.Second)
	t.Log("Phase 2: PASS")

	// Phase 3: kill client2, start client3 (third identity)
	// Verifies the server can accumulate multiple client identities and still
	// route data to the correct (latest) client.
	t.Log("Phase 3: killing client2, starting client3...")
	clientProc2.Process.Kill()
	clientProc2.Wait()
	time.Sleep(1 * time.Second)

	socksPort3 := freePort(t)
	socksAddr3 := fmt.Sprintf("127.0.0.1:%d", socksPort3)
	alias3 := fmt.Sprintf("rapid-%d-c", atomic.AddUint32(&testCounter, 1))
	clientProc3 := startSPClient(t, resolvedURL, socksPort3, alias3)
	t.Cleanup(func() { clientProc3.Process.Kill(); clientProc3.Wait() })

	waitForTCP(t, socksAddr3, 180*time.Second)
	time.Sleep(3 * time.Second)

	t.Log("Phase 3: HTTP via client3 (third identity)")
	httpViaSOCKS5WithRetry(t, socksAddr3, target, "rapid-restart-2", 180*time.Second)
	t.Log("Phase 3: PASS")

	t.Log("PASS: SharePoint rapid client restart — server discovers each new identity")
}

// TestE2E_SharePoint_Stability runs a real SharePoint simplex tunnel stability
// test, sending HTTP GET to baidu.com every 10 seconds through SOCKS5.
//
// Usage:
//
//	SHAREPOINT_REAL=1 go test -v -tags "graph" -run TestE2E_SharePoint_Stability \
//	  ./runner/ -count=1 -timeout=15m
func TestE2E_SharePoint_Stability(t *testing.T) {
	if os.Getenv("SHAREPOINT_REAL") != "1" {
		t.Skip("Skipping SharePoint stability test (set SHAREPOINT_REAL=1 to run)")
	}

	const (
		testDuration  = 10 * time.Minute
		probeInterval = 10 * time.Second
		targetURL     = "http://www.baidu.com"
	)

	socksAddr := setupSharePointE2E(t)
	t.Logf("SOCKS5 ready at %s, starting %v stability test against %s", socksAddr, testDuration, targetURL)

	startTime := time.Now()
	deadline := startTime.Add(testDuration)
	total, success, fail := 0, 0, 0
	reconnects := 0
	var lastErr error
	consecutiveFails := 0
	maxConsecutiveFails := 0

	for time.Now().Before(deadline) {
		total++
		elapsed := time.Since(startTime).Truncate(time.Second)

		// If we just had a failure, check if SOCKS5 port is reachable first.
		// If not, wait for the client to reconnect and re-establish the listener.
		if consecutiveFails > 0 {
			conn, dialErr := net.DialTimeout("tcp", socksAddr, 2*time.Second)
			if dialErr != nil {
				// Port is down — client is reconnecting. Wait for it.
				t.Logf("[%v] probe #%d: SOCKS5 port down, waiting for reconnect...", elapsed, total)
				reconnectDeadline := time.Now().Add(180 * time.Second)
				recovered := false
				for time.Now().Before(reconnectDeadline) {
					time.Sleep(5 * time.Second)
					conn2, err2 := net.DialTimeout("tcp", socksAddr, 2*time.Second)
					if err2 == nil {
						conn2.Close()
						recovered = true
						break
					}
				}
				if !recovered {
					t.Fatalf("SOCKS5 port did not recover within 180s after reconnect. Last error: %v", lastErr)
				}
				reconnects++
				t.Logf("[%v] probe #%d: SOCKS5 port recovered (reconnect #%d), resuming probes",
					elapsed, total, reconnects)
				time.Sleep(5 * time.Second)
				continue
			}
			conn.Close()
		}

		httpClient := newSOCKS5Client(t, socksAddr, 90*time.Second)
		resp, err := httpClient.Get(targetURL)
		if err != nil {
			fail++
			consecutiveFails++
			if consecutiveFails > maxConsecutiveFails {
				maxConsecutiveFails = consecutiveFails
			}
			lastErr = err
			t.Logf("[%v] probe #%d FAIL (%d consecutive): %v", elapsed, total, consecutiveFails, err)
		} else {
			body, _ := io.ReadAll(resp.Body)
			resp.Body.Close()

			if resp.StatusCode == 200 && len(body) > 0 {
				success++
				if consecutiveFails > 0 {
					t.Logf("[%v] probe #%d RECOVERED after %d failures", elapsed, total, consecutiveFails)
				}
				consecutiveFails = 0
			} else {
				fail++
				consecutiveFails++
				if consecutiveFails > maxConsecutiveFails {
					maxConsecutiveFails = consecutiveFails
				}
				t.Logf("[%v] probe #%d FAIL: status=%d bodyLen=%d", elapsed, total, resp.StatusCode, len(body))
			}
		}
		httpClient.CloseIdleConnections()

		// Log periodic summary every 10 probes
		if total%10 == 0 {
			rate := float64(success) / float64(total) * 100
			t.Logf("[%v] === %d probes: %d ok, %d fail, %d reconnects (%.1f%% success) ===",
				elapsed, total, success, fail, reconnects, rate)
		}

		if consecutiveFails >= 5 {
			t.Fatalf("5 consecutive failures at probe %d — tunnel likely dead. Last error: %v", total, lastErr)
		}

		time.Sleep(probeInterval)
	}

	// Final summary
	successRate := float64(success) / float64(total) * 100
	t.Logf("========== STABILITY TEST COMPLETE ==========")
	t.Logf("Duration:           %v", time.Since(startTime).Truncate(time.Second))
	t.Logf("Total probes:       %d", total)
	t.Logf("Success:            %d (%.1f%%)", success, successRate)
	t.Logf("Fail:               %d", fail)
	t.Logf("Reconnects:         %d", reconnects)
	t.Logf("Max consec. fails:  %d", maxConsecutiveFails)
	if lastErr != nil {
		t.Logf("Last error:         %v", lastErr)
	}

	if successRate < 90.0 {
		t.Fatalf("Success rate %.1f%% is below 90%% threshold", successRate)
	}
}

// TestE2E_SharePoint_ConsecutiveRestarts kills and restarts the server 5 times,
// verifying HTTP recovery after each restart.  SharePoint has RST notification,
// so recovery should be fast (~30-60s per cycle).
func TestE2E_SharePoint_ConsecutiveRestarts(t *testing.T) {
	if os.Getenv("SHAREPOINT_REAL") != "1" {
		t.Skip("Skipping SharePoint consecutive restart test (set SHAREPOINT_REAL=1 to run)")
	}

	const (
		restartCount     = 5
		perRestartWindow = 300 * time.Second
	)

	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte("sp-restart-ok"))
	}))
	defer ts.Close()

	listName := fmt.Sprintf("rem-consrestart-%d", time.Now().UnixNano()%100000)
	serverURL := spTestURL(listName)

	// Phase 0: initial setup
	t.Log("Phase 0: starting server1 + client")
	serverProc, serverStdout := startSPServer(t, serverURL)

	resolvedURL := waitForSharePointLink(t, serverStdout, 120*time.Second)
	t.Logf("Server resolved URL: %s", resolvedURL)
	time.Sleep(5 * time.Second)

	socksPort := freePort(t)
	socksAddr := fmt.Sprintf("127.0.0.1:%d", socksPort)
	alias := fmt.Sprintf("spcons-%d", atomic.AddUint32(&testCounter, 1))
	clientProc := startSPClient(t, resolvedURL, socksPort, alias)
	t.Cleanup(func() { clientProc.Process.Kill(); clientProc.Wait() })

	waitForTCP(t, socksAddr, 180*time.Second)
	time.Sleep(10 * time.Second)

	t.Log("Phase 0: baseline HTTP GET")
	httpViaSOCKS5WithRetry(t, socksAddr, ts.URL, "baseline", 180*time.Second)
	t.Log("Phase 0: PASS")

	for i := 1; i <= restartCount; i++ {
		t.Logf("===== Restart %d/%d =====", i, restartCount)

		t.Logf("  Killing server%d...", i)
		serverProc.Process.Kill()
		serverProc.Wait()
		time.Sleep(2 * time.Second)

		t.Logf("  Starting server%d...", i+1)
		serverProc, serverStdout = startSPServer(t, resolvedURL)

		waitForSharePointLink(t, serverStdout, 60*time.Second)
		t.Logf("  Server%d up, waiting for recovery...", i+1)

		deadline := time.Now().Add(perRestartWindow)
		recovered := false
		for time.Now().Before(deadline) {
			client := newSOCKS5Client(t, socksAddr, 30*time.Second)
			resp, err := client.Get(ts.URL)
			client.CloseIdleConnections()
			if err == nil {
				body, _ := io.ReadAll(resp.Body)
				resp.Body.Close()
				if string(body) == "sp-restart-ok" {
					recovered = true
					t.Logf("  Restart %d: HTTP OK after %v", i, time.Since(deadline.Add(-perRestartWindow)).Truncate(time.Second))
					break
				}
			}
			time.Sleep(5 * time.Second)
		}

		if !recovered {
			t.Fatalf("  Restart %d: HTTP never recovered within %v", i, perRestartWindow)
		}
	}

	serverProc.Process.Kill()
	serverProc.Wait()

	t.Logf("PASS: SharePoint %d consecutive server restarts — all recovered", restartCount)
}
