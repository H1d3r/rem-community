package udp

import (
	"context"
	"net"

	"github.com/chainreactors/rem/protocol/core"
	"github.com/chainreactors/rem/x/kcp"
)

func init() {
	core.DialerRegister(core.UDPTunnel, func(ctx context.Context) (core.TunnelDialer, error) {
		return NewUDPDialer(ctx), nil
	})
	core.ListenerRegister(core.UDPTunnel, func(ctx context.Context) (core.TunnelListener, error) {
		return NewUDPListener(ctx), nil
	})
}

type UDPDialer struct {
	net.Conn
	meta core.Metas
}

type UDPListener struct {
	listener *kcp.Listener
	meta     core.Metas
}

func NewUDPDialer(ctx context.Context) *UDPDialer {
	return &UDPDialer{
		meta: core.GetMetas(ctx),
	}
}

func NewUDPListener(ctx context.Context) *UDPListener {
	return &UDPListener{
		meta: core.GetMetas(ctx),
	}
}

func (c *UDPListener) Accept() (net.Conn, error) {
	conn, err := c.listener.AcceptKCP()
	if err != nil {
		return nil, err
	}
	return kcp.NewKCPConn(conn, kcp.RadicalKCPConfig), nil
}

func (c *UDPDialer) Dial(dst string) (net.Conn, error) {
	u, _ := core.NewURL(dst)
	c.meta["url"] = u

	conn, err := kcp.DialWithOptions("udp", u.Host, nil, 0, 0)
	if err != nil {
		return nil, err
	}

	return kcp.NewKCPConn(conn, kcp.RadicalKCPConfig), nil
}

func (c *UDPListener) Listen(dst string) (net.Listener, error) {
	u, _ := core.NewURL(dst)
	c.meta["url"] = u

	listener, err := kcp.Listen("udp", u.Host)
	if err != nil {
		return nil, err
	}

	// 设置监听器参数
	if l, ok := listener.(*kcp.Listener); ok {
		l.SetReadBuffer(core.MaxPacketSize)
		l.SetWriteBuffer(core.MaxPacketSize)
		l.SetDSCP(46)
		c.listener = l
	}

	return listener, nil
}

func (c *UDPListener) Close() error {
	if c.listener != nil {
		return c.listener.Close()
	}
	return nil
}

func (c *UDPListener) Addr() net.Addr {
	return c.meta.URL()
}
