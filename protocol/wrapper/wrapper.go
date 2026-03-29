package wrapper

import (
	"bufio"
	"github.com/chainreactors/rem/protocol/core"
	"net"
)

// WrapperChain 实现了wrapper链式调用
type WrapperChain struct {
	wrappers []core.Wrapper
	conn     net.Conn
	reader   *ChainReader
}

type ChainReader struct {
	wrapper core.Wrapper
	next    *ChainReader
	reader  *bufio.Reader
}

func NewChainReader(w core.Wrapper) *ChainReader {
	return &ChainReader{
		wrapper: w,
		reader:  bufio.NewReader(w),
	}
}

func (r *ChainReader) Chain(next *ChainReader) {
	r.next = next
}

func (r *ChainReader) Read(p []byte) (n int, err error) {
	if r.next != nil {
		return r.next.Read(p)
	}
	return r.wrapper.Read(p)
}

// NewChainWrapper 创建一个wrapper链
func NewChainWrapper(conn net.Conn, opts []*core.WrapperOption) (core.Wrapper, error) {
	chain := &WrapperChain{
		conn:     conn,
		wrappers: make([]core.Wrapper, 0, len(opts)),
	}

	// 创建写入链
	var current core.Wrapper = nil
	for _, opt := range opts {
		var w core.Wrapper
		var err error
		if current == nil {
			w, err = core.WrapperCreate(opt.Name, conn, conn, opt.Options)
		} else {
			w, err = core.WrapperCreate(opt.Name, current, current, opt.Options)
		}
		if err != nil {
			return nil, err
		}
		chain.wrappers = append(chain.wrappers, w)
		current = w
	}

	// 创建读取链
	var reader *ChainReader
	for i := len(chain.wrappers) - 1; i >= 0; i-- {
		w := chain.wrappers[i]
		newReader := NewChainReader(w)
		if reader != nil {
			newReader.Chain(reader)
		}
		reader = newReader
	}
	chain.reader = reader

	return chain, nil
}

func (c *WrapperChain) Name() string {
	return "chain"
}

// Read 从最后一个wrapper开始读，保持解密顺序
func (c *WrapperChain) Read(p []byte) (n int, err error) {
	if c.reader == nil {
		return c.conn.Read(p)
	}
	return c.reader.Read(p)
}

// Write 从第一个wrapper开始写，保持加密顺序
func (c *WrapperChain) Write(p []byte) (n int, err error) {
	if len(c.wrappers) == 0 {
		return c.conn.Write(p)
	}
	return c.wrappers[len(c.wrappers)-1].Write(p)
}

// Close 关闭所有wrapper
func (c *WrapperChain) Close() error {
	for _, w := range c.wrappers {
		if err := w.Close(); err != nil {
			return err
		}
	}
	return c.conn.Close()
}
