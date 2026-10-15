// Package hostlink is the firmware's network link to the computer over the
// USB gadget's CDC-NCM function (FORK.md D17).
//
// The link runs its own IPv6 and TCP stack (gVisor's netstack) on the
// interface's raw frames, and the device's kernel never configures an
// address there: nothing the JetKVM serves (its web server, SSH, mDNS) is
// reachable from the computer. The stack only dials out, to the computer's
// SSH server for a client's clipboard sync, and takes one short report from
// the computer when a client installs its key.
package hostlink

import (
	"bufio"
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"net"
	"regexp"
	"strings"
	"sync"
	"time"
	"unicode"

	"github.com/rs/zerolog"
	"gvisor.dev/gvisor/pkg/tcpip"
	"gvisor.dev/gvisor/pkg/tcpip/adapters/gonet"
	"gvisor.dev/gvisor/pkg/tcpip/header"
	"gvisor.dev/gvisor/pkg/tcpip/network/ipv6"
	"gvisor.dev/gvisor/pkg/tcpip/stack"
	"gvisor.dev/gvisor/pkg/tcpip/transport/icmp"
	"gvisor.dev/gvisor/pkg/tcpip/transport/tcp"
	"gvisor.dev/gvisor/pkg/tcpip/transport/udp"
	"gvisor.dev/gvisor/pkg/waiter"
)

const (
	// SSHPort is the computer's SSH server.
	SSHPort = 22
	// ReportPort takes the computer's report: the installer a client types
	// on the computer sends its user name here.
	ReportPort = 4022

	nicID     tcpip.NICID = 1
	reportMax             = 1024
	mdnsPort              = 5353
)

// ErrNoHost means no computer answered on the link.
var ErrNoHost = errors.New("no computer answers on the USB link")

// Report is what the computer sent to ReportPort last.
type Report struct {
	// User is the account that ran the installer.
	User string `json:"user"`
	// Name is the computer's name, for display.
	Name string    `json:"name,omitempty"`
	At   time.Time `json:"at"`
}

// Link is the stack on one interface.
type Link struct {
	stack *stack.Stack
	local tcpip.Address
	log   *zerolog.Logger
	// closeEndpoint stops the frames' pumps (nil for test endpoints).
	closeEndpoint func()
	dead          chan struct{}
	deadOnce      sync.Once

	discoverMu sync.Mutex
	mu         sync.Mutex
	host       tcpip.Address
	report     *Report
	listener   *gonet.TCPListener
}

// New runs a stack on ep, a link endpoint that carries Ethernet frames, with
// the link-local address derived from mac. Duplicate address detection is
// off: the link has two ends and the other one's address differs.
func New(ep stack.LinkEndpoint, mac net.HardwareAddr, log *zerolog.Logger) (*Link, error) {
	s := stack.New(stack.Options{
		NetworkProtocols: []stack.NetworkProtocolFactory{ipv6.NewProtocolWithOptions(ipv6.Options{
			DADConfigs: stack.DADConfigurations{DupAddrDetectTransmits: 0},
		})},
		TransportProtocols: []stack.TransportProtocolFactory{tcp.NewProtocol, udp.NewProtocol, icmp.NewProtocol6},
	})
	if err := s.CreateNIC(nicID, ep); err != nil {
		s.Close()
		return nil, fmt.Errorf("create NIC: %s", err)
	}
	local := header.LinkLocalAddr(tcpip.LinkAddress(mac))
	if err := s.AddProtocolAddress(nicID, tcpip.ProtocolAddress{
		Protocol:          ipv6.ProtocolNumber,
		AddressWithPrefix: tcpip.AddressWithPrefix{Address: local, PrefixLen: 64},
	}, stack.AddressProperties{}); err != nil {
		s.Close()
		return nil, fmt.Errorf("add address: %s", err)
	}
	s.SetRouteTable([]tcpip.Route{{Destination: header.IPv6EmptySubnet, NIC: nicID}})

	l := &Link{stack: s, local: local, log: log, dead: make(chan struct{})}
	listener, err := gonet.ListenTCP(s, tcpip.FullAddress{NIC: nicID, Port: ReportPort}, ipv6.ProtocolNumber)
	if err != nil {
		s.Close()
		return nil, fmt.Errorf("listen for reports: %w", err)
	}
	l.listener = listener
	go l.serveReports(listener)
	return l, nil
}

// LocalAddress is the device's link-local address.
func (l *Link) LocalAddress() string { return l.local.String() }

// HostAddress is the computer's link-local address as last discovered, or "".
func (l *Link) HostAddress() string {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.host.BitLen() == 0 {
		return ""
	}
	return l.host.String()
}

// Report returns the computer's last report, or nil.
func (l *Link) Report() *Report {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.report == nil {
		return nil
	}
	r := *l.report
	return &r
}

// Done closes when the link stopped carrying frames (or was closed).
func (l *Link) Done() <-chan struct{} { return l.dead }

func (l *Link) markDead() { l.deadOnce.Do(func() { close(l.dead) }) }

// Close stops the stack and the frames' pumps.
func (l *Link) Close() {
	_ = l.listener.Close()
	if l.closeEndpoint != nil {
		l.closeEndpoint()
	}
	l.stack.Destroy()
	l.markDead()
}

// Discover finds the computer's link-local address: it pings all nodes and
// asks mDNS (which answers even with the macOS firewall's stealth mode on),
// and takes the first answer that isn't its own.
func (l *Link) Discover(ctx context.Context) (tcpip.Address, error) {
	l.discoverMu.Lock()
	defer l.discoverMu.Unlock()
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()

	found := make(chan tcpip.Address, 2)
	errs := make(chan error, 2)
	go func() { errs <- l.listenForAnswer(ctx, icmp.ProtocolNumber6, pingRequest(), 0, found) }()
	go func() { errs <- l.listenForAnswer(ctx, udp.ProtocolNumber, mdnsQuery(), mdnsPort, found) }()

	var lastErr error
	for pending := 2; pending > 0; {
		select {
		case host := <-found:
			l.mu.Lock()
			l.host = host
			l.mu.Unlock()
			return host, nil
		case err := <-errs:
			pending--
			if err != nil && !errors.Is(err, context.Canceled) && !errors.Is(err, context.DeadlineExceeded) {
				lastErr = err
			}
		}
	}
	if lastErr != nil {
		return tcpip.Address{}, lastErr
	}
	return tcpip.Address{}, ErrNoHost
}

// listenForAnswer sends payload to a multicast group (all nodes for a ping,
// mDNS's group otherwise) and reports the first source that isn't us.
func (l *Link) listenForAnswer(ctx context.Context, proto tcpip.TransportProtocolNumber, payload []byte, port uint16,
	found chan<- tcpip.Address) error {
	var wq waiter.Queue
	ep, terr := l.stack.NewEndpoint(proto, ipv6.ProtocolNumber, &wq)
	if terr != nil {
		return fmt.Errorf("endpoint: %s", terr)
	}
	defer ep.Close()
	entry, readable := waiter.NewChannelEntry(waiter.ReadableEvents)
	wq.EventRegister(&entry)
	defer wq.EventUnregister(&entry)

	group := header.IPv6AllNodesMulticastAddress
	if proto == udp.ProtocolNumber {
		group = tcpip.AddrFrom16([16]byte{0xff, 0x02, 15: 0xfb})
	}
	to := tcpip.FullAddress{NIC: nicID, Addr: group, Port: port}
	send := func() error {
		if _, terr := ep.Write(bytes.NewReader(payload), tcpip.WriteOptions{To: &to}); terr != nil {
			return fmt.Errorf("send: %s", terr)
		}
		return nil
	}
	if err := send(); err != nil {
		return err
	}
	resend := time.NewTicker(500 * time.Millisecond)
	defer resend.Stop()
	for {
		res, terr := ep.Read(io.Discard, tcpip.ReadOptions{NeedRemoteAddr: true})
		if terr == nil {
			if from := res.RemoteAddr.Addr; from != l.local && from.BitLen() == 128 {
				select {
				case found <- from:
				default:
				}
				return nil
			}
			continue
		}
		if _, ok := terr.(*tcpip.ErrWouldBlock); !ok {
			return fmt.Errorf("read: %s", terr)
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-readable:
		case <-resend.C:
			if err := send(); err != nil {
				return err
			}
		}
	}
}

// DialHost connects to a TCP port on the computer, discovering it first
// when needed (and again if the address it knew stopped answering).
func (l *Link) DialHost(ctx context.Context, port uint16) (net.Conn, error) {
	l.mu.Lock()
	host := l.host
	l.mu.Unlock()
	if host.BitLen() != 0 {
		dialCtx, cancel := context.WithTimeout(ctx, 3*time.Second)
		conn, err := l.dial(dialCtx, host, port)
		cancel()
		if err == nil {
			return conn, nil
		}
		l.log.Info().Err(err).Str("host", host.String()).Msg("host link: known address failed, discovering again")
	}
	discoverCtx, cancel := context.WithTimeout(ctx, 3*time.Second)
	host, err := l.Discover(discoverCtx)
	cancel()
	if err != nil {
		return nil, err
	}
	return l.dial(ctx, host, port)
}

func (l *Link) dial(ctx context.Context, host tcpip.Address, port uint16) (net.Conn, error) {
	return gonet.DialContextTCP(ctx, l.stack, tcpip.FullAddress{NIC: nicID, Addr: host, Port: port}, ipv6.ProtocolNumber)
}

// serveReports takes one report at a time, so a flood of connections from
// the computer costs it waiting, not the device memory.
func (l *Link) serveReports(listener *gonet.TCPListener) {
	for {
		conn, err := listener.Accept()
		if err != nil {
			return
		}
		l.readReport(conn)
	}
}

// readReport takes "key=value" lines (user, name) up to reportMax bytes.
func (l *Link) readReport(conn net.Conn) {
	defer conn.Close()
	_ = conn.SetReadDeadline(time.Now().Add(3 * time.Second))
	data, _ := io.ReadAll(io.LimitReader(conn, reportMax))
	report, err := ParseReport(data)
	if err != nil {
		l.log.Debug().Err(err).Msg("host link: ignoring a malformed report")
		return
	}
	report.At = time.Now()
	l.mu.Lock()
	l.report = report
	l.mu.Unlock()
	l.log.Info().Msg("host link: the computer reported its user")
}

var userPattern = regexp.MustCompile(`^[A-Za-z0-9_][A-Za-z0-9._-]{0,63}$`)

// ParseReport reads a report: "user=<short name>" and optionally
// "name=<computer name>", one per line.
func ParseReport(data []byte) (*Report, error) {
	r := &Report{}
	scanner := bufio.NewScanner(bytes.NewReader(data))
	for scanner.Scan() {
		key, value, ok := strings.Cut(strings.TrimSpace(scanner.Text()), "=")
		if !ok {
			continue
		}
		switch key {
		case "user":
			r.User = value
		case "name":
			r.Name = strings.Map(func(c rune) rune {
				if unicode.IsPrint(c) {
					return c
				}
				return -1
			}, value)
			if len([]rune(r.Name)) > 128 {
				r.Name = string([]rune(r.Name)[:128])
			}
		}
	}
	if !userPattern.MatchString(r.User) {
		return nil, fmt.Errorf("invalid user %q", r.User)
	}
	return r, nil
}

// pingRequest is an ICMPv6 echo request; the endpoint fills in its ID.
func pingRequest() []byte {
	req := make([]byte, header.ICMPv6EchoMinimumSize)
	header.ICMPv6(req).SetType(header.ICMPv6EchoRequest)
	binary.BigEndian.PutUint16(req[6:], 1)
	return req
}

// mdnsQuery asks for the PTR records of _ssh._tcp.local (there while Remote
// Login is on) and of the service list. Sent from a port other than 5353,
// it is a legacy unicast query: responders answer to its source.
func mdnsQuery() []byte {
	q := []byte{0, 0, 0, 0, 0, 2, 0, 0, 0, 0, 0, 0} // ID 0, two questions
	for _, name := range [][]string{{"_ssh", "_tcp", "local"}, {"_services", "_dns-sd", "_udp", "local"}} {
		for _, label := range name {
			q = append(q, byte(len(label)))
			q = append(q, label...)
		}
		q = append(q, 0, 0, 12, 0, 1) // root, PTR, IN
	}
	return q
}
