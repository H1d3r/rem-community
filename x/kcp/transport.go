package kcp

import (
	"net"
)

// PacketConn is the basic interface for packet-based connections
type PacketConn interface {
	net.PacketConn
}
