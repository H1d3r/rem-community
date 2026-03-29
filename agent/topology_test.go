package agent

import (
	"encoding/json"
	"fmt"
	"io"
	"net"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/chainreactors/rem/protocol/core"
	"github.com/chainreactors/rem/x/utils"
)

// mockInbound implements core.Inbound for testing.
type mockInbound struct {
	name  string
	proxy *utils.Proxies
}

func (m *mockInbound) Name() string                                                    { return m.name }
func (m *mockInbound) Relay(_ net.Conn, _ io.ReadWriteCloser) (net.Conn, error)        { return nil, nil }
func (m *mockInbound) ToClash() *utils.Proxies                                        { return m.proxy }

// mockOutbound implements core.Outbound for testing.
type mockOutbound struct {
	name  string
	proxy *utils.Proxies
}

func (m *mockOutbound) Name() string                                                    { return m.name }
func (m *mockOutbound) Handle(_ io.ReadWriteCloser, _ net.Conn) (net.Conn, error)       { return nil, nil }
func (m *mockOutbound) ToClash() *utils.Proxies                                        { return m.proxy }

func newMockAgent(id, typ, mod, via, redirect string) *Agent {
	cfg := &Config{
		Alias: id,
		Type:  typ,
		Via:   via,
		URLs:  &core.URLs{},
	}
	if redirect != "" {
		cfg.Redirect = redirect
	}
	a := &Agent{
		Config: cfg,
		ID:     id,
	}
	return a
}

func setInbound(a *Agent, proto, host string, port int) {
	a.Inbound = &mockInbound{
		name:  proto,
		proxy: &utils.Proxies{Type: proto, Server: host, Port: port},
	}
}

func setOutbound(a *Agent, proto, host string, port int) {
	a.Outbound = &mockOutbound{
		name:  proto,
		proxy: &utils.Proxies{Type: proto, Server: host, Port: port},
	}
}

func setURLs(a *Agent, local, remote string) {
	if local != "" {
		a.LocalURL, _ = core.NewURL(local)
	}
	if remote != "" {
		a.RemoteURL, _ = core.NewURL(remote)
	}
}

func addChild(parent *Agent, child *Agent) {
	parent.children.Store(child.ID, child)
}

func setBridges(a *Agent, n int64) {
	atomic.StoreInt64(&a.connCount, n)
}

// TestTopology_10Nodes_30Serves builds a 10-node network with 30 serves
// and outputs topology JSON + mermaid diagram.
//
// Network topology:
//
//   root (server)
//   ├── relay-A (直连)
//   │   ├── relay-B (via A)
//   │   │   ├── node-E (via B)
//   │   │   └── node-F (via B)
//   │   └── node-C (via A)
//   ├── relay-D (直连)
//   │   ├── node-G (via D)
//   │   └── relay-H (via D)
//   │       └── node-I (via H)
//   └── node-J (直连)
//
func TestTopology_10Nodes_30Serves(t *testing.T) {
	// Clear global state
	Agents.Range(func(key, _ interface{}) bool {
		Agents.Delete(key)
		return true
	})

	// ====== 10 nodes ======

	relayA := newMockAgent("relay-A", core.SERVER, "reverse", "", "")
	relayA.Hostname = "dmz-gateway"
	relayA.Interfaces = []string{"10.1.0.1/24", "192.168.1.100/24"}

	relayB := newMockAgent("relay-B", core.SERVER, "reverse", "relay-A", "")
	relayB.Hostname = "office-proxy"
	relayB.Interfaces = []string{"10.2.0.1/24"}

	nodeC := newMockAgent("node-C", core.SERVER, "reverse", "relay-A", "")
	nodeC.Hostname = "web-server"
	nodeC.Interfaces = []string{"10.1.0.50/24"}

	relayD := newMockAgent("relay-D", core.SERVER, "reverse", "", "")
	relayD.Hostname = "cloud-vm"
	relayD.Interfaces = []string{"172.16.0.1/24"}

	nodeE := newMockAgent("node-E", core.SERVER, "reverse", "relay-B", "node-G")
	nodeE.Hostname = "dev-box"
	nodeE.Interfaces = []string{"10.2.0.100/24"}

	nodeF := newMockAgent("node-F", core.SERVER, "proxy", "relay-B", "")
	nodeF.Hostname = "db-server"
	nodeF.Interfaces = []string{"10.2.0.200/24"}

	nodeG := newMockAgent("node-G", core.SERVER, "reverse", "relay-D", "")
	nodeG.Hostname = "k8s-node1"
	nodeG.Interfaces = []string{"172.16.0.10/24", "10.244.0.1/16"}

	relayH := newMockAgent("relay-H", core.SERVER, "reverse", "relay-D", "")
	relayH.Hostname = "isolated-gw"
	relayH.Interfaces = []string{"172.16.0.2/24", "10.3.0.1/24"}

	nodeI := newMockAgent("node-I", core.SERVER, "reverse", "relay-H", "node-E")
	nodeI.Hostname = "core-db"
	nodeI.Interfaces = []string{"10.3.0.100/24"}

	nodeJ := newMockAgent("node-J", core.SERVER, "reverse", "", "")
	nodeJ.Hostname = "jump-server"
	nodeJ.Interfaces = []string{"192.168.2.10/24"}

	// ====== 30 serves (inbound + outbound distributed across nodes) ======

	// relay-A: 4 serves
	setInbound(relayA, "socks5", "0.0.0.0", 10080)         // s1
	setOutbound(relayA, "raw", "0.0.0.0", 0)                // s2
	setURLs(relayA, "raw://0.0.0.0:0", "socks5://0.0.0.0:10080")
	setBridges(relayA, 5)
	// fork children on relay-A
	forkA1 := newMockAgent("relay-A:http", core.SERVER, "reverse", "", "")
	setInbound(forkA1, "http", "0.0.0.0", 8080)             // s3
	forkA2 := newMockAgent("relay-A:ss", core.SERVER, "reverse", "", "")
	setInbound(forkA2, "shadowsocks", "0.0.0.0", 8388)      // s4
	addChild(relayA, forkA1)
	addChild(relayA, forkA2)

	// relay-B: 3 serves
	setInbound(relayB, "socks5", "0.0.0.0", 10081)          // s5
	setOutbound(relayB, "raw", "0.0.0.0", 0)                // s6
	setURLs(relayB, "raw://0.0.0.0:0", "socks5://0.0.0.0:10081")
	setBridges(relayB, 3)
	forkB1 := newMockAgent("relay-B:port-fwd", core.SERVER, "reverse", "", "")
	setInbound(forkB1, "port", "0.0.0.0", 3389)             // s7
	setURLs(forkB1, "port://10.2.0.200:3389", "raw://0.0.0.0:3389")
	addChild(relayB, forkB1)

	// node-C: 3 serves (reverse socks5 + port forwards)
	setInbound(nodeC, "socks5", "0.0.0.0", 10082)           // s8
	setOutbound(nodeC, "raw", "0.0.0.0", 0)                 // s9
	setURLs(nodeC, "raw://0.0.0.0:0", "socks5://0.0.0.0:10082")
	forkC1 := newMockAgent("node-C:web", core.SERVER, "reverse", "", "")
	setInbound(forkC1, "port", "0.0.0.0", 8443)             // s10
	setURLs(forkC1, "port://10.1.0.50:443", "raw://0.0.0.0:8443")
	addChild(nodeC, forkC1)

	// relay-D: 4 serves
	setInbound(relayD, "socks5", "0.0.0.0", 10083)          // s11
	setOutbound(relayD, "raw", "0.0.0.0", 0)                // s12
	setURLs(relayD, "raw://0.0.0.0:0", "socks5://0.0.0.0:10083")
	forkD1 := newMockAgent("relay-D:trojan", core.SERVER, "reverse", "", "")
	setInbound(forkD1, "trojan", "0.0.0.0", 443)            // s13
	forkD2 := newMockAgent("relay-D:http", core.SERVER, "reverse", "", "")
	setInbound(forkD2, "http", "0.0.0.0", 8081)             // s14
	addChild(relayD, forkD1)
	addChild(relayD, forkD2)

	// node-E: 3 serves (-d node-G, socks5 + port fwd)
	setInbound(nodeE, "socks5", "0.0.0.0", 10084)           // s15
	setOutbound(nodeE, "raw", "0.0.0.0", 0)                 // s16
	setURLs(nodeE, "raw://0.0.0.0:0", "socks5://0.0.0.0:10084")
	setBridges(nodeE, 2)
	forkE1 := newMockAgent("node-E:ssh", core.SERVER, "reverse", "", "")
	setInbound(forkE1, "port", "0.0.0.0", 2222)             // s17
	setURLs(forkE1, "port://10.2.0.100:22", "raw://0.0.0.0:2222")
	addChild(nodeE, forkE1)

	// node-F: 3 serves (proxy mode, outbound socks5 + port fwd)
	setInbound(nodeF, "socks5", "10.2.0.200", 1080)         // s18
	setOutbound(nodeF, "raw", "0.0.0.0", 0)                 // s19
	setURLs(nodeF, "socks5://10.2.0.200:1080", "raw://0.0.0.0:0")
	forkF1 := newMockAgent("node-F:mysql", core.SERVER, "proxy", "", "")
	setInbound(forkF1, "port", "10.2.0.200", 13306)         // s20
	setURLs(forkF1, "port://10.2.0.200:13306", "port://0.0.0.0:3306")
	addChild(nodeF, forkF1)

	// node-G: 3 serves
	setInbound(nodeG, "socks5", "0.0.0.0", 10085)           // s21
	setOutbound(nodeG, "raw", "0.0.0.0", 0)                 // s22
	setURLs(nodeG, "raw://0.0.0.0:0", "socks5://0.0.0.0:10085")
	setBridges(nodeG, 4)
	forkG1 := newMockAgent("node-G:k8s-api", core.SERVER, "reverse", "", "")
	setInbound(forkG1, "port", "0.0.0.0", 6443)             // s23
	setURLs(forkG1, "port://172.16.0.10:6443", "raw://0.0.0.0:6443")
	addChild(nodeG, forkG1)

	// relay-H: 2 serves
	setInbound(relayH, "socks5", "0.0.0.0", 10086)          // s24
	setOutbound(relayH, "raw", "0.0.0.0", 0)                // s25
	setURLs(relayH, "raw://0.0.0.0:0", "socks5://0.0.0.0:10086")

	// node-I: 3 serves (-d node-E)
	setInbound(nodeI, "socks5", "0.0.0.0", 10087)           // s26
	setOutbound(nodeI, "raw", "0.0.0.0", 0)                 // s27
	setURLs(nodeI, "raw://0.0.0.0:0", "socks5://0.0.0.0:10087")
	setBridges(nodeI, 1)
	forkI1 := newMockAgent("node-I:oracle", core.SERVER, "reverse", "", "")
	setInbound(forkI1, "port", "0.0.0.0", 1521)             // s28
	setURLs(forkI1, "port://10.3.0.100:1521", "raw://0.0.0.0:1521")
	addChild(nodeI, forkI1)

	// node-J: 2 serves
	setInbound(nodeJ, "socks5", "0.0.0.0", 10088)           // s29
	setOutbound(nodeJ, "raw", "0.0.0.0", 0)                 // s30
	setURLs(nodeJ, "raw://0.0.0.0:0", "socks5://0.0.0.0:10088")
	setBridges(nodeJ, 1)

	// Register all main agents
	for _, a := range []*Agent{relayA, relayB, nodeC, relayD, nodeE, nodeF, nodeG, relayH, nodeI, nodeJ} {
		Agents.Store(a.ID, a)
	}

	// ====== Build topology ======
	graph := BuildTopologyGraph("root-server")

	jsonBytes, err := json.MarshalIndent(graph, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("=== TOPOLOGY JSON ===\n%s", string(jsonBytes))

	// Count serves
	totalServices := 0
	for _, n := range graph.Nodes {
		totalServices += len(n.Services)
		totalServices += len(n.Children) // each fork child is a serve
	}
	t.Logf("nodes: %d, edges: %d, services (inbound+outbound): %d, fork children: counted in services",
		len(graph.Nodes), len(graph.Edges), totalServices)

	// ====== Generate Mermaid ======
	mermaid := generateMermaid(graph)
	t.Logf("=== MERMAID DIAGRAM ===\n%s", mermaid)

	// Cleanup
	Agents.Range(func(key, _ interface{}) bool {
		Agents.Delete(key)
		return true
	})
}

func generateMermaid(graph *TopologyGraph) string {
	var sb strings.Builder
	sb.WriteString("graph TD\n")

	// Node definitions with subgraph-like styling
	for _, n := range graph.Nodes {
		label := n.ID
		if n.Hostname != "" {
			label += fmt.Sprintf("\\n%s", n.Hostname)
		}
		if len(n.Interfaces) > 0 {
			label += fmt.Sprintf("\\n%s", n.Interfaces[0])
		}

		// Services summary
		var svcParts []string
		for _, svc := range n.Services {
			s := svc.Protocol
			if svc.Address != "" {
				s += " " + svc.Address
			}
			svcParts = append(svcParts, fmt.Sprintf("%s:%s", svc.Role, s))
		}
		if len(n.Children) > 0 {
			svcParts = append(svcParts, fmt.Sprintf("forks:%s", strings.Join(n.Children, ",")))
		}
		if len(svcParts) > 0 {
			label += fmt.Sprintf("\\n[%s]", strings.Join(svcParts, " | "))
		}
		if n.Bridges > 0 {
			label += fmt.Sprintf("\\nbridges:%d", n.Bridges)
		}

		// Shape by type
		id := sanitizeMermaidID(n.ID)
		switch n.Type {
		case "server":
			sb.WriteString(fmt.Sprintf("    %s[[\"%s\"]]\n", id, label))
		case "relay":
			sb.WriteString(fmt.Sprintf("    %s[\"%s\"]\n", id, label))
		default:
			sb.WriteString(fmt.Sprintf("    %s(\"%s\")\n", id, label))
		}
	}

	sb.WriteString("\n")

	// Edges
	for _, e := range graph.Edges {
		from := sanitizeMermaidID(e.From)
		to := sanitizeMermaidID(e.To)
		switch e.Layer {
		case "tunnel":
			sb.WriteString(fmt.Sprintf("    %s -->|tunnel| %s\n", from, to))
		case "serve":
			sb.WriteString(fmt.Sprintf("    %s -.->|route -d| %s\n", from, to))
		}
	}

	// Styling
	sb.WriteString("\n")
	for _, n := range graph.Nodes {
		id := sanitizeMermaidID(n.ID)
		switch n.Type {
		case "server":
			sb.WriteString(fmt.Sprintf("    style %s fill:#e74c3c,color:#fff\n", id))
		case "relay":
			sb.WriteString(fmt.Sprintf("    style %s fill:#3498db,color:#fff\n", id))
		default:
			sb.WriteString(fmt.Sprintf("    style %s fill:#2ecc71,color:#fff\n", id))
		}
	}

	return sb.String()
}

func sanitizeMermaidID(id string) string {
	r := strings.NewReplacer("-", "_", ":", "_", ".", "_", " ", "_")
	return r.Replace(id)
}
