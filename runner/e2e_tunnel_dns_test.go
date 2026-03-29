//go:build dns && !tinygo

package runner

import (
	"fmt"
	"runtime"
	"sync/atomic"
	"testing"
	"time"

	"github.com/chainreactors/rem/harness/runner"
	_ "github.com/chainreactors/rem/protocol/tunnel/dns"
)

func init() {
	registerTunnelCases(runner.CaseSpec{
		Name: "dns_transport",
		Check: func(*testing.T) string {
			if runtime.GOOS == "windows" {
				return "dns transport smoke is only verified in Linux CI"
			}
			return ""
		},
		Build: buildDNSTransportFixture,
		Scenarios: []runner.Scenario{
			socksSmokeScenario("hello dns_transport"),
			largeTransferScenario(16 * 1024),
		},
	})
}

func buildDNSTransportFixture(t *testing.T) runner.FixtureSpec {
	dnsPort := freeUDPPort(t)
	socksPort := freePort(t)
	alias := fmt.Sprintf("dns_%d", atomic.AddUint32(&testCounter, 1))

	return runner.FixtureSpec{
		HelperTest: e2eHelperTestPattern,
		Vars: runner.Vars{
			"dns_port":   fmt.Sprintf("%d", dnsPort),
			"dns_addr":   fmt.Sprintf("127.0.0.1:%d", dnsPort),
			"socks_port": fmt.Sprintf("%d", socksPort),
			"socks_addr": fmt.Sprintf("127.0.0.1:%d", socksPort),
			"alias":      alias,
		},
		Processes: []runner.ProcessSpec{
			{
				Name:    "server",
				Mode:    "console_run",
				Command: "--debug -s dns://0.0.0.0:{{dns_port}}/?wrapper=raw&domain=rem.test.local&interval=50 -i 127.0.0.1 --no-sub",
				Wait:    []runner.ReadySpec{{Kind: runner.ReadyDelayOnly, Timeout: 5 * time.Second}},
			},
			{
				Name:    "client",
				Mode:    "console_run",
				Command: "-c dns://{{dns_addr}}/?wrapper=raw&domain=rem.test.local&interval=50 -l socks5://127.0.0.1:{{socks_port}} -a {{alias}} --debug",
				Wait: []runner.ReadySpec{
					{Kind: runner.ReadyTCPListen, Target: "{{socks_addr}}", Timeout: 40 * time.Second},
					{Kind: runner.ReadyDelayOnly, Timeout: 500 * time.Millisecond},
				},
			},
		},
	}
}
