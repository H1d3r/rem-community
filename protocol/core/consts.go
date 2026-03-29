package core

var (
	MaxPacketSize       = 1024 * 128
	MaxBufferSize int64 = int64(MaxPacketSize * 8)
	DefaultKey          = "nonenonenonenone"

	DefaultScheme       = "default"
	DefaultConsoleProto = "tcp"
	DefaultUsername     = "remno1"
	DefaultPassword     = "0onmer"

	LOCALHOST = "127.0.0.1"
)

const (
	InboundPlugin  = "inbound"
	OutboundPlugin = "outbound"
)

// proto
const (
	RawServe = "raw"
	//RelayServe = "relay" // custom socks5 protocol
	Socks5Serve       = "socks5"
	HTTPServe         = "http"
	ShadowSocksServe  = "ss"
	TrojanServe       = "trojan"
	PortForwardServe  = "forward"
	CobaltStrikeServe = "cs"
	FetchServe        = "fetch"
)

// transport
const (
	ICMPTunnel        = "icmp"
	HTTPTunnel        = "http"
	HTTP2Tunnel       = "http2"
	StreamHTTPTunnel  = "streamhttp"
	StreamHTTPSTunnel = "streamhttps"
	UDPTunnel         = "udp"
	TCPTunnel         = "tcp"
	UNIXTunnel        = "unix"
	WebSocketTunnel   = "ws"
	WireGuardTunnel   = "wireguard"
	MemoryTunnel      = "memory"
	WASMTunnel        = "wasm"
	DNSTunnel         = "dns"
	SimplexTunnel     = "simplex"
	OSSTunnel         = "oss"
)

func DefaultTunnelPort(s string) string {
	switch s {
	case StreamHTTPTunnel, HTTP2Tunnel:
		return "80"
	case WebSocketTunnel, OSSTunnel, HTTPTunnel, StreamHTTPSTunnel, "http2s":
		return "443"
	default:
		return "34996"
	}
}

func NormalizeServe(s string) string {
	switch s {
	case "socks5", "s5", "socks":
		return Socks5Serve
	case "ss", "shadowsocks":
		return ShadowSocksServe
	case "trojan":
		return TrojanServe
	case "forward", "port", "pf", "portfoward":
		return PortForwardServe
	case "http", "https":
		return HTTPServe
	case "raw":
		return RawServe
	case "smb", "pipe", "unix", "sock":
		return UNIXTunnel
	case "ws", "websocket", "wss":
		return WebSocketTunnel
	case "wireguard", "wg":
		return WireGuardTunnel
	case "wasm":
		return WASMTunnel
	case "fetch", "fetchproxy", "fp":
		return FetchServe
	default:
		return s
	}
}

// wrapper
const (
	CryptorWrapper = "cryptor"
	PaddingWrapper = "padding"
)

// InboundSide indicates which side of the tunnel runs the inbound listener.
const (
	SideLocal  = "local"  // inbound on local (client) side
	SideRemote = "remote" // inbound on remote (server) side
)

// duplex channel direction
const (
	DirUp   = "up"
	DirDown = "down"
)

// agent type
const (
	SERVER = "server"

	CLIENT = "client"

	Redirect = "redirect"
)
