package tunnel

import (
	"bytes"
	"encoding/binary"
	"github.com/chainreactors/rem/x/trojanx/internal/common"
	"github.com/pkg/errors"
	"io"
	"io/ioutil"
	"net"
)

type TrojanConn struct {
	net.Conn
}

func (t *TrojanConn) ReadFrom(payload []byte) (int, net.Addr, error) {
	return t.ReadWithMetadata(payload)
}

func (t *TrojanConn) WriteTo(payload []byte, addr net.Addr) (int, error) {
	address, err := common.NewAddressFromAddr("udp", addr.String())
	if err != nil {
		return 0, err
	}
	return t.WriteWithMetadata(payload, address)
}

func (t *TrojanConn) WriteWithMetadata(payload []byte, addr *common.Address) (int, error) {
	packet := make([]byte, 0, common.MaxPacketSize)
	w := bytes.NewBuffer(packet)
	addr.WriteTo(w)
	length := len(payload)
	lengthBuf := [2]byte{}
	crlf := [2]byte{0x0d, 0x0a}

	binary.BigEndian.PutUint16(lengthBuf[:], uint16(length))
	w.Write(lengthBuf[:])
	w.Write(crlf[:])
	w.Write(payload)

	_, err := t.Conn.Write(w.Bytes())

	return len(payload), err
}

func (t *TrojanConn) ReadWithMetadata(payload []byte) (int, *common.Address, error) {
	addr := &common.Address{
		NetworkType: "udp",
	}
	if err := addr.ReadFrom(t.Conn); err != nil {
		return 0, nil, errors.New("failed to parse udp packet addr")
	}
	lengthBuf := [2]byte{}
	if _, err := io.ReadFull(t.Conn, lengthBuf[:]); err != nil {
		return 0, nil, errors.New("failed to read length")
	}
	length := int(binary.BigEndian.Uint16(lengthBuf[:]))

	crlf := [2]byte{}
	if _, err := io.ReadFull(t.Conn, crlf[:]); err != nil {
		return 0, nil, errors.New("failed to read crlf")
	}

	if len(payload) < length || length > common.MaxPacketSize {
		io.CopyN(ioutil.Discard, t.Conn, int64(length))
		return 0, nil, errors.New("incoming packet size is too large")
	}

	if _, err := io.ReadFull(t.Conn, payload[:length]); err != nil {
		return 0, nil, errors.New("failed to read payload")
	}

	return length, addr, nil
}
