package kvm

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"time"

	"github.com/coder/websocket"
	"github.com/gin-gonic/gin"
	"github.com/wottpal/blitzkvm/approval/broker"
	"github.com/wottpal/blitzkvm/approval/wire"

	"github.com/jetkvm/kvm/internal/hostlink"
	"github.com/jetkvm/kvm/internal/logging"
	"github.com/jetkvm/kvm/internal/usbgadget"
)

// Remote approvals (FORK.md D20): the approval broker (BlitzKVM's
// approval/broker, MIT) takes asks from the computer's approval daemon over
// the host link, offers them to the owner's approvers (BlitzKVM on a Mac,
// directly at /approvals/v1/ws or through a relay; the iPhone app through
// the relay) and, for screen asks, clicks and types on the computer with
// the USB keyboard and pointer. This file holds only the device's side:
// storage, the keyboard and pointer, the link listener and the routes.

// approvalsDir keeps the broker's keys and roster; approvals are on while a
// roster is there. Stock firmware ignores the directory.
var approvalsDir = "/userdata/jetkvm/approvals"

var approvalsLogger = logging.GetSubsystemLogger("approvals")

var (
	approvalsBroker *broker.Broker
	// approvalsActing blocks the session's keyboard and pointer, and hides
	// key states from it, while the broker clicks or types. Session reports
	// hold approvalsInput's read lock while they check it and write, so the
	// broker starts only after any report in flight is out.
	approvalsActing atomic.Bool
	approvalsInput  sync.RWMutex
)

// approvalsAct marks the broker as acting until the returned func runs.
func approvalsAct() func() {
	approvalsInput.Lock()
	approvalsActing.Store(true)
	approvalsInput.Unlock()
	return func() { approvalsActing.Store(false) }
}

type approvalsStore struct{}

func (approvalsStore) Load(name string) ([]byte, error) {
	return os.ReadFile(filepath.Join(approvalsDir, name))
}

func (approvalsStore) Save(name string, data []byte) error {
	if err := os.MkdirAll(approvalsDir, 0700); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(approvalsDir, name+".*")
	if err != nil {
		return err
	}
	if _, err := tmp.Write(data); err != nil {
		_ = tmp.Close()
		_ = os.Remove(tmp.Name())
		return err
	}
	if err := tmp.Sync(); err != nil {
		_ = tmp.Close()
		_ = os.Remove(tmp.Name())
		return err
	}
	_ = tmp.Close()
	return os.Rename(tmp.Name(), filepath.Join(approvalsDir, name))
}

func (approvalsStore) Remove(name string) error {
	if err := os.Remove(filepath.Join(approvalsDir, name)); err != nil && !os.IsNotExist(err) {
		return err
	}
	return nil
}

// approvalsHID presses on the computer for the broker, past the session's
// input path (which is blocked meanwhile) and without logging keys.
type approvalsHID struct{}

const (
	approvalsKeyDelay   = 25 * time.Millisecond
	approvalsClickDelay = 80 * time.Millisecond
	hidCapsLock         = 0x39
)

func (approvalsHID) Click(ctx context.Context, x, y float64) error {
	if !usbReadyForHidReports() {
		return errors.New("the computer's USB isn't ready")
	}
	defer approvalsAct()()
	ax, ay := int(x*32767), int(y*32767)
	for _, buttons := range []uint8{0, 1, 0} {
		if err := gadget.AbsMouseReport(ax, ay, buttons); err != nil {
			return err
		}
		select {
		case <-ctx.Done():
			_ = gadget.AbsMouseReport(ax, ay, 0)
			return ctx.Err()
		case <-time.After(approvalsClickDelay):
		}
	}
	return nil
}

func (approvalsHID) Type(ctx context.Context, strokes []wire.Stroke) error {
	if !usbReadyForHidReports() {
		return errors.New("the computer's USB isn't ready")
	}
	defer approvalsAct()()
	release := func() { _ = gadget.KeyboardReportSecret(0, nil) }
	release()
	defer release()
	if gadget.GetKeyboardState().CapsLock && typesCharacters(strokes) {
		// A Mac needs Caps Lock held a moment to take it.
		if err := pressFor(hidCapsLock, 0, 150*time.Millisecond); err != nil {
			return err
		}
	}
	for i, s := range strokes {
		if err := ctx.Err(); err != nil {
			return fmt.Errorf("stopped after %d of %d strokes: %w", i, len(strokes), err)
		}
		if !usbReadyForHidReports() {
			return fmt.Errorf("the computer's USB went away after %d of %d strokes", i, len(strokes))
		}
		if err := pressFor(s[0], s[1], approvalsKeyDelay); err != nil {
			return fmt.Errorf("stroke %d of %d: %w", i+1, len(strokes), err)
		}
	}
	return nil
}

func pressFor(key, mods byte, hold time.Duration) error {
	var keys []byte
	if key != 0 {
		keys = []byte{key}
	}
	if err := gadget.KeyboardReportSecret(mods, keys); err != nil && !usbgadget.IsHIDTemporarilyUnavailableError(err) {
		return err
	}
	time.Sleep(hold)
	if err := gadget.KeyboardReportSecret(0, nil); err != nil && !usbgadget.IsHIDTemporarilyUnavailableError(err) {
		return err
	}
	time.Sleep(approvalsKeyDelay)
	return nil
}

// typesCharacters: strokes beyond Return, Escape, Tab, Backspace and bare
// modifiers, which Caps Lock would change.
func typesCharacters(strokes []wire.Stroke) bool {
	for _, s := range strokes {
		if _, safe := wire.SafeKeys[s[0]]; !safe {
			return true
		}
	}
	return false
}

func initApprovals() {
	b, err := broker.New(broker.Config{
		Store:  approvalsStore{},
		HID:    approvalsHID{},
		Synced: func() bool { return timeSync != nil && timeSync.IsSyncSuccess() },
		Logf:   func(format string, args ...any) { approvalsLogger.Info().Msgf(format, args...) },
		DialRelay: func(ctx context.Context, url string) (broker.Conn, error) {
			c, _, err := websocket.Dial(ctx, url, nil)
			if err != nil {
				return nil, err
			}
			c.SetReadLimit(64 << 10)
			return wsConn{c}, nil
		},
	})
	if err != nil {
		approvalsLogger.Warn().Err(err).Msg("approvals unavailable")
		return
	}
	approvalsBroker = b
	go b.Run(context.Background())
	go superviseApprovalsLink(b)
}

// superviseApprovalsLink listens on each host link the supervisor starts
// (a link is replaced when the gadget recreates its interface).
func superviseApprovalsLink(b *broker.Broker) {
	var link *hostlink.Link
	var ln net.Listener
	for {
		current := currentHostLink()
		if current != link {
			if ln != nil {
				_ = ln.Close()
				ln = nil
			}
			link = current
			if link != nil {
				var err error
				if ln, err = link.Listen(hostlink.ApprovalsPort); err != nil {
					approvalsLogger.Warn().Err(err).Msg("can't listen on the host link")
				} else {
					go acceptApprovalsLink(b, ln)
				}
			}
		}
		time.Sleep(2 * time.Second)
	}
}

// acceptApprovalsLink serves the computer's daemon, a few connections at a
// time; each must authenticate within the broker's handshake.
func acceptApprovalsLink(b *broker.Broker, ln net.Listener) {
	slots := make(chan struct{}, 4)
	for {
		conn, err := ln.Accept()
		if err != nil {
			return
		}
		select {
		case slots <- struct{}{}:
		default:
			_ = conn.Close()
			continue
		}
		go func() {
			defer func() { <-slots }()
			if !b.Enabled() {
				_ = conn.Close()
				return
			}
			b.ServeLink(context.Background(), conn)
		}()
		time.Sleep(100 * time.Millisecond) // a flood costs the computer waiting
	}
}

// wsConn adapts coder/websocket to broker.Conn.
type wsConn struct{ c *websocket.Conn }

func (w wsConn) Read(ctx context.Context) ([]byte, error) {
	_, data, err := w.c.Read(ctx)
	return data, err
}

func (w wsConn) Write(ctx context.Context, data []byte) error {
	return w.c.Write(ctx, websocket.MessageText, data)
}

func (w wsConn) Close() error { return w.c.Close(websocket.StatusNormalClosure, "") }

var approverSockets = make(chan struct{}, 16)

// handleApprovalsWS serves an approver. No cookie: approvers authenticate
// with their keys inside the protocol. Browsers never get here (they send
// Origin), so no website can talk to the broker through one.
func handleApprovalsWS(c *gin.Context) {
	b := approvalsBroker
	if b == nil || !b.Enabled() {
		c.JSON(http.StatusNotFound, gin.H{"error": "approvals are off"})
		return
	}
	if c.Request.Header.Get("Origin") != "" {
		c.JSON(http.StatusForbidden, gin.H{"error": "not for browsers"})
		return
	}
	select {
	case approverSockets <- struct{}{}:
		defer func() { <-approverSockets }()
	default:
		c.JSON(http.StatusTooManyRequests, gin.H{"error": "too many approvers"})
		return
	}
	conn, err := websocket.Accept(c.Writer, c.Request, &websocket.AcceptOptions{})
	if err != nil {
		return
	}
	conn.SetReadLimit(64 << 10)
	b.ServeApprover(c.Request.Context(), wsConn{conn})
}

var approvalsRPCMu sync.Mutex

func rpcGetApprovalsState() (broker.State, error) {
	if approvalsBroker == nil {
		return broker.State{}, errors.New("approvals unavailable")
	}
	return approvalsBroker.State(), nil
}

// rpcSetApprovalsEnabled turns approvals on with a roster (the first one,
// or an owner-signed update) or off. It runs over the identity-pinned
// session of an authenticated client, which is what makes a genesis roster
// trustworthy.
func rpcSetApprovalsEnabled(enabled bool, roster wire.Envelope) (broker.State, error) {
	approvalsRPCMu.Lock()
	defer approvalsRPCMu.Unlock()
	b := approvalsBroker
	if b == nil {
		return broker.State{}, errors.New("approvals unavailable")
	}
	if !enabled {
		return b.State(), b.Disable()
	}
	if roster.Type == "" {
		return broker.State{}, errors.New("missing roster")
	}
	if !hostLinkEnabled() {
		return broker.State{}, errors.New("turn the host link on first: approvals reach the computer through it")
	}
	if err := b.Enable(&roster); err != nil {
		return broker.State{}, err
	}
	approvalsLogger.Info().Msg("approvals on")
	return b.State(), nil
}

func approvalsEnabled() bool { return approvalsBroker != nil && approvalsBroker.Enabled() }
