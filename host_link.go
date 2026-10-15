package kvm

import (
	"context"
	"errors"
	"fmt"
	"net"
	"os"
	"sync"
	"time"

	"github.com/jetkvm/kvm/internal/hostlink"
	"github.com/jetkvm/kvm/internal/logging"
	"github.com/pion/webrtc/v4"
)

// The host link (FORK.md D17): a USB network function to the computer that
// a client uses to reach the computer's SSH server, for clipboard sync.
// The device's own services never become reachable over it (see
// internal/hostlink).

// hostLinkOnFile turns the host link on while it exists. A flag file rather
// than a configuration key, like D16's: stock firmware ignores it, so going
// back to it drops the link, and a factory reset removes it.
var hostLinkOnFile = "/userdata/jetkvm/host-link.on"

// hostLinkSSHLabel is the data channel a client opens to reach the
// computer's SSH server. Its messages are the TCP stream's bytes.
const hostLinkSSHLabel = "hostlink-ssh"

var hostLinkLogger = logging.GetSubsystemLogger("hostlink")

var (
	// hostLinkStartMu serializes starts, which wait for the interface
	// without holding hostLinkMu.
	hostLinkStartMu sync.Mutex
	hostLinkMu      sync.Mutex
	hostLink        *hostlink.Link
	// hostLinkIndex is the interface the link runs on: the gadget can
	// recreate it.
	hostLinkIndex int
	// hostLinkStartErr is why the last start failed ("" after a success).
	hostLinkStartErr string
)

func hostLinkEnabled() bool {
	_, err := os.Stat(hostLinkOnFile)
	return err == nil
}

// HostLinkState is getHostLinkState's answer.
type HostLinkState struct {
	Enabled bool `json:"enabled"`
	// Running: the link's stack runs on the interface.
	Running bool `json:"running"`
	// DeviceAddress is the device's link-local address (the source the
	// computer sees), HostAddress the computer's, once found.
	DeviceAddress string `json:"deviceAddress,omitempty"`
	HostAddress   string `json:"hostAddress,omitempty"`
	// SSH: the computer's SSH server accepted a connection (only with probe).
	SSH bool `json:"ssh"`
	// Report is what the computer's installer sent last.
	Report *hostlink.Report `json:"report,omitempty"`
	Error  string           `json:"error,omitempty"`
}

// rpcGetHostLinkState reports the link. With probe it looks for the
// computer and tries its SSH server (up to a few seconds).
func rpcGetHostLinkState(probe bool) HostLinkState {
	state := HostLinkState{Enabled: hostLinkEnabled()}
	link := currentHostLink()
	if link == nil {
		if state.Enabled {
			hostLinkMu.Lock()
			state.Error = hostLinkStartErr
			hostLinkMu.Unlock()
		}
		return state
	}
	state.Running = true
	state.DeviceAddress = link.LocalAddress()
	if probe {
		ctx, cancel := context.WithTimeout(context.Background(), 6*time.Second)
		conn, err := link.DialHost(ctx, hostlink.SSHPort)
		cancel()
		if err == nil {
			state.SSH = true
			_ = conn.Close()
		} else {
			state.Error = err.Error()
		}
	}
	state.HostAddress = link.HostAddress()
	state.Report = link.Report()
	return state
}

// rpcSetHostLinkState turns the link on or off. The gadget changes, so the
// computer re-enumerates the JetKVM's USB devices once.
func rpcSetHostLinkState(enabled bool) error {
	if enabled == hostLinkEnabled() {
		if enabled && currentHostLink() == nil {
			return startHostLink()
		}
		return nil
	}
	if err := setHostLinkFlag(enabled); err != nil {
		return err
	}
	if !enabled {
		stopHostLink() // after the flag: the supervisor would start it again
	}
	gadget.SetGadgetDevices(effectiveUsbDevices())
	if err := updateUsbRelatedConfig(); err != nil {
		// A gadget that fails to build must not come back at every boot.
		_ = setHostLinkFlag(!enabled)
		gadget.SetGadgetDevices(effectiveUsbDevices())
		return err
	}
	if enabled {
		return startHostLink()
	}
	return nil
}

func setHostLinkFlag(enabled bool) error {
	if enabled {
		if err := os.WriteFile(hostLinkOnFile, nil, 0644); err != nil {
			return fmt.Errorf("failed to turn the host link on: %w", err)
		}
		return nil
	}
	if err := os.Remove(hostLinkOnFile); err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("failed to turn the host link off: %w", err)
	}
	return nil
}

func currentHostLink() *hostlink.Link {
	hostLinkMu.Lock()
	defer hostLinkMu.Unlock()
	return hostLink
}

// superviseHostLink keeps the link's stack in line with the flag: it starts
// it at boot (and after a start that failed), restarts it on a recreated
// interface, and stops it when the flag is removed (over SSH, say).
func superviseHostLink() {
	for {
		checkHostLink()
		time.Sleep(5 * time.Second)
	}
}

func checkHostLink() {
	if !hostLinkEnabled() {
		stopHostLink()
		return
	}
	hostLinkMu.Lock()
	link, index := hostLink, hostLinkIndex
	hostLinkMu.Unlock()
	if link != nil {
		iface, err := net.InterfaceByName(gadget.NetworkInterfaceName())
		select {
		case <-link.Done():
			hostLinkLogger.Info().Msg("the host link stopped carrying frames: restarting it")
		default:
			if err == nil && iface.Index == index {
				return
			}
			hostLinkLogger.Info().Msg("the USB network interface changed: restarting the host link")
		}
		stopHostLink()
	}
	_ = startHostLink()
}

// startHostLink runs the link's stack on the gadget's network interface,
// which appears with the function.
func startHostLink() error {
	hostLinkStartMu.Lock()
	defer hostLinkStartMu.Unlock()
	if currentHostLink() != nil || !hostLinkEnabled() {
		return nil
	}
	link, index, err := openHostLink()
	hostLinkMu.Lock()
	defer hostLinkMu.Unlock()
	if err != nil {
		if err.Error() != hostLinkStartErr {
			hostLinkLogger.Warn().Err(err).Msg("failed to start the host link")
		}
		hostLinkStartErr = err.Error()
		return err
	}
	hostLink, hostLinkIndex, hostLinkStartErr = link, index, ""
	hostLinkLogger.Info().Str("address", link.LocalAddress()).Msg("host link up")
	return nil
}

// openHostLink waits for the gadget's network interface (it appears with
// the function) and runs the link on it.
func openHostLink() (*hostlink.Link, int, error) {
	var ifname string
	for i := 0; i < 50 && ifname == ""; i++ {
		if ifname = gadget.NetworkInterfaceName(); ifname == "" {
			time.Sleep(100 * time.Millisecond)
		}
	}
	if ifname == "" {
		return nil, 0, errors.New("the USB network interface didn't appear")
	}
	iface, err := net.InterfaceByName(ifname)
	if err != nil {
		return nil, 0, err
	}
	link, err := hostlink.Open(ifname, hostLinkLogger)
	if err != nil {
		return nil, 0, err
	}
	return link, iface.Index, nil
}

func stopHostLink() {
	hostLinkMu.Lock()
	link := hostLink
	hostLink = nil
	hostLinkMu.Unlock()
	if link != nil {
		link.Close()
		hostLinkLogger.Info().Msg("host link down")
	}
}

// handleHostLinkSSHChannel connects the data channel to the computer's SSH
// server. The channel closes when the connection does, and the other way
// round; a client that can't reach the computer sees it close at once.
func handleHostLinkSSHChannel(d *webrtc.DataChannel) {
	scopedLogger := hostLinkLogger.With().Uint16("data_channel_id", *d.ID()).Logger()
	b := hostlink.NewBridge(d)
	// Messages can arrive as soon as the channel opens: queue them until the
	// connection is up.
	d.OnMessage(func(msg webrtc.DataChannelMessage) { b.FromChannel(msg.Data) })
	d.OnClose(b.Close)
	d.OnOpen(func() {
		go func() {
			link := currentHostLink()
			if link == nil {
				scopedLogger.Info().Msg("host link is off: closing the SSH channel")
				_ = d.Close()
				return
			}
			ctx, cancel := context.WithTimeout(context.Background(), 8*time.Second)
			conn, err := link.DialHost(ctx, hostlink.SSHPort)
			cancel()
			if err != nil {
				scopedLogger.Info().Err(err).Msg("can't reach the computer's SSH server")
				_ = d.Close()
				return
			}
			scopedLogger.Info().Msg("SSH channel connected to the computer")
			b.Run(conn)
			scopedLogger.Info().Msg("SSH channel closed")
		}()
	})
}
