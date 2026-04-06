package runner

import (
	"net"
	"net/url"
	"testing"

	"github.com/chainreactors/proxyclient"
	"github.com/chainreactors/rem/harness/netconn"
)

func TestNewConsole(t *testing.T) {
	u, err := url.Parse("rem+tcp://nonenonenonenone:@127.0.0.1:34996/?wrapper=raw")
	if err != nil {
		t.Fatalf("parse proxy URL: %v", err)
	}
	proxy, err := proxyclient.NewClient(u)
	if err != nil {
		t.Fatalf("proxyclient.NewClient: %v", err)
	}
	if proxy == nil {
		t.Fatal("expected non-nil proxy dialer")
	}
}

func TestClient(t *testing.T) {
	t.Skip("manual integration test")
}

func TestRemProxy(t *testing.T) {
	t.Skip("manual integration test")
}

func TestRemHTTPProxy(t *testing.T) {
	t.Skip("manual integration test")
}

func TestServer(t *testing.T) {
	t.Skip("manual integration test")
}

// ---------------------------------------------------------------------------
// Merged from connhub_runtime_netconn_test.go
// ---------------------------------------------------------------------------

func TestMergeHalfConn_NetConn(t *testing.T) {
	netconn.TestConn(t, func() (c1, c2 net.Conn, stop func(), err error) {
		write1, read2 := net.Pipe()
		write2, read1 := net.Pipe()

		c1 = mergeHalfConn(read1, write1)
		c2 = mergeHalfConn(read2, write2)

		stop = func() {
			_ = c1.Close()
			_ = c2.Close()
		}
		return c1, c2, stop, nil
	})
}
