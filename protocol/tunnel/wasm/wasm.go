//go:build js && wasm

package wasm

import (
	"context"
	"crypto/tls"
	"fmt"
	"net"
	"net/http"

	"github.com/chainreactors/rem/protocol/core"
	"github.com/chainreactors/rem/x/wasmws"
	"golang.org/x/net/websocket"
)

func init() {
	core.DialerRegister(core.WASMTunnel, func(ctx context.Context) (core.TunnelDialer, error) {
		return NewWASMDialer(ctx), nil
	})

	core.ListenerRegister(core.WASMTunnel, func(ctx context.Context) (core.TunnelListener, error) {
		return NewWASMListener(ctx), nil
	})
}

type WASMDialer struct {
	net.Conn
	meta core.Metas
}

func NewWASMDialer(ctx context.Context) *WASMDialer {
	return &WASMDialer{
		meta: core.GetMetas(ctx),
	}
}

func (c *WASMDialer) Dial(dst string) (net.Conn, error) {
	u, err := core.NewURL(dst)
	if err != nil {
		return nil, err
	}
	c.meta["url"] = u

	scheme := "ws"
	if u.Scheme == "https" || u.RawScheme == "wss" {
		scheme = "wss"
	}

	var wsURL string
	if u.PathString() != "" {
		wsURL = fmt.Sprintf("%s://%s/%s", scheme, u.Host, u.PathString())
	} else {
		wsURL = fmt.Sprintf("%s://%s/", scheme, u.Host)
	}

	conn, err := wasmws.DialContext(context.Background(), "websocket", wsURL)
	if err != nil {
		return nil, err
	}

	return conn, nil
}

// WASMListener 使用与WebSocket相同的实现模式
type WASMListener struct {
	acceptCh chan *websocket.Conn
	server   *http.Server
	listener net.Listener
	meta     core.Metas
}

func NewWASMListener(ctx context.Context) *WASMListener {
	return &WASMListener{
		meta: core.GetMetas(ctx),
	}
}

func (c *WASMListener) Listen(dst string) (net.Listener, error) {
	u, err := core.NewURL(dst)
	if err != nil {
		return nil, err
	}
	c.meta["url"] = u
	if u.PathString() == "" {
		u.Path = "/" + c.meta.GetString("path")
	}
	ln, err := net.Listen("tcp", u.Host)
	if err != nil {
		return nil, err
	}
	c.listener = ln

	c.acceptCh = make(chan *websocket.Conn)
	muxer := http.NewServeMux()
	muxer.Handle(u.Path, websocket.Handler(func(conn *websocket.Conn) {
		conn.PayloadType = websocket.BinaryFrame
		notifyCh := make(chan struct{})
		c.acceptCh <- conn
		<-notifyCh
	}))

	c.server = &http.Server{
		Handler: muxer,
		TLSConfig: &tls.Config{
			InsecureSkipVerify: true,
		},
	}
	go c.server.Serve(ln)

	return c, nil
}

func (c *WASMListener) Accept() (net.Conn, error) {
	conn, ok := <-c.acceptCh
	if !ok {
		return nil, fmt.Errorf("websocket error")
	}
	return conn, nil
}

func (c *WASMListener) Close() error {
	if c.server != nil {
		return c.server.Close()
	}
	return nil
}

func (c *WASMListener) Addr() net.Addr {
	return c.meta.URL()
}
