package simplex

import (
	"context"
	"fmt"
	"io"
	"sync"
	"time"
)

// PeekableChannel 支持Peek操作的Channel
type PeekableChannel struct {
	ch     chan *SimplexPacket
	front  []*SimplexPacket // front queue used by Peek() and rollback-on-send-failure
	closed bool
	mu     sync.Mutex
}

func NewPeekableChannel(size int) *PeekableChannel {
	return &PeekableChannel{
		ch: make(chan *SimplexPacket, size),
	}
}

func (pc *PeekableChannel) Put(packet *SimplexPacket) error {
	pc.mu.Lock()
	defer pc.mu.Unlock()

	if pc.closed {
		return io.ErrClosedPipe
	}

	select {
	case pc.ch <- packet:
		return nil
	default:
		return fmt.Errorf("channel full")
	}
}

func (pc *PeekableChannel) TryPut(packet *SimplexPacket) (bool, error) {
	pc.mu.Lock()
	defer pc.mu.Unlock()

	if pc.closed {
		return false, io.ErrClosedPipe
	}

	select {
	case pc.ch <- packet:
		return true, nil
	default:
		return false, nil
	}
}

func (pc *PeekableChannel) PutFront(packet *SimplexPacket) error {
	if packet == nil {
		return nil
	}

	pc.mu.Lock()
	defer pc.mu.Unlock()

	if pc.closed {
		return io.ErrClosedPipe
	}

	pc.front = append(pc.front, nil)
	copy(pc.front[1:], pc.front[:len(pc.front)-1])
	pc.front[0] = packet
	return nil
}

// PutWait applies backpressure instead of dropping the packet when the channel
// is temporarily full. It exits when the packet is queued, the channel closes,
// or ctx is cancelled.
func (pc *PeekableChannel) PutWait(ctx context.Context, packet *SimplexPacket) error {
	if packet == nil {
		return nil
	}
	for {
		pc.mu.Lock()
		if pc.closed {
			pc.mu.Unlock()
			return io.ErrClosedPipe
		}
		select {
		case pc.ch <- packet:
			pc.mu.Unlock()
			return nil
		default:
		}
		pc.mu.Unlock()

		if ctx != nil {
			select {
			case <-ctx.Done():
				return ctx.Err()
			default:
			}
		}
		time.Sleep(simplexQueueBackpressurePollInterval)
	}
}

func (pc *PeekableChannel) Get() (*SimplexPacket, error) {
	pc.mu.Lock()
	defer pc.mu.Unlock()

	if pc.closed {
		return nil, io.ErrClosedPipe
	}

	if len(pc.front) > 0 {
		packet := pc.front[0]
		pc.front = pc.front[1:]
		return packet, nil
	}

	select {
	case packet := <-pc.ch:
		return packet, nil
	default:
		return nil, nil
	}
}

func (pc *PeekableChannel) Peek() (*SimplexPacket, error) {
	pc.mu.Lock()
	defer pc.mu.Unlock()

	if pc.closed {
		return nil, io.ErrClosedPipe
	}

	if len(pc.front) > 0 {
		return pc.front[0], nil
	}

	select {
	case packet := <-pc.ch:
		pc.front = append(pc.front, packet)
		return packet, nil
	default:
		return nil, nil
	}
}

func (pc *PeekableChannel) Close() error {
	pc.mu.Lock()
	defer pc.mu.Unlock()

	if !pc.closed {
		pc.closed = true
		close(pc.ch)
	}
	return nil
}

func (pc *PeekableChannel) Size() int {
	pc.mu.Lock()
	defer pc.mu.Unlock()

	return len(pc.ch) + len(pc.front)
}
