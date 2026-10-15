package hostlink

import (
	"context"
	"io"
	"net"
	"testing"
	"time"

	"github.com/rs/zerolog"
	"gvisor.dev/gvisor/pkg/buffer"
	"gvisor.dev/gvisor/pkg/tcpip"
	"gvisor.dev/gvisor/pkg/tcpip/adapters/gonet"
	"gvisor.dev/gvisor/pkg/tcpip/header"
	"gvisor.dev/gvisor/pkg/tcpip/link/channel"
	"gvisor.dev/gvisor/pkg/tcpip/link/ethernet"
	"gvisor.dev/gvisor/pkg/tcpip/network/ipv6"
	"gvisor.dev/gvisor/pkg/tcpip/stack"
	"gvisor.dev/gvisor/pkg/tcpip/transport/icmp"
	"gvisor.dev/gvisor/pkg/tcpip/transport/tcp"
)

var (
	deviceMAC, _ = net.ParseMAC("02:4a:4b:00:00:01")
	hostMAC, _   = net.ParseMAC("02:4a:4b:00:00:02")
)

// computer is the other end: a stack with an SSH-like server that sends a
// banner, wired to the link through two channel endpoints.
type computer struct {
	stack *stack.Stack
	addr  tcpip.Address
}

func newPair(t *testing.T) (*Link, *computer) {
	t.Helper()
	deviceCh := channel.New(64, 1500, tcpip.LinkAddress(deviceMAC))
	hostCh := channel.New(64, 1500, tcpip.LinkAddress(hostMAC))
	log := zerolog.Nop()
	link, err := New(ethernet.New(deviceCh), deviceMAC, &log)
	if err != nil {
		t.Fatal(err)
	}

	s := stack.New(stack.Options{
		NetworkProtocols: []stack.NetworkProtocolFactory{ipv6.NewProtocolWithOptions(ipv6.Options{
			DADConfigs: stack.DADConfigurations{DupAddrDetectTransmits: 0},
		})},
		TransportProtocols: []stack.TransportProtocolFactory{tcp.NewProtocol, icmp.NewProtocol6},
	})
	if err := s.CreateNIC(1, ethernet.New(hostCh)); err != nil {
		t.Fatal(err)
	}
	addr := header.LinkLocalAddr(tcpip.LinkAddress(hostMAC))
	if err := s.AddProtocolAddress(1, tcpip.ProtocolAddress{Protocol: ipv6.ProtocolNumber,
		AddressWithPrefix: tcpip.AddressWithPrefix{Address: addr, PrefixLen: 64}}, stack.AddressProperties{}); err != nil {
		t.Fatal(err)
	}
	s.SetRouteTable([]tcpip.Route{{Destination: header.IPv6EmptySubnet, NIC: 1}})

	ctx, cancel := context.WithCancel(context.Background())
	go pump(ctx, deviceCh, hostCh)
	go pump(ctx, hostCh, deviceCh)
	t.Cleanup(func() {
		cancel()
		link.Close()
		s.Close()
	})
	return link, &computer{stack: s, addr: addr}
}

// pump copies frames from one endpoint's output to the other's input, as
// the cable would.
func pump(ctx context.Context, from, to *channel.Endpoint) {
	for {
		pkt := from.ReadContext(ctx)
		if pkt == nil {
			return
		}
		view := pkt.ToView()
		frame := append([]byte(nil), view.AsSlice()...)
		view.Release()
		pkt.DecRef()
		in := stack.NewPacketBuffer(stack.PacketBufferOptions{Payload: buffer.MakeWithData(frame)})
		to.InjectInbound(0, in)
		in.DecRef()
	}
}

func TestDiscoverFindsTheComputer(t *testing.T) {
	link, host := newPair(t)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	got, err := link.Discover(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if got != host.addr {
		t.Fatalf("discovered %s, want %s", got, host.addr)
	}
	if link.HostAddress() != host.addr.String() {
		t.Fatalf("HostAddress() = %q", link.HostAddress())
	}
	if want := "fe80::4a:4bff:fe00:1"; link.LocalAddress() != want {
		t.Fatalf("LocalAddress() = %q, want %q", link.LocalAddress(), want)
	}
}

func TestDialHostReachesTheSSHServer(t *testing.T) {
	link, host := newPair(t)
	listener, err := gonet.ListenTCP(host.stack, tcpip.FullAddress{NIC: 1, Port: SSHPort}, ipv6.ProtocolNumber)
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	go func() {
		conn, err := listener.Accept()
		if err != nil {
			return
		}
		_, _ = conn.Write([]byte("SSH-2.0-test\r\n"))
		_, _ = io.Copy(conn, conn) // echo
		conn.Close()
	}()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	conn, err := link.DialHost(ctx, SSHPort)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	_ = conn.SetDeadline(time.Now().Add(5 * time.Second))
	banner := make([]byte, 14)
	if _, err := io.ReadFull(conn, banner); err != nil || string(banner) != "SSH-2.0-test\r\n" {
		t.Fatalf("banner %q, %v", banner, err)
	}
	// A payload larger than a segment makes it through both ways.
	payload := make([]byte, 256<<10)
	for i := range payload {
		payload[i] = byte(i * 7)
	}
	go func() { _, _ = conn.Write(payload) }()
	echo := make([]byte, len(payload))
	if _, err := io.ReadFull(conn, echo); err != nil {
		t.Fatal(err)
	}
	for i := range echo {
		if echo[i] != payload[i] {
			t.Fatalf("byte %d differs", i)
		}
	}
}

func TestReportFromTheComputer(t *testing.T) {
	link, host := newPair(t)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	conn, err := gonet.DialContextTCP(ctx, host.stack,
		tcpip.FullAddress{NIC: 1, Addr: header.LinkLocalAddr(tcpip.LinkAddress(deviceMAC)), Port: ReportPort}, ipv6.ProtocolNumber)
	if err != nil {
		t.Fatal(err)
	}
	_, _ = conn.Write([]byte("user=zoma\nname=Dennis' Mac mini\n"))
	conn.Close()
	deadline := time.Now().Add(5 * time.Second)
	for link.Report() == nil {
		if time.Now().After(deadline) {
			t.Fatal("no report")
		}
		time.Sleep(10 * time.Millisecond)
	}
	if r := link.Report(); r.User != "zoma" || r.Name != "Dennis' Mac mini" || r.At.IsZero() {
		t.Fatalf("report %+v", r)
	}
}

func TestParseReport(t *testing.T) {
	for _, tc := range []struct {
		in   string
		user string
		ok   bool
	}{
		{"user=zoma\n", "zoma", true},
		{"name=x\nuser=first.last-2\n", "first.last-2", true},
		{"user=\n", "", false},
		{"user=a b\n", "", false},
		{"user=../etc\n", "", false},
		{"user=-rf\n", "", false},
		{"name=only\n", "", false},
	} {
		r, err := ParseReport([]byte(tc.in))
		if (err == nil) != tc.ok {
			t.Errorf("%q: err %v", tc.in, err)
			continue
		}
		if tc.ok && r.User != tc.user {
			t.Errorf("%q: user %q", tc.in, r.User)
		}
	}
	r, err := ParseReport([]byte("user=u\nname=a\x07b\n"))
	if err != nil || r.Name != "ab" {
		t.Fatalf("control characters kept: %+v %v", r, err)
	}
}
