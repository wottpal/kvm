//go:build linux

package hostlink

import (
	"context"
	"errors"
	"fmt"
	"net"
	"os"
	"sync"

	"github.com/rs/zerolog"
	"github.com/vishvananda/netlink"
	"golang.org/x/sys/unix"
	"gvisor.dev/gvisor/pkg/buffer"
	"gvisor.dev/gvisor/pkg/tcpip"
	"gvisor.dev/gvisor/pkg/tcpip/link/channel"
	"gvisor.dev/gvisor/pkg/tcpip/link/ethernet"
	"gvisor.dev/gvisor/pkg/tcpip/stack"
)

// Open runs a Link on the interface: the kernel's IPv6 is turned off there,
// its IPv4 neither answers ARP nor accepts a packet (the interface has no
// address and no route, so strict reverse-path filtering drops them all),
// the interface comes up, and its IPv6 frames go through a packet socket to
// the link's own stack.
//
// The frames are pumped through a channel endpoint rather than gVisor's
// fdbased one: on 32-bit ARM, fdbased's sendmmsg batches fail with EMSGSIZE
// for every packet made of more than one buffer.
func Open(ifname string, log *zerolog.Logger) (*Link, error) {
	iface, err := net.InterfaceByName(ifname)
	if err != nil {
		return nil, err
	}
	for _, sysctl := range []struct{ path, value string }{
		{"ipv6/conf/" + ifname + "/disable_ipv6", "1"},
		{"ipv4/conf/" + ifname + "/rp_filter", "1"},
		{"ipv4/conf/" + ifname + "/arp_ignore", "8"},
	} {
		if err := os.WriteFile("/proc/sys/net/"+sysctl.path, []byte(sysctl.value), 0o644); err != nil {
			return nil, fmt.Errorf("isolate %s from the kernel: %w", ifname, err)
		}
	}
	nl, err := netlink.LinkByName(ifname)
	if err != nil {
		return nil, err
	}
	if err := netlink.LinkSetARPOff(nl); err != nil {
		return nil, fmt.Errorf("turn off ARP on %s: %w", ifname, err)
	}
	if err := netlink.LinkSetUp(nl); err != nil {
		return nil, fmt.Errorf("bring %s up: %w", ifname, err)
	}

	proto := htons(unix.ETH_P_IPV6)
	fd, err := unix.Socket(unix.AF_PACKET, unix.SOCK_RAW|unix.SOCK_CLOEXEC|unix.SOCK_NONBLOCK, int(proto))
	if err != nil {
		return nil, fmt.Errorf("packet socket: %w", err)
	}
	if err := unix.Bind(fd, &unix.SockaddrLinklayer{Protocol: proto, Ifindex: iface.Index}); err != nil {
		_ = unix.Close(fd)
		return nil, fmt.Errorf("bind packet socket: %w", err)
	}
	// A non-blocking descriptor goes to Go's poller, so Close ends a
	// pending Read.
	file := os.NewFile(uintptr(fd), "hostlink-"+ifname)

	mtu := uint32(iface.MTU)
	if mtu == 0 {
		mtu = 1500
	}
	ch := channel.New(256, mtu, tcpip.LinkAddress(iface.HardwareAddr))
	link, err := New(ethernet.New(ch), iface.HardwareAddr, log)
	if err != nil {
		_ = file.Close()
		return nil, err
	}

	ctx, cancel := context.WithCancel(context.Background())
	var wg sync.WaitGroup
	wg.Add(2)
	// Either pump ending (a read error, say) leaves the link deaf: Done tells.
	go func() { defer wg.Done(); defer link.markDead(); receive(file, ch, log) }()
	go func() { defer wg.Done(); defer link.markDead(); transmit(ctx, file, ch, log) }()
	link.closeEndpoint = func() {
		cancel()
		_ = file.Close()
		wg.Wait()
		ch.Close()
	}
	return link, nil
}

func receive(file *os.File, ch *channel.Endpoint, log *zerolog.Logger) {
	buf := make([]byte, 65536)
	for {
		n, err := file.Read(buf)
		if err != nil {
			if !isClosed(err) {
				log.Warn().Err(err).Msg("host link: reading frames failed")
			}
			return
		}
		pkt := stack.NewPacketBuffer(stack.PacketBufferOptions{Payload: buffer.MakeWithData(append([]byte(nil), buf[:n]...))})
		ch.InjectInbound(0, pkt) // the Ethernet endpoint reads the type from the frame
		pkt.DecRef()
	}
}

func transmit(ctx context.Context, file *os.File, ch *channel.Endpoint, log *zerolog.Logger) {
	for {
		pkt := ch.ReadContext(ctx)
		if pkt == nil {
			return
		}
		view := pkt.ToView()
		_, err := file.Write(view.AsSlice())
		view.Release()
		pkt.DecRef()
		if err != nil {
			if isClosed(err) {
				return
			}
			log.Debug().Err(err).Msg("host link: sending a frame failed")
		}
	}
}

func isClosed(err error) bool { return errors.Is(err, os.ErrClosed) }

func htons(v uint16) uint16 { return v<<8 | v>>8 }
