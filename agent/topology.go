package agent

import (
	"encoding/json"
	"fmt"
	"sort"
	"sync/atomic"
)

// ServiceInfo describes one inbound or outbound service on an agent.
type ServiceInfo struct {
	Role     string `json:"role"`               // "inbound" or "outbound"
	Protocol string `json:"protocol,omitempty"`  // e.g. "socks5", "raw", "port"
	Address  string `json:"address,omitempty"`   // listen/target address
}

// TopologyNode represents a node (agent) in the network.
type TopologyNode struct {
	ID          string        `json:"id"`
	Type        string        `json:"type"`                  // "server", "relay", "client", "redirect"
	Hostname    string        `json:"hostname,omitempty"`
	Username    string        `json:"username,omitempty"`
	Interfaces  []string      `json:"interfaces,omitempty"`
	Via         string        `json:"via,omitempty"`          // tunnel layer: upstream relay ID
	InboundSide string        `json:"inbound_side,omitempty"` // serve layer: "local", "remote", or ""
	Destination string        `json:"destination,omitempty"`  // serve layer: -d alias routing target
	Local       string        `json:"local,omitempty"`        // local URL string
	Remote      string        `json:"remote,omitempty"`       // remote URL string
	Services    []ServiceInfo `json:"services,omitempty"`     // active inbound/outbound
	Children    []string      `json:"children,omitempty"`     // forked child agent IDs
	Bridges     int64         `json:"bridges"`                // active bridge count
	BytesIn     int64         `json:"bytes_in"`               // total bytes received
	BytesOut    int64         `json:"bytes_out"`              // total bytes sent
}

// TopologyEdge represents a connection between two nodes.
type TopologyEdge struct {
	From  string `json:"from"`
	To    string `json:"to"`
	Layer string `json:"layer"` // "tunnel" (conn relay) or "serve" (alias routing via -d)
}

// TopologyGraph is the complete network topology.
type TopologyGraph struct {
	Nodes []TopologyNode `json:"nodes"`
	Edges []TopologyEdge `json:"edges"`
}

// BuildTopologyGraph constructs the full network graph from the global Agents map.
// Forked child agents are merged into their parent node as additional services.
func BuildTopologyGraph(serverID string) *TopologyGraph {
	graph := &TopologyGraph{
		Nodes: []TopologyNode{{ID: serverID, Type: "server"}},
	}

	viaSet := make(map[string]bool)
	nodeIndex := make(map[string]int) // agent ID → index in graph.Nodes

	// First pass: add non-forked agents as tunnel nodes
	Agents.Range(func(_, v interface{}) bool {
		a := v.(*Agent)
		if a.parent != nil {
			return true // skip forked children in first pass
		}
		node := buildNode(a)
		nodeIndex[a.ID] = len(graph.Nodes)
		graph.Nodes = append(graph.Nodes, node)

		parent := serverID
		if a.Via != "" {
			parent = a.Via
			viaSet[a.Via] = true
		}
		graph.Edges = append(graph.Edges, TopologyEdge{From: parent, To: a.ID, Layer: "tunnel"})

		if a.Redirect != "" && a.Redirect != a.ID {
			graph.Edges = append(graph.Edges, TopologyEdge{From: a.ID, To: a.Redirect, Layer: "serve"})
		}
		return true
	})

	// Second pass: merge forked children's services into parent nodes
	Agents.Range(func(_, v interface{}) bool {
		a := v.(*Agent)
		if a.parent == nil {
			return true // skip non-forked in second pass
		}
		childNode := buildNode(a)
		// Find parent in graph
		if idx, ok := nodeIndex[a.parent.ID]; ok {
			// Merge services
			graph.Nodes[idx].Services = append(graph.Nodes[idx].Services, childNode.Services...)
			// Merge children list
			graph.Nodes[idx].Children = append(graph.Nodes[idx].Children, a.ID)
			// Accumulate traffic
			graph.Nodes[idx].BytesIn += childNode.BytesIn
			graph.Nodes[idx].BytesOut += childNode.BytesOut
			graph.Nodes[idx].Bridges += childNode.Bridges
		}
		return true
	})

	// Mark relay and client nodes
	for i := range graph.Nodes {
		if graph.Nodes[i].ID == serverID {
			continue // keep server as-is
		}
		if viaSet[graph.Nodes[i].ID] {
			graph.Nodes[i].Type = "relay"
		} else if graph.Nodes[i].Type != "redirect" {
			graph.Nodes[i].Type = "client"
		}
	}

	// Stable sort
	sort.Slice(graph.Nodes, func(i, j int) bool {
		return graph.Nodes[i].ID < graph.Nodes[j].ID
	})
	sort.Slice(graph.Edges, func(i, j int) bool {
		if graph.Edges[i].From != graph.Edges[j].From {
			return graph.Edges[i].From < graph.Edges[j].From
		}
		if graph.Edges[i].To != graph.Edges[j].To {
			return graph.Edges[i].To < graph.Edges[j].To
		}
		return graph.Edges[i].Layer < graph.Edges[j].Layer
	})

	return graph
}

func buildNode(a *Agent) TopologyNode {
	node := TopologyNode{
		ID:         a.ID,
		Type:       a.Type,
		Hostname:   a.Hostname,
		Username:   a.Username,
		Interfaces: a.Interfaces,
		Via:        a.Via,
		InboundSide: a.InboundSide,
		Bridges:    atomic.LoadInt64(&a.connCount),
	}

	// Traffic stats
	bIn, bOut, _, _ := a.TrafficStats()
	node.BytesIn = bIn
	node.BytesOut = bOut

	if a.Redirect != "" && a.Redirect != a.ID {
		node.Destination = a.Redirect
	}

	if a.URLs != nil {
		if a.LocalURL != nil && a.LocalURL.String() != "" {
			node.Local = a.LocalURL.String()
		}
		if a.RemoteURL != nil && a.RemoteURL.String() != "" {
			node.Remote = a.RemoteURL.String()
		}
	}

	// Inbound service
	if a.Inbound != nil {
		svc := ServiceInfo{Role: "inbound", Protocol: a.Inbound.Name()}
		if p := a.Inbound.ToClash(); p != nil {
			if p.Port != 0 {
				svc.Address = fmt.Sprintf("%s:%d", p.Server, p.Port)
			}
		}
		node.Services = append(node.Services, svc)
	}

	// Outbound service
	if a.Outbound != nil {
		svc := ServiceInfo{Role: "outbound", Protocol: a.Outbound.Name()}
		if p := a.Outbound.ToClash(); p != nil {
			if p.Port != 0 {
				svc.Address = fmt.Sprintf("%s:%d", p.Server, p.Port)
			}
		}
		node.Services = append(node.Services, svc)
	}

	// Forked children
	a.children.Range(func(key, _ interface{}) bool {
		node.Children = append(node.Children, key.(string))
		return true
	})
	if len(node.Children) > 1 {
		sort.Strings(node.Children)
	}

	return node
}

// TopologyJSON returns the topology graph as a JSON string.
func TopologyJSON(serverID string) string {
	graph := BuildTopologyGraph(serverID)
	b, err := json.Marshal(graph)
	if err != nil {
		return "{}"
	}
	return string(b)
}
