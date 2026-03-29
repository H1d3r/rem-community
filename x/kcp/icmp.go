package kcp

import (
	"encoding/binary"
	"net"
	"sync"
	"sync/atomic"

	"golang.org/x/net/icmp"
	"golang.org/x/net/ipv4"
)

type remoteInfo struct {
	addr net.Addr
	id   int
}

type icmpConn struct {
	*icmp.PacketConn
	id       int // convid作为ICMP ID
	port     int // port作为token
	seq      int32
	isClient bool

	// 连接池相关
	remotes sync.Map // key: addr.String(), value: *remoteInfo
}

func (c *icmpConn) getOrCreateRemote(addr net.Addr, id int) *remoteInfo {
	key := addr.String()
	if info, ok := c.remotes.Load(key); ok {
		return info.(*remoteInfo)
	}

	info := &remoteInfo{
		addr: addr,
		id:   id,
	}
	c.remotes.Store(key, info)
	return info
}

func (c *icmpConn) WriteTo(p []byte, addr net.Addr) (n int, err error) {
	seq := atomic.AddInt32(&c.seq, 1)
	msgType := ipv4.ICMPTypeEcho

	// 在数据前加入port作为token
	data := make([]byte, 2+len(p))
	binary.BigEndian.PutUint16(data[:2], uint16(c.port))
	copy(data[2:], p)

	var remoteID int
	if !c.isClient {
		msgType = ipv4.ICMPTypeEchoReply
		// 从连接池获取对应的remote信息
		if info, ok := c.remotes.Load(addr.String()); ok {
			remoteID = info.(*remoteInfo).id
		} else {
			return 0, nil
		}
	} else {
		remoteID = c.id
	}

	m := &icmp.Message{
		Type: msgType,
		Code: 0,
		Body: &icmp.Echo{
			ID:   remoteID,
			Seq:  int(seq),
			Data: data,
		},
	}

	wb, err := m.Marshal(nil)
	if err != nil {
		return 0, err
	}

	n, err = c.PacketConn.WriteTo(wb, addr)
	if err != nil {
		return 0, err
	}

	return len(p), nil
}

func (c *icmpConn) ReadFrom(p []byte) (n int, addr net.Addr, err error) {
	for {
		n, addr, err = c.PacketConn.ReadFrom(p)
		if err != nil {
			return
		}

		msg, err := icmp.ParseMessage(1, p[:n])
		if err != nil {
			continue
		}

		if echo, ok := msg.Body.(*icmp.Echo); ok {
			if len(echo.Data) < 2 {
				continue
			}

			// 验证port token
			recvPort := int(binary.BigEndian.Uint16(echo.Data[:2]))
			if recvPort != c.port {
				continue
			}

			if !c.isClient {
				// 服务端收到新连接，记录到连接池
				c.getOrCreateRemote(addr, echo.ID)
			}

			// 返回数据时去除port token
			n = copy(p, echo.Data[2:])
			return n, addr, nil
		}
	}
}

func (c *icmpConn) LocalAddr() net.Addr {
	return c.PacketConn.LocalAddr()
}
