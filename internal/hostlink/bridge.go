package hostlink

import (
	"net"
	"sync"
)

// MessageChannel is what a Bridge needs of a WebRTC data channel.
type MessageChannel interface {
	Send([]byte) error
	BufferedAmount() uint64
	SetBufferedAmountLowThreshold(uint64)
	OnBufferedAmountLow(func())
	Close() error
}

const (
	// bridgeChunk is the largest message sent; bridgeHighWater pauses
	// reading the connection while that much waits to go out.
	bridgeChunk     = 16 << 10
	bridgeHighWater = 1 << 20
	// bridgeQueueMax bounds what a client may send ahead of the computer.
	// SSH's window keeps it far below that; a client past it is dropped.
	bridgeQueueMax = 16 << 20
)

// Bridge copies between a data channel and a TCP connection, with
// backpressure both ways: the connection's reads pause while the channel's
// buffer is full, and the channel's messages queue (bounded) until the
// connection takes them.
type Bridge struct {
	d MessageChannel

	mu      sync.Mutex
	queue   [][]byte
	queued  int
	closed  bool
	conn    net.Conn
	wake    chan struct{}
	drained chan struct{}
}

// NewBridge prepares a bridge for d; register FromChannel and Close as d's
// message and close handlers, then call Run with the connection.
func NewBridge(d MessageChannel) *Bridge {
	b := &Bridge{d: d, wake: make(chan struct{}, 1), drained: make(chan struct{}, 1)}
	d.SetBufferedAmountLowThreshold(bridgeHighWater / 2)
	d.OnBufferedAmountLow(func() { signal(b.drained) })
	return b
}

func signal(ch chan struct{}) {
	select {
	case ch <- struct{}{}:
	default:
	}
}

// FromChannel queues a message for the connection.
func (b *Bridge) FromChannel(data []byte) {
	b.mu.Lock()
	if b.closed {
		b.mu.Unlock()
		return
	}
	if b.queued+len(data) > bridgeQueueMax {
		b.mu.Unlock()
		b.Close()
		_ = b.d.Close()
		return
	}
	b.queue = append(b.queue, append([]byte(nil), data...))
	b.queued += len(data)
	b.mu.Unlock()
	signal(b.wake)
}

// Close ends the bridge (the channel closed), and its connection: a write
// to a computer that stopped answering must not keep it alive.
func (b *Bridge) Close() {
	b.mu.Lock()
	b.closed = true
	b.queue, b.queued = nil, 0
	conn := b.conn
	b.mu.Unlock()
	if conn != nil {
		_ = conn.Close()
	}
	signal(b.wake)
	signal(b.drained)
}

func (b *Bridge) isClosed() bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.closed
}

// Run copies until either side ends, then closes both.
func (b *Bridge) Run(conn net.Conn) {
	b.mu.Lock()
	b.conn = conn
	closed := b.closed
	b.mu.Unlock()
	if closed {
		_ = conn.Close()
		return
	}
	done := make(chan struct{})
	go func() {
		defer close(done)
		b.toConn(conn)
		_ = conn.Close()
	}()
	b.toChannel(conn)
	_ = conn.Close()
	b.Close()
	_ = b.d.Close()
	<-done
}

func (b *Bridge) toConn(conn net.Conn) {
	for {
		b.mu.Lock()
		queue, closed := b.queue, b.closed
		b.queue, b.queued = nil, 0
		b.mu.Unlock()
		for _, data := range queue {
			if _, err := conn.Write(data); err != nil {
				return
			}
		}
		if closed {
			return
		}
		<-b.wake
	}
}

func (b *Bridge) toChannel(conn net.Conn) {
	buf := make([]byte, bridgeChunk)
	for {
		n, err := conn.Read(buf)
		if n > 0 {
			for b.d.BufferedAmount() > bridgeHighWater && !b.isClosed() {
				<-b.drained
			}
			if b.isClosed() {
				return
			}
			if sendErr := b.d.Send(append([]byte(nil), buf[:n]...)); sendErr != nil {
				return
			}
		}
		if err != nil {
			return
		}
	}
}
