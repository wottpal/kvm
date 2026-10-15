package hostlink

import (
	"bytes"
	"io"
	"net"
	"sync"
	"testing"
	"time"
)

// fakeChannel records what the bridge sends and lets the test drain its
// buffer by hand.
type fakeChannel struct {
	mu       sync.Mutex
	sent     bytes.Buffer
	buffered uint64
	low      func()
	closed   bool
}

func (f *fakeChannel) Send(p []byte) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.sent.Write(p)
	f.buffered += uint64(len(p))
	return nil
}

func (f *fakeChannel) sentBytes() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.sent.Len()
}

func (f *fakeChannel) BufferedAmount() uint64 {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.buffered
}

func (f *fakeChannel) SetBufferedAmountLowThreshold(uint64) {}

func (f *fakeChannel) OnBufferedAmountLow(fn func()) { f.low = fn }

func (f *fakeChannel) Close() error {
	f.mu.Lock()
	f.closed = true
	f.mu.Unlock()
	return nil
}

func (f *fakeChannel) drain() {
	f.mu.Lock()
	f.buffered = 0
	f.mu.Unlock()
	f.low()
}

func (f *fakeChannel) isClosed() bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.closed
}

func TestBridgeQueuesUntilTheConnectionIsUp(t *testing.T) {
	ch := &fakeChannel{}
	b := NewBridge(ch)
	b.FromChannel([]byte("SSH-2.0-client\r\n")) // before Run, as a fast client does
	device, computer := net.Pipe()
	done := make(chan struct{})
	go func() { b.Run(device); close(done) }()

	got := make([]byte, 16)
	if _, err := io.ReadFull(computer, got); err != nil || string(got) != "SSH-2.0-client\r\n" {
		t.Fatalf("computer got %q, %v", got, err)
	}
	if _, err := computer.Write([]byte("SSH-2.0-server\r\n")); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(2 * time.Second)
	for {
		ch.mu.Lock()
		s := ch.sent.String()
		ch.mu.Unlock()
		if s == "SSH-2.0-server\r\n" {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("channel got %q", s)
		}
		time.Sleep(5 * time.Millisecond)
	}

	// The computer hanging up closes the channel.
	computer.Close()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("Run didn't return")
	}
	if !ch.isClosed() {
		t.Fatal("channel left open")
	}
}

func TestBridgeClosesTheConnectionWhenTheChannelCloses(t *testing.T) {
	ch := &fakeChannel{}
	b := NewBridge(ch)
	device, computer := net.Pipe()
	done := make(chan struct{})
	go func() { b.Run(device); close(done) }()
	b.Close()
	if _, err := computer.Read(make([]byte, 1)); err == nil {
		t.Fatal("connection still open")
	}
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("Run didn't return")
	}
}

func TestBridgePausesWhileTheChannelIsFull(t *testing.T) {
	ch := &fakeChannel{}
	b := NewBridge(ch)
	device, computer := net.Pipe()
	go b.Run(device)
	go func() { _, _ = computer.Write(make([]byte, 4*bridgeHighWater)) }()

	// Sends stop once more than the high-water mark is buffered...
	deadline := time.Now().Add(2 * time.Second)
	for ch.BufferedAmount() <= bridgeHighWater {
		if time.Now().After(deadline) {
			t.Fatal("bridge stopped sending early")
		}
		time.Sleep(time.Millisecond)
	}
	time.Sleep(50 * time.Millisecond)
	paused := ch.sentBytes()
	time.Sleep(100 * time.Millisecond)
	if ch.sentBytes() != paused {
		t.Fatal("bridge kept sending into a full channel")
	}
	// ...and go on once it drains.
	ch.drain()
	deadline = time.Now().Add(2 * time.Second)
	for ch.sentBytes() == paused {
		if time.Now().After(deadline) {
			t.Fatal("bridge didn't resume")
		}
		time.Sleep(time.Millisecond)
	}
	b.Close()
	computer.Close()
}

func TestBridgeDropsAClientThatFloodsIt(t *testing.T) {
	ch := &fakeChannel{}
	b := NewBridge(ch)
	chunk := make([]byte, 1<<20)
	for i := 0; i <= bridgeQueueMax/len(chunk); i++ {
		b.FromChannel(chunk)
	}
	if !ch.isClosed() || !b.isClosed() {
		t.Fatal("a client past the queue limit stays connected")
	}
}
