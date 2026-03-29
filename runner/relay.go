package runner

import (
	"fmt"
	"io"
	"net"
	"net/url"
	"sync"

	"github.com/chainreactors/rem/protocol/core"
	"github.com/chainreactors/rem/x/utils"
)

// StartRelayListeners starts raw TCP relay listeners for each -s address.
// Each accepted downstream connection is transparently proxied to the upstream server.
// The relay does not decrypt or re-encrypt data — wrapper encryption is end-to-end
// between the downstream node and the root server.
func StartRelayListeners(upstreamURL *core.URL, relayListenURLs []*core.URL, agentID string, runner *RunnerConfig) {
	for _, listenURL := range relayListenURLs {
		go runRelayListener(listenURL, upstreamURL, agentID, runner)
	}
}

func runRelayListener(listenURL, upstreamURL *core.URL, agentID string, runner *RunnerConfig) {
	ln, err := net.Listen("tcp", listenURL.Host)
	if err != nil {
		utils.Log.Errorf("[relay] listen %s: %v", listenURL.Host, err)
		return
	}
	defer ln.Close()

	// Build and print relay link for downstream nodes to use.
	relayLink := buildRelayLink(upstreamURL, listenURL, agentID, runner.IP)
	utils.Log.Importantf("[relay] %s listening on %s", agentID, listenURL.Host)
	utils.Log.Importantf("[relay] relay link: %s", relayLink)

	// The upstream address for proxy connections: raw TCP (no wrapper).
	// Downstream nodes encrypt with root's wrapper; we just pipe raw bytes.
	upstreamAddr := upstreamURL.Host

	var wg sync.WaitGroup
	for {
		down, err := ln.Accept()
		if err != nil {
			utils.Log.Debugf("[relay] accept: %v", err)
			break
		}
		wg.Add(1)
		go func(down net.Conn) {
			defer wg.Done()
			relayConn(down, upstreamAddr)
		}(down)
	}
	wg.Wait()
}

// relayConn transparently pipes a downstream connection to upstream via raw TCP.
// When either direction closes, both connections are closed immediately to avoid
// leaving the upstream server's Accept() blocked on a phantom connection.
func relayConn(down net.Conn, upstreamAddr string) {
	utils.Log.Debugf("[relay] accepted from %s, dialing upstream %s", down.RemoteAddr(), upstreamAddr)
	up, err := net.Dial("tcp", upstreamAddr)
	if err != nil {
		utils.Log.Errorf("[relay] dial upstream %s: %v", upstreamAddr, err)
		down.Close()
		return
	}
	utils.Log.Debugf("[relay] proxy established: %s <-> %s", down.RemoteAddr(), upstreamAddr)

	// closeOnce ensures both connections are closed when either direction ends.
	var once sync.Once
	closeAll := func() {
		once.Do(func() {
			down.Close()
			up.Close()
		})
	}
	defer closeAll()

	done := make(chan struct{})
	go func() {
		io.Copy(up, down)
		closeAll() // close both so the other io.Copy unblocks
		close(done)
	}()
	io.Copy(down, up)
	closeAll()
	<-done
	utils.Log.Debugf("[relay] proxy closed: %s", down.RemoteAddr())
}

// buildRelayLink generates a connection link for downstream nodes.
// It takes the upstream URL (with wrapper/key config), replaces host:port
// with the relay's listen address, and appends &via=agentID.
func buildRelayLink(upstreamURL, listenURL *core.URL, agentID, externalIP string) string {
	// Clone the upstream URL to preserve wrapper/key configuration.
	u, err := url.Parse(upstreamURL.String())
	if err != nil {
		return fmt.Sprintf("tcp://%s (error building relay link: %v)", listenURL.Host, err)
	}

	// Replace host with relay's external address.
	host := externalIP
	if host == "" {
		host = "0.0.0.0"
	}
	_, port, _ := net.SplitHostPort(listenURL.Host)
	u.Host = net.JoinHostPort(host, port)

	// Append via parameter.
	q := u.Query()
	q.Set("via", agentID)
	u.RawQuery = q.Encode()

	return u.String()
}
