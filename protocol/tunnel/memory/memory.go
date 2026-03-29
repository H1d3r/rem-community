package memory

import (
	"context"
	"fmt"
	"net"
	"sync"

	"github.com/chainreactors/rem/protocol/core"
)

var (
	globalBridge = NewMemoryBridge()
)

func init() {
	core.DialerRegister(core.MemoryTunnel, func(ctx context.Context) (core.TunnelDialer, error) {
		return &MemoryDialer{meta: core.GetMetas(ctx)}, nil
	})

	core.ListenerRegister(core.MemoryTunnel, func(ctx context.Context) (core.TunnelListener, error) {
		return &MemoryListener{meta: core.GetMetas(ctx)}, nil
	})
}

type MemoryBridge struct {
	connMap sync.Map // map[string]chan net.Conn
}

func NewMemoryBridge() *MemoryBridge {
	return &MemoryBridge{}
}

func (b *MemoryBridge) CreateChannel(id string) chan net.Conn {
	// Close old channel first so that any goroutine blocked on Accept()
	// wakes up and returns an error instead of silently consuming
	// connections destined for the replacement listener.
	if old, loaded := b.connMap.LoadAndDelete(id); loaded {
		close(old.(chan net.Conn))
	}
	ch := make(chan net.Conn, 1)
	b.connMap.Store(id, ch)
	return ch
}

func (b *MemoryBridge) RemoveChannel(id string) {
	if old, loaded := b.connMap.LoadAndDelete(id); loaded {
		close(old.(chan net.Conn))
	}
}

func (b *MemoryBridge) GetChannel(id string) (chan net.Conn, error) {
	ch, ok := b.connMap.Load(id)
	if ok {
		return ch.(chan net.Conn), nil
	} else {
		return nil, fmt.Errorf("not found memory listener")
	}
}

type MemoryDialer struct {
	meta core.Metas
}

func NewMemoryDialer(ctx context.Context) *MemoryDialer {
	return &MemoryDialer{meta: core.GetMetas(ctx)}
}

func NewMemoryListener(ctx context.Context) *MemoryListener {
	return &MemoryListener{meta: core.GetMetas(ctx)}
}

type MemoryListener struct {
	meta     core.Metas
	connChan chan net.Conn
	id       string // hostname key used in globalBridge
}

// Dialer implementation
func (d *MemoryDialer) Dial(dst string) (net.Conn, error) {
	u, err := core.NewURL(dst)
	if err != nil {
		return nil, err
	}
	d.meta["url"] = u

	client, server := net.Pipe()
	ch, err := globalBridge.GetChannel(u.Hostname())
	if err != nil {
		client.Close()
		server.Close()
		return nil, err
	}

	// Protect against sending on a channel that was closed by
	// MemoryListener.Close() or CreateChannel() between GetChannel
	// and this send (race with agent.Close()).
	sent := false
	func() {
		defer func() {
			if r := recover(); r != nil {
				// Channel was closed between GetChannel and send
			}
		}()
		ch <- server
		sent = true
	}()

	if !sent {
		client.Close()
		server.Close()
		return nil, fmt.Errorf("memory listener closed")
	}
	return client, nil
}

// Listener implementation
func (l *MemoryListener) Listen(dst string) (net.Listener, error) {
	u, err := core.NewURL(dst)
	if err != nil {
		return nil, err
	}
	l.meta["url"] = u
	l.id = u.Hostname()
	l.connChan = globalBridge.CreateChannel(l.id)
	return l, nil
}

func (l *MemoryListener) Accept() (net.Conn, error) {
	conn, ok := <-l.connChan
	if !ok {
		return nil, fmt.Errorf("memory listener closed")
	}
	return conn, nil
}

// TryAccept attempts a non-blocking accept. Returns (conn, nil, true) if a
// connection was available, or (nil, nil, false) if none was pending.
// Used by the TinyGo WASM polling loop where goroutine-based Accept blocks forever.
func (l *MemoryListener) TryAccept() (net.Conn, error, bool) {
	select {
	case conn := <-l.connChan:
		return conn, nil, true
	default:
		return nil, nil, false
	}
}

func (l *MemoryListener) Close() error {
	globalBridge.RemoveChannel(l.id)
	return nil
}

func (l *MemoryListener) Addr() net.Addr {
	return l.meta.URL()
}
