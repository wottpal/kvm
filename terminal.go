package kvm

import (
	"bytes"
	"encoding/json"
	"io"
	"os"
	"os/exec"
	"sync"

	"github.com/creack/pty"
	"github.com/pion/webrtc/v4"
)

type TerminalSize struct {
	Rows int `json:"rows"`
	Cols int `json:"cols"`
}

// devModeEnabled reports whether developer mode is on; an error reading its
// state counts as off.
func devModeEnabled() bool {
	state, err := rpcGetDevModeState()
	if err != nil {
		terminalLogger.Warn().Err(err).Msg("failed to read the developer mode state")
		return false
	}
	return state.Enabled
}

// terminalShellStops ends the shell of every open terminal channel; turning
// developer mode off calls it (D15).
var (
	terminalShellStopsMu sync.Mutex
	terminalShellStops   = map[*webrtc.DataChannel]func(){}
)

func endTerminalShells() {
	terminalShellStopsMu.Lock()
	defer terminalShellStopsMu.Unlock()
	for _, stop := range terminalShellStops {
		stop()
	}
}

// handleTerminalChannel runs a root shell on the device for the channel, only
// while developer mode is on (D15). The web UI opens the channel with every
// session and shows the terminal only in developer mode; the device used to
// start the shell for every session regardless. This narrows what a session
// starts by default; it is not an access boundary, since a session can turn
// developer mode on. The shell starts when the channel opens in developer
// mode, else with the first message once it is on, and ends when developer
// mode is turned off.
func handleTerminalChannel(d *webrtc.DataChannel) {
	scopedLogger := terminalLogger.With().
		Uint16("data_channel_id", *d.ID()).Logger()

	var (
		mu     sync.Mutex
		ptmx   *os.File
		cmd    *exec.Cmd
		size   *pty.Winsize // the last size the client sent, for a shell started later
		closed bool
	)

	stopShellLocked := func() {
		if ptmx != nil {
			ptmx.Close()
			ptmx = nil
		}
		if cmd != nil && cmd.Process != nil {
			_ = cmd.Process.Kill()
		}
		cmd = nil
	}

	// startShellLocked returns the shell's pty, starting the shell if developer
	// mode allows it; nil if it does not or the shell failed to start.
	startShellLocked := func() *os.File {
		if closed {
			return nil
		}
		if !devModeEnabled() {
			// Also when developer mode went off other than by the RPC.
			if ptmx != nil {
				scopedLogger.Info().Msg("developer mode is off, ending the terminal's shell")
				stopShellLocked()
			}
			return nil
		}
		if ptmx != nil {
			return ptmx
		}

		c := exec.Command("/bin/sh")
		p, err := pty.StartWithSize(c, size)
		if err != nil {
			scopedLogger.Warn().Err(err).Msg("Failed to start pty")
			// Not under mu: closing may run OnClose, which takes it.
			go d.Close()
			return nil
		}
		ptmx, cmd = p, c
		// Reap the shell when it exits, whether on its own or from the kill
		// in OnClose. Without this every closed session leaves a zombie.
		go func() { _ = c.Wait() }()

		go func() {
			buf := make([]byte, 1024)
			for {
				n, err := p.Read(buf)
				if err != nil {
					if err != io.EOF {
						scopedLogger.Warn().Err(err).Msg("Failed to read from pty")
					}
					break
				}
				err = d.Send(buf[:n])
				if err != nil {
					scopedLogger.Warn().Err(err).Msg("Failed to send pty output")
					break
				}
			}
		}()
		return p
	}

	terminalShellStopsMu.Lock()
	terminalShellStops[d] = func() {
		mu.Lock()
		defer mu.Unlock()
		if ptmx != nil {
			scopedLogger.Info().Msg("developer mode turned off, ending the terminal's shell")
			stopShellLocked()
		}
	}
	terminalShellStopsMu.Unlock()

	d.OnOpen(func() {
		mu.Lock()
		defer mu.Unlock()
		startShellLocked()
	})

	d.OnMessage(func(msg webrtc.DataChannelMessage) {
		var resize *pty.Winsize
		if msg.IsString {
			maybeJson := bytes.TrimSpace(msg.Data)
			// Cheap check to see if this resembles JSON
			if len(maybeJson) > 1 && maybeJson[0] == '{' && maybeJson[len(maybeJson)-1] == '}' {
				var s TerminalSize
				if err := json.Unmarshal(maybeJson, &s); err != nil {
					scopedLogger.Warn().Err(err).Msg("Failed to parse terminal size")
				} else {
					resize = &pty.Winsize{Rows: uint16(s.Rows), Cols: uint16(s.Cols)}
				}
			}
		}

		mu.Lock()
		if resize != nil {
			size = resize
		}
		p := startShellLocked()
		mu.Unlock()
		if p == nil {
			return
		}

		if resize != nil {
			if err := pty.Setsize(p, resize); err != nil {
				scopedLogger.Warn().Err(err).Msg("Failed to set terminal size")
				return
			}
			scopedLogger.Info().Uint16("rows", resize.Rows).Uint16("cols", resize.Cols).Msg("Set terminal size")
			return
		}
		if _, err := p.Write(msg.Data); err != nil {
			scopedLogger.Warn().Err(err).Msg("Failed to write to pty")
		}
	})

	d.OnClose(func() {
		mu.Lock()
		closed = true
		stopShellLocked()
		mu.Unlock()

		terminalShellStopsMu.Lock()
		delete(terminalShellStops, d)
		terminalShellStopsMu.Unlock()
		scopedLogger.Info().Msg("Terminal channel closed")
	})

	d.OnError(func(err error) {
		scopedLogger.Warn().Err(err).Msg("Terminal channel error")
	})
}
