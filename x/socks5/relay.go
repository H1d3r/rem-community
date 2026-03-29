package socks5

import (
	"bytes"
	"io"
	"net"
	"strconv"
)

func NewRelay(s string) (*RelayRequest, error) {
	host, port, err := net.SplitHostPort(s)
	if err != nil {
		return nil, err
	}
	portS, err := strconv.Atoi(port)
	if err != nil {
		return nil, err
	}
	ip := net.ParseIP(host)
	var req *RelayRequest
	if ip != nil {
		req = &RelayRequest{&AddrSpec{
			IP:   ip,
			Port: portS,
		}}
	} else {
		req = &RelayRequest{&AddrSpec{
			FQDN: host,
			Port: portS,
		}}
	}
	return req, nil
}

type RelayRequest struct {
	// AddrSpec of the desired destination
	DestAddr *AddrSpec
}

func (req *RelayRequest) String() string {
	return req.DestAddr.String()
}

func (req *RelayRequest) BuildRelay() []byte {
	buf := &bytes.Buffer{}

	buf.Write([]byte{
		0x05, // VER
		0x01, // NMETHODS
		0x03, // METHOD (Relay Auth)
	})
	buf.Write([]byte{
		0x05, // VER
		0x01, // CMD
		0x00, // RSV
	})

	if req.DestAddr.IP != nil {
		if ip4 := req.DestAddr.IP.To4(); ip4 != nil {
			buf.WriteByte(0x01) // IPv4
			buf.Write(ip4)
		} else {
			buf.WriteByte(0x04) // IPv6
			buf.Write(req.DestAddr.IP)
		}
	} else {
		buf.WriteByte(0x03) // Domain
		buf.WriteByte(byte(len(req.DestAddr.FQDN)))
		buf.WriteString(req.DestAddr.FQDN)
	}

	buf.Write([]byte{
		byte(req.DestAddr.Port >> 8),
		byte(req.DestAddr.Port),
	})

	return buf.Bytes()
}

func (req *RelayRequest) HandleRelay(conn net.Conn, rwc io.ReadWriteCloser) error {
	_, err := rwc.Write(req.BuildRelay())
	if err != nil {
		return err
	}

	_, addr, err := ReadReply(rwc)
	if err != nil {
		return err
	}

	replyData, err := BuildReply(SuccessReply, addr)
	if err != nil {
		return err
	}
	_, err = conn.Write(replyData)
	if err != nil {
		return err
	}
	return nil
}
