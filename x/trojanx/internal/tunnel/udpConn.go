package tunnel

import (
	"github.com/chainreactors/rem/x/trojanx/internal/common"
	"net"
)

type UDPConn struct {
	*net.UDPConn
}

func (c *UDPConn) WriteWithMetadata(p []byte, adder *common.Address) (int, error) {
	return c.WriteTo(p, adder)
}

func (c *UDPConn) ReadWithMetadata(p []byte) (int, *common.Address, error) {
	n, addr, err := c.ReadFrom(p)
	if err != nil {
		return 0, nil, err
	}
	address, err := common.NewAddressFromAddr("udp", addr.String())
	return n, address, nil
}

func (c *UDPConn) WriteTo(p []byte, addr net.Addr) (int, error) {
	if udpAddr, ok := addr.(*net.UDPAddr); ok {
		return c.WriteToUDP(p, udpAddr)
	}
	ip, err := addr.(*common.Address).ResolveIP()
	if err != nil {
		return 0, err
	}
	udpAddr := &net.UDPAddr{
		IP:   ip,
		Port: addr.(*common.Address).Port,
	}
	return c.WriteToUDP(p, udpAddr)
}
