//go:build graph && !tinygo

package runner

import (
	"bufio"
	"crypto/sha256"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	_ "github.com/chainreactors/rem/protocol/tunnel/simplex"
)

// ============================================================
// OneDrive Integration Tests — Full-stack E2E via SOCKS5
//
// These tests use the real OneDrive Graph API (same Azure AD credentials
// as SharePoint tests) and exercise the full stack:
//   agent → ARQ → simplex → OneDrive → Graph API
//
// Usage:
//   SHAREPOINT_REAL=1 go test -v -tags graph -run "TestE2E_OneDrive_" \
//     ./runner/ -count=1 -timeout=600s
// ============================================================

func odTestURL(dirName string) string {
	tenant := spGetEnv("SHAREPOINT_TENANT_ID", "4194e4a5-cad1-4e7b-aeed-6928128121cc")
	client := spGetEnv("SHAREPOINT_CLIENT_ID", "ee056d17-bd75-4864-93d6-e502dfa61136")
	secret := spGetEnv("SHAREPOINT_CLIENT_SECRET", "REPLACE_WITH_YOUR_SECRET")
	site := spGetEnv("SHAREPOINT_SITE_ID", "cloudnz395.sharepoint.com,6a5a7eb0-e088-40bc-8c17-c5993303883a,d27ad5c4-403c-4731-961a-6f56cd07c98e")
	// OneDrive uses "prefix" query parameter (not URL path) to specify the directory
	return fmt.Sprintf("simplex+onedrive:///?tenant=%s&client=%s&secret=%s&site=%s&prefix=%s&interval=2000&wrapper=raw",
		tenant, client, secret, site, dirName)
}

// odTestURLWithInterval builds an OneDrive URL with a custom polling interval.
func odTestURLWithInterval(dirName string, intervalMs int) string {
	tenant := spGetEnv("SHAREPOINT_TENANT_ID", "4194e4a5-cad1-4e7b-aeed-6928128121cc")
	client := spGetEnv("SHAREPOINT_CLIENT_ID", "ee056d17-bd75-4864-93d6-e502dfa61136")
	secret := spGetEnv("SHAREPOINT_CLIENT_SECRET", "REPLACE_WITH_YOUR_SECRET")
	site := spGetEnv("SHAREPOINT_SITE_ID", "cloudnz395.sharepoint.com,6a5a7eb0-e088-40bc-8c17-c5993303883a,d27ad5c4-403c-4731-961a-6f56cd07c98e")
	return fmt.Sprintf("simplex+onedrive:///?tenant=%s&client=%s&secret=%s&site=%s&prefix=%s&interval=%d&wrapper=raw",
		tenant, client, secret, site, dirName, intervalMs)
}

// startODServer starts a rem server subprocess using OneDrive transport.
func startODServer(t *testing.T, serverURL string) (*exec.Cmd, io.Reader) {
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
		t.Fatalf("start OD server subprocess: %v", err)
	}
	return proc, stdoutPipe
}

// startODClient starts a rem client subprocess connecting via OneDrive transport.
func startODClient(t *testing.T, clientURL string, socksPort int, alias string) *exec.Cmd {
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
		t.Fatalf("start OD client subprocess: %v", err)
	}
	return proc
}

// waitForOneDriveLink reads server subprocess output until it finds the
// resolved OneDrive URL. Unlike SharePoint, OneDrive URLs don't contain list_id,
// so we look for the second URL logged (the resolved one after channel start).
func waitForOneDriveLink(t *testing.T, r interface{ Read([]byte) (int, error) }, timeout time.Duration) string {
	t.Helper()
	result := make(chan string, 1)

	go func() {
		scanner := bufio.NewScanner(r)
		scanner.Buffer(make([]byte, 0, 64*1024), 256*1024)
		count := 0
		for scanner.Scan() {
			line := scanner.Text()
			fmt.Fprintln(os.Stderr, line)
			clean := ansiStripRe.ReplaceAllString(line, "")
			if idx := strings.Index(clean, "simplex+onedrive://"); idx >= 0 {
				remainder := clean[idx:]
				if spaceIdx := strings.IndexByte(remainder, ' '); spaceIdx > 0 {
					remainder = remainder[:spaceIdx]
				}
				remainder = strings.TrimSpace(remainder)
				count++
				// The first URL is the console log (no resolved params),
				// the second URL is the resolved one after channel start.
				if count >= 2 {
					result <- remainder
					return
				}
			}
		}
	}()

	select {
	case u := <-result:
		return u
	case <-time.After(timeout):
		t.Fatal("timeout waiting for OneDrive server to output resolved URL")
		return ""
	}
}

// setupOneDriveE2E launches an OneDrive-backed REM server and client,
// waits for the SOCKS5 listener, and returns its address.
func setupOneDriveE2E(t *testing.T) string {
	t.Helper()
	alias := fmt.Sprintf("ode2e%d", atomic.AddUint32(&testCounter, 1))
	dirName := fmt.Sprintf("rem-od-%d", time.Now().UnixNano()%100000)
	serverURL := odTestURL(dirName)

	// --- server subprocess ---
	serverProc, stdoutPipe := startODServer(t, serverURL)
	t.Cleanup(func() { serverProc.Process.Kill(); serverProc.Wait() })

	resolvedURL := waitForOneDriveLink(t, stdoutPipe, 120*time.Second)
	t.Logf("Server resolved URL: %s", resolvedURL)
	time.Sleep(5 * time.Second)

	socksPort := freePort(t)
	clientProc := startODClient(t, resolvedURL, socksPort, alias)
	t.Cleanup(func() { clientProc.Process.Kill(); clientProc.Wait() })

	socksAddr := fmt.Sprintf("127.0.0.1:%d", socksPort)
	waitForTCP(t, socksAddr, 180*time.Second)
	time.Sleep(10 * time.Second)
	return socksAddr
}

// TestE2E_OneDrive_SOCKS5Proxy verifies basic HTTP via SOCKS5 over OneDrive transport.
func TestE2E_OneDrive_SOCKS5Proxy(t *testing.T) {
	if os.Getenv("SHAREPOINT_REAL") != "1" {
		t.Skip("Skipping OneDrive integration test (set SHAREPOINT_REAL=1 to run)")
	}

	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte("hello onedrive rem"))
	}))
	defer ts.Close()

	socksAddr := setupOneDriveE2E(t)
	httpViaSOCKS5WithRetry(t, socksAddr, ts.URL, "onedrive-socks5", 180*time.Second)
	t.Log("OneDrive SOCKS5 proxy test PASSED")
}

// TestE2E_OneDrive_ServerRestart kills the server, restarts it, and verifies recovery.
func TestE2E_OneDrive_ServerRestart(t *testing.T) {
	if os.Getenv("SHAREPOINT_REAL") != "1" {
		t.Skip("Skipping OneDrive server restart test (set SHAREPOINT_REAL=1 to run)")
	}

	const target = "http://www.baidu.com"
	dirName := fmt.Sprintf("rem-od-restart-%d", time.Now().UnixNano()%100000)
	serverURL := odTestURL(dirName)

	// Phase 0: start server1
	t.Log("Phase 0: starting server1...")
	serverProc1, stdoutPipe1 := startODServer(t, serverURL)

	resolvedURL := waitForOneDriveLink(t, stdoutPipe1, 120*time.Second)
	t.Logf("server1 resolved URL: %s", resolvedURL)
	time.Sleep(5 * time.Second)

	// Start client (stays alive)
	socksPort := freePort(t)
	socksAddr := fmt.Sprintf("127.0.0.1:%d", socksPort)
	alias := fmt.Sprintf("odrestart-%d", atomic.AddUint32(&testCounter, 1))
	clientProc := startODClient(t, resolvedURL, socksPort, alias)
	t.Cleanup(func() { clientProc.Process.Kill(); clientProc.Wait() })

	waitForTCP(t, socksAddr, 180*time.Second)
	time.Sleep(10 * time.Second)

	// Phase 1: baseline
	t.Log("Phase 1: baseline HTTP GET")
	httpViaSOCKS5WithRetry(t, socksAddr, target, "baseline", 180*time.Second)
	t.Log("Phase 1: PASS")

	// Phase 2: kill server, restart
	t.Log("Phase 2: killing server1, restarting server2...")
	serverProc1.Process.Kill()
	serverProc1.Wait()
	time.Sleep(2 * time.Second)

	serverProc2, stdoutPipe2 := startODServer(t, resolvedURL)
	t.Cleanup(func() { serverProc2.Process.Kill(); serverProc2.Wait() })
	waitForOneDriveLink(t, stdoutPipe2, 60*time.Second)
	// Extra stabilization: server2 needs time to discover client files and
	// the client needs time to re-login through the OneDrive polling loop.
	time.Sleep(20 * time.Second)

	// Phase 3: verify recovery
	t.Log("Phase 3: verifying HTTP after server restart...")
	httpViaSOCKS5WithRetry(t, socksAddr, target, "after-server-restart", 180*time.Second)
	t.Log("Phase 3: PASS")

	t.Log("PASS: OneDrive server restart — HTTP recovers")
}

// TestE2E_OneDrive_ClientRestart kills the client, starts a new one, and verifies recovery.
func TestE2E_OneDrive_ClientRestart(t *testing.T) {
	if os.Getenv("SHAREPOINT_REAL") != "1" {
		t.Skip("Skipping OneDrive client restart test (set SHAREPOINT_REAL=1 to run)")
	}

	const target = "http://www.baidu.com"
	dirName := fmt.Sprintf("rem-od-clirestart-%d", time.Now().UnixNano()%100000)
	serverURL := odTestURL(dirName)

	// Phase 0: start server (stays alive)
	t.Log("Phase 0: starting server...")
	serverProc, stdoutPipe := startODServer(t, serverURL)
	t.Cleanup(func() { serverProc.Process.Kill(); serverProc.Wait() })

	resolvedURL := waitForOneDriveLink(t, stdoutPipe, 120*time.Second)
	t.Logf("server resolved URL: %s", resolvedURL)
	time.Sleep(5 * time.Second)

	// Start client1
	socksPort1 := freePort(t)
	socksAddr1 := fmt.Sprintf("127.0.0.1:%d", socksPort1)
	alias1 := fmt.Sprintf("odcli-%d-a", atomic.AddUint32(&testCounter, 1))
	clientProc1 := startODClient(t, resolvedURL, socksPort1, alias1)

	waitForTCP(t, socksAddr1, 180*time.Second)
	time.Sleep(10 * time.Second)

	// Phase 1: baseline
	t.Log("Phase 1: baseline HTTP GET via client1")
	httpViaSOCKS5WithRetry(t, socksAddr1, target, "baseline", 180*time.Second)
	t.Log("Phase 1: PASS")

	// Phase 2: kill client1, start client2
	t.Log("Phase 2: killing client1, starting client2...")
	clientProc1.Process.Kill()
	clientProc1.Wait()
	time.Sleep(2 * time.Second)

	socksPort2 := freePort(t)
	socksAddr2 := fmt.Sprintf("127.0.0.1:%d", socksPort2)
	alias2 := fmt.Sprintf("odcli-%d-b", atomic.AddUint32(&testCounter, 1))
	clientProc2 := startODClient(t, resolvedURL, socksPort2, alias2)
	t.Cleanup(func() { clientProc2.Process.Kill(); clientProc2.Wait() })

	waitForTCP(t, socksAddr2, 180*time.Second)
	time.Sleep(10 * time.Second)

	// Phase 3: verify
	t.Log("Phase 3: verifying HTTP via client2...")
	httpViaSOCKS5WithRetry(t, socksAddr2, target, "after-client-restart", 180*time.Second)
	t.Log("Phase 3: PASS")

	t.Log("PASS: OneDrive client restart — HTTP recovers")
}

// TestE2E_OneDrive_NetworkOutage30s kills server for 30s and verifies recovery.
func TestE2E_OneDrive_NetworkOutage30s(t *testing.T) {
	if os.Getenv("SHAREPOINT_REAL") != "1" {
		t.Skip("Skipping OneDrive network outage test (set SHAREPOINT_REAL=1 to run)")
	}

	const target = "http://www.baidu.com"
	dirName := fmt.Sprintf("rem-od-outage-%d", time.Now().UnixNano()%100000)
	serverURL := odTestURL(dirName)

	// Phase 0: start server1
	t.Log("Phase 0: starting server1...")
	serverProc1, stdoutPipe1 := startODServer(t, serverURL)

	resolvedURL := waitForOneDriveLink(t, stdoutPipe1, 120*time.Second)
	t.Logf("server1 resolved URL: %s", resolvedURL)
	time.Sleep(5 * time.Second)

	// Start client (stays alive)
	socksPort := freePort(t)
	socksAddr := fmt.Sprintf("127.0.0.1:%d", socksPort)
	alias := fmt.Sprintf("odoutage-%d", atomic.AddUint32(&testCounter, 1))
	clientProc := startODClient(t, resolvedURL, socksPort, alias)
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
	serverProc2, stdoutPipe2 := startODServer(t, resolvedURL)
	t.Cleanup(func() { serverProc2.Process.Kill(); serverProc2.Wait() })
	waitForOneDriveLink(t, stdoutPipe2, 60*time.Second)
	time.Sleep(20 * time.Second) // stabilization for client re-login

	// Phase 4: verify
	t.Log("Phase 4: verifying HTTP recovery after 30s outage...")
	httpViaSOCKS5WithRetry(t, socksAddr, target, "after-30s-outage", 180*time.Second)
	t.Log("Phase 4: PASS")

	t.Log("PASS: OneDrive 30s network outage — HTTP recovers")
}

// TestE2E_OneDrive_RapidClientRestart verifies that the server handles 3x rapid
// client restarts, each with a new random clientID.
func TestE2E_OneDrive_RapidClientRestart(t *testing.T) {
	if os.Getenv("SHAREPOINT_REAL") != "1" {
		t.Skip("Skipping OneDrive rapid client restart test (set SHAREPOINT_REAL=1 to run)")
	}

	const target = "http://www.baidu.com"
	dirName := fmt.Sprintf("rem-od-rapid-%d", time.Now().UnixNano()%100000)

	// Start server (stays alive for entire test)
	t.Log("Phase 0: starting server...")
	serverProc, stdoutPipe := startODServer(t, odTestURL(dirName))
	t.Cleanup(func() { serverProc.Process.Kill(); serverProc.Wait() })

	resolvedURL := waitForOneDriveLink(t, stdoutPipe, 120*time.Second)
	t.Logf("server resolved URL: %s", resolvedURL)
	time.Sleep(5 * time.Second)

	// Phase 1: client1 — baseline
	socksPort1 := freePort(t)
	socksAddr1 := fmt.Sprintf("127.0.0.1:%d", socksPort1)
	alias1 := fmt.Sprintf("rapid-%d-a", atomic.AddUint32(&testCounter, 1))
	clientProc1 := startODClient(t, resolvedURL, socksPort1, alias1)

	waitForTCP(t, socksAddr1, 180*time.Second)
	time.Sleep(10 * time.Second)

	t.Log("Phase 1: baseline HTTP via client1")
	httpViaSOCKS5WithRetry(t, socksAddr1, target, "rapid-baseline", 180*time.Second)
	t.Log("Phase 1: PASS")

	// Phase 2: kill client1, start client2 (new random clientID)
	t.Log("Phase 2: killing client1, starting client2 immediately...")
	clientProc1.Process.Kill()
	clientProc1.Wait()
	time.Sleep(1 * time.Second)

	socksPort2 := freePort(t)
	socksAddr2 := fmt.Sprintf("127.0.0.1:%d", socksPort2)
	alias2 := fmt.Sprintf("rapid-%d-b", atomic.AddUint32(&testCounter, 1))
	clientProc2 := startODClient(t, resolvedURL, socksPort2, alias2)

	waitForTCP(t, socksAddr2, 180*time.Second)
	time.Sleep(10 * time.Second)

	t.Log("Phase 2: HTTP via client2 (new identity)")
	httpViaSOCKS5WithRetry(t, socksAddr2, target, "rapid-restart-1", 180*time.Second)
	t.Log("Phase 2: PASS")

	// Phase 3: kill client2, start client3 (third identity)
	t.Log("Phase 3: killing client2, starting client3...")
	clientProc2.Process.Kill()
	clientProc2.Wait()
	time.Sleep(1 * time.Second)

	socksPort3 := freePort(t)
	socksAddr3 := fmt.Sprintf("127.0.0.1:%d", socksPort3)
	alias3 := fmt.Sprintf("rapid-%d-c", atomic.AddUint32(&testCounter, 1))
	clientProc3 := startODClient(t, resolvedURL, socksPort3, alias3)
	t.Cleanup(func() { clientProc3.Process.Kill(); clientProc3.Wait() })

	waitForTCP(t, socksAddr3, 180*time.Second)
	time.Sleep(10 * time.Second)

	t.Log("Phase 3: HTTP via client3 (third identity)")
	httpViaSOCKS5WithRetry(t, socksAddr3, target, "rapid-restart-2", 180*time.Second)
	t.Log("Phase 3: PASS")

	t.Log("PASS: OneDrive rapid client restart — server discovers each new identity")
}

// TestE2E_OneDrive_NetworkOutage60s kills the server for 60s and verifies recovery.
func TestE2E_OneDrive_NetworkOutage60s(t *testing.T) {
	if os.Getenv("SHAREPOINT_REAL") != "1" {
		t.Skip("Skipping OneDrive 60s outage test (set SHAREPOINT_REAL=1 to run)")
	}

	const target = "http://www.baidu.com"
	dirName := fmt.Sprintf("rem-od-outage60-%d", time.Now().UnixNano()%100000)
	serverURL := odTestURL(dirName)

	t.Log("Phase 0: starting server1...")
	serverProc1, stdoutPipe1 := startODServer(t, serverURL)

	resolvedURL := waitForOneDriveLink(t, stdoutPipe1, 120*time.Second)
	t.Logf("server1 resolved URL: %s", resolvedURL)
	time.Sleep(5 * time.Second)

	socksPort := freePort(t)
	socksAddr := fmt.Sprintf("127.0.0.1:%d", socksPort)
	alias := fmt.Sprintf("odoutage60-%d", atomic.AddUint32(&testCounter, 1))
	clientProc := startODClient(t, resolvedURL, socksPort, alias)
	t.Cleanup(func() { clientProc.Process.Kill(); clientProc.Wait() })

	waitForTCP(t, socksAddr, 180*time.Second)
	time.Sleep(10 * time.Second)

	t.Log("Phase 1: baseline HTTP GET")
	httpViaSOCKS5WithRetry(t, socksAddr, target, "baseline", 180*time.Second)
	t.Log("Phase 1: PASS")

	t.Log("Phase 2: killing server, waiting 60s...")
	serverProc1.Process.Kill()
	serverProc1.Wait()
	time.Sleep(60 * time.Second)

	t.Log("Phase 3: restarting server...")
	serverProc2, stdoutPipe2 := startODServer(t, resolvedURL)
	t.Cleanup(func() { serverProc2.Process.Kill(); serverProc2.Wait() })
	waitForOneDriveLink(t, stdoutPipe2, 60*time.Second)
	time.Sleep(20 * time.Second) // stabilization for client re-login

	t.Log("Phase 4: verifying HTTP recovery after 60s outage...")
	httpViaSOCKS5WithRetry(t, socksAddr, target, "after-60s-outage", 180*time.Second)
	t.Log("Phase 4: PASS")

	t.Log("PASS: OneDrive 60s network outage — HTTP recovers")
}

// TestE2E_OneDrive_IdleResume idles 90s then verifies HTTP still works.
func TestE2E_OneDrive_IdleResume(t *testing.T) {
	if os.Getenv("SHAREPOINT_REAL") != "1" {
		t.Skip("Skipping OneDrive idle resume test (set SHAREPOINT_REAL=1 to run)")
	}

	const target = "http://www.baidu.com"

	socksAddr := setupOneDriveE2E(t)

	t.Log("Phase 1: baseline HTTP GET")
	httpViaSOCKS5WithRetry(t, socksAddr, target, "baseline", 180*time.Second)

	t.Log("Phase 2: idling for 90 seconds...")
	time.Sleep(90 * time.Second)

	t.Log("Phase 3: verifying HTTP after idle...")
	httpViaSOCKS5WithRetry(t, socksAddr, target, "after-idle", 60*time.Second)

	t.Log("PASS: OneDrive idle resume — HTTP works after 90s idle")
}

// TestE2E_OneDrive_BurstTraffic sends 3 sequential HTTP GETs.
func TestE2E_OneDrive_BurstTraffic(t *testing.T) {
	if os.Getenv("SHAREPOINT_REAL") != "1" {
		t.Skip("Skipping OneDrive burst traffic test (set SHAREPOINT_REAL=1 to run)")
	}

	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte("burst-ok"))
	}))
	defer ts.Close()

	socksAddr := setupOneDriveE2E(t)

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

	t.Log("PASS: OneDrive burst traffic — sequential HTTP GETs succeed")
}

// TestE2E_OneDrive_LargePayload sends a 100KB payload and verifies SHA256.
func TestE2E_OneDrive_LargePayload(t *testing.T) {
	if os.Getenv("SHAREPOINT_REAL") != "1" {
		t.Skip("Skipping OneDrive large payload test (set SHAREPOINT_REAL=1 to run)")
	}

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

	socksAddr := setupOneDriveE2E(t)

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

	t.Log("PASS: OneDrive large payload — 100KB transferred with SHA256 integrity")
}

// TestE2E_OneDrive_ConcurrentRequests sends 2 concurrent HTTP GETs.
func TestE2E_OneDrive_ConcurrentRequests(t *testing.T) {
	if os.Getenv("SHAREPOINT_REAL") != "1" {
		t.Skip("Skipping OneDrive concurrent requests test (set SHAREPOINT_REAL=1 to run)")
	}

	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte("concurrent-ok"))
	}))
	defer ts.Close()

	socksAddr := setupOneDriveE2E(t)

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

	t.Log("PASS: OneDrive concurrent requests — parallel HTTP GETs succeed")
}

// TestE2E_OneDrive_HighLatency uses interval=8000 to simulate degraded conditions.
func TestE2E_OneDrive_HighLatency(t *testing.T) {
	if os.Getenv("SHAREPOINT_REAL") != "1" {
		t.Skip("Skipping OneDrive high latency test (set SHAREPOINT_REAL=1 to run)")
	}

	const target = "http://www.baidu.com"
	dirName := fmt.Sprintf("rem-od-hilat-%d", time.Now().UnixNano()%100000)
	serverURL := odTestURLWithInterval(dirName, 8000) // 8s polling

	t.Logf("Using high-latency config: interval=8000ms")

	serverProc, stdoutPipe := startODServer(t, serverURL)
	t.Cleanup(func() { serverProc.Process.Kill(); serverProc.Wait() })

	resolvedURL := waitForOneDriveLink(t, stdoutPipe, 120*time.Second)
	t.Logf("server resolved URL: %s", resolvedURL)
	time.Sleep(5 * time.Second)

	socksPort := freePort(t)
	socksAddr := fmt.Sprintf("127.0.0.1:%d", socksPort)
	alias := fmt.Sprintf("odhilat-%d", atomic.AddUint32(&testCounter, 1))
	clientProc := startODClient(t, resolvedURL, socksPort, alias)
	t.Cleanup(func() { clientProc.Process.Kill(); clientProc.Wait() })

	waitForTCP(t, socksAddr, 180*time.Second)
	// Extra stabilization time for slow polling
	time.Sleep(20 * time.Second)

	// With 8s polling, a single SOCKS5+HTTP roundtrip can take ~100s
	// (3 message roundtrips × 32s each). Use a much longer per-attempt timeout.
	t.Log("Phase 1: HTTP GET under high-latency (8s interval)")
	limit := time.Now().Add(300 * time.Second)
	for {
		client := newSOCKS5Client(t, socksAddr, 120*time.Second) // 120s per attempt
		resp, err := client.Get(target)
		client.CloseIdleConnections()
		if err == nil && resp.StatusCode < 400 {
			resp.Body.Close()
			t.Logf("[high-latency] HTTP %d from %s — OK", resp.StatusCode, target)
			break
		}
		if resp != nil {
			resp.Body.Close()
		}
		if time.Now().After(limit) {
			if err != nil {
				t.Fatalf("[high-latency] HTTP GET %s timed out after 300s: %v", target, err)
			}
			t.Fatalf("[high-latency] HTTP %d from %s (deadline exceeded)", resp.StatusCode, target)
		}
		t.Logf("[high-latency] retrying (%v remaining)...", time.Until(limit).Truncate(time.Second))
		time.Sleep(5 * time.Second)
	}
	t.Log("Phase 1: PASS")

	t.Log("PASS: OneDrive high-latency — HTTP works with 8s polling interval")
}

// TestE2E_OneDrive_Stability runs a 5-minute stability test, probing every 15s.
// Uses a local httptest.Server to avoid external dependency and reduce per-probe latency.
// Duration limited to 5 minutes because the rem keepalive timeout (~3m without pongs)
// can trigger a non-recoverable reconnection on OneDrive transport (known issue: the old
// OneDriveClient's monitoring goroutine leaks, consuming API resources).
func TestE2E_OneDrive_Stability(t *testing.T) {
	if os.Getenv("SHAREPOINT_REAL") != "1" {
		t.Skip("Skipping OneDrive stability test (set SHAREPOINT_REAL=1 to run)")
	}

	const (
		testDuration  = 5 * time.Minute
		probeInterval = 15 * time.Second
	)

	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte("stability-ok"))
	}))
	defer ts.Close()
	targetURL := ts.URL

	socksAddr := setupOneDriveE2E(t)
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
		if consecutiveFails > 0 {
			conn, dialErr := net.DialTimeout("tcp", socksAddr, 2*time.Second)
			if dialErr != nil {
				t.Logf("[%v] probe #%d: SOCKS5 port down, waiting for reconnect...", elapsed, total)
				// OneDrive reconnection can take 3+ minutes:
				// keepalive timeout (3m) + new client creation + login handshake (~60s)
				reconnectDeadline := time.Now().Add(300 * time.Second)
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
					t.Fatalf("SOCKS5 port did not recover within 300s. Last error: %v", lastErr)
				}
				reconnects++
				t.Logf("[%v] probe #%d: SOCKS5 port recovered (reconnect #%d)", elapsed, total, reconnects)
				time.Sleep(10 * time.Second) // extra stabilization after reconnect
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

		if total%10 == 0 {
			rate := float64(success) / float64(total) * 100
			t.Logf("[%v] === %d probes: %d ok, %d fail, %d reconnects (%.1f%% success) ===",
				elapsed, total, success, fail, reconnects, rate)
		}

		if consecutiveFails >= 8 {
			t.Fatalf("8 consecutive failures at probe %d — tunnel likely dead. Last error: %v", total, lastErr)
		}

		time.Sleep(probeInterval)
	}

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

// TestE2E_OneDrive_ConsecutiveRestarts kills and restarts the server 5 times,
// verifying HTTP recovery after each restart.  Tests the LoginTimeout fix:
// without it, Login() blocks forever and the SOCKS5 port never re-binds.
func TestE2E_OneDrive_ConsecutiveRestarts(t *testing.T) {
	if os.Getenv("SHAREPOINT_REAL") != "1" {
		t.Skip("Skipping OneDrive consecutive restart test (set SHAREPOINT_REAL=1 to run)")
	}

	const (
		restartCount     = 5
		perRestartWindow = 300 * time.Second // max wait per restart cycle
	)

	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte("restart-ok"))
	}))
	defer ts.Close()

	dirName := fmt.Sprintf("rem-od-consrestart-%d", time.Now().UnixNano()%100000)
	serverURL := odTestURL(dirName)

	// Phase 0: initial setup
	t.Log("Phase 0: starting server1 + client")
	serverProc, stdoutPipe := startODServer(t, serverURL)

	resolvedURL := waitForOneDriveLink(t, stdoutPipe, 120*time.Second)
	t.Logf("Server resolved URL: %s", resolvedURL)
	time.Sleep(5 * time.Second)

	socksPort := freePort(t)
	socksAddr := fmt.Sprintf("127.0.0.1:%d", socksPort)
	alias := fmt.Sprintf("odcons-%d", atomic.AddUint32(&testCounter, 1))
	clientProc := startODClient(t, resolvedURL, socksPort, alias)
	t.Cleanup(func() { clientProc.Process.Kill(); clientProc.Wait() })

	waitForTCP(t, socksAddr, 180*time.Second)
	time.Sleep(10 * time.Second)

	// Baseline
	t.Log("Phase 0: baseline HTTP GET")
	httpViaSOCKS5WithRetry(t, socksAddr, ts.URL, "baseline", 180*time.Second)
	t.Log("Phase 0: PASS")

	// Restart loop
	for i := 1; i <= restartCount; i++ {
		t.Logf("===== Restart %d/%d =====", i, restartCount)

		// Kill current server
		t.Logf("  Killing server%d...", i)
		serverProc.Process.Kill()
		serverProc.Wait()
		time.Sleep(2 * time.Second)

		// Start new server with resolved URL (preserves dir)
		t.Logf("  Starting server%d...", i+1)
		serverProc, stdoutPipe = startODServer(t, resolvedURL)

		waitForOneDriveLink(t, stdoutPipe, 60*time.Second)
		t.Logf("  Server%d up, waiting for client reconnect...", i+1)

		// Wait for SOCKS5 port to come back (client reconnect)
		deadline := time.Now().Add(perRestartWindow)
		recovered := false
		for time.Now().Before(deadline) {
			// Try HTTP via SOCKS5
			client := newSOCKS5Client(t, socksAddr, 30*time.Second)
			resp, err := client.Get(ts.URL)
			client.CloseIdleConnections()
			if err == nil {
				body, _ := io.ReadAll(resp.Body)
				resp.Body.Close()
				if string(body) == "restart-ok" {
					recovered = true
					t.Logf("  Restart %d: HTTP OK after %v", i, time.Since(deadline.Add(-perRestartWindow)).Truncate(time.Second))
					break
				}
			}
			time.Sleep(5 * time.Second)
		}

		if !recovered {
			t.Fatalf("  Restart %d: SOCKS5 HTTP never recovered within %v", i, perRestartWindow)
		}
	}

	// Cleanup: kill the last server
	serverProc.Process.Kill()
	serverProc.Wait()

	t.Logf("PASS: OneDrive %d consecutive server restarts — all recovered", restartCount)
}
