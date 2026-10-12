# Fork divergences

This repository is a fork of [jetkvm/kvm](https://github.com/jetkvm/kvm) (GPL-2.0), the JetKVM device firmware. It
tunes the video encoder for desktop content and adds the encoder control that the BlitzKVM app uses. This file lists
every **divergence**: each change that makes this fork differ from upstream `dev`. A pull request that adds, changes
or removes a divergence updates this file in the same pull request.

## Base

- **Upstream:** [jetkvm/kvm](https://github.com/jetkvm/kvm), branch `dev`
- **Merge base:** `8f6c3001b1064eb32b3f66afd04efbe536bbaf87` (`release/0.5.9` plus upstream `dev` up to "Revert \"Add Auto
  video quality using RTCP REMB\" (#1693)", 2026-10-05)
- **Fork created:** 2026-10-11. Last upstream sync: 2026-10-11.
- **Latest release:** `release/0.5.9-fork.4` (see **Releases**).

## Policy

- **Firmware only.** The fork never changes the web UI (`ui/`), the device's persisted configuration or its update
  mechanism. Every divergence lives in the video path and its control API.
- **Defaults change for every client.** Encoder divergences change what every client gets, the web UI included.
  Runtime overrides stay in memory and drop back to those defaults whenever the stream stops or another session
  takes over.
- **Upstream version.** Builds keep upstream's `VERSION` (see **Releases**), so the device's auto-update replaces a fork
  build with the next upstream release.
- **Upstream later.** No divergence is proposed upstream yet. Each records its upstream status, and a divergence is
  removed once upstream ships it.

## Format

Each divergence is a `### Dnn — title` section below. IDs are never reused. Each section has these bullets:

- **Since:** date the divergence was first merged into the fork.
- **Kind:** `feature`, `fix`, `refactor`, `test`, `docs` or `ci`.
- **Upstream:** `fork-only`, `not proposed`, or a link to the upstream pull request or issue with its state.
- **Files:** one backticked path per nested bullet: an exact path, or a directory prefix ending in `/` for a directory
  the fork adds (changes inside upstream directories are listed file by file). Deleted files and the old path of a
  renamed file are listed too. A path may appear under more than one divergence.
- **What**, **Why** (optional) and **Remove when**.

The divergence audit (D02) reads the **Files** lists and the workflow inventory mechanically; keep them in this format.
It skips HTML comments and fenced code blocks.

## Divergences

### D01 — Fork notice and divergence log

- **Since:** 2026-10-11
- **Kind:** docs
- **Upstream:** fork-only
- **Files:**
  - `README.md`
  - `FORK.md`
  - `CLAUDE.md`
- **What:** a notice at the top of `README.md` linking to this file; this file; `CLAUDE.md`, which tells coding agents
  that every divergence is recorded here.
- **Remove when:** never (fork-only).

### D02 — Divergence audit in CI

- **Since:** 2026-10-11
- **Kind:** ci
- **Upstream:** fork-only
- **Files:**
  - `.github/workflows/fork-checks.yml`
  - `scripts/fork/`
- **What:** the `fork-checks` workflow runs `scripts/fork/audit_divergence.py` on every push to `dev` and every pull
  request. Every file that differs from the merge base with upstream `dev` must be listed under a divergence, every
  listed path must still differ, a directory entry may only name a directory the fork adds, and every workflow file
  must be in the workflow inventory. Run it locally with `python3 scripts/fork/audit_divergence.py` after
  `git fetch upstream dev` (`--staged` audits the index before committing).
- **Remove when:** never (fork-only).

### D03 — Color signaling matches the HDMI bridge

- **Since:** 2026-10-11
- **Kind:** fix
- **Upstream:** not proposed
- **Files:**
  - `internal/native/cgo/video.c`
- **What:** the stream is tagged with the BT.601 matrix, BT.709 primaries and transfer, limited range, at every
  resolution: in the H.264/H.265 VUI, the V4L2 capture format and the encoder's frame color gamut. Upstream tags
  modes above 576 lines as BT.709 throughout.
- **Why:** the TC358749 bridge converts RGB to BT.601 limited-range YCbCr for the YUV 4:2:2 bus whatever the
  resolution (`tc35874x_set_csi_color_space()`, `MASK_VOUT_COLOR_601_YCBCR_LIMITED`). Measured against a test image,
  decoded colors match the BT.601 matrix within 0.5 code values; decoded as BT.709, they are off by about 8. Only the
  tags change; the conversion is the bridge's.
- **Remove when:** upstream tags the stream by the bridge's conversion.

### D04 — Keyframes on demand

- **Since:** 2026-10-11
- **Kind:** feature
- **Upstream:** not proposed
- **Files:**
  - `go.mod`
  - `native.go`
  - `video.go`
  - `webrtc.go`
  - `jsonrpc.go`
  - `internal/native/cgo/ctrl.c`
  - `internal/native/cgo/ctrl.h`
  - `internal/native/cgo/video.c`
  - `internal/native/cgo/video.h`
  - `internal/native/cgo_linux.go`
  - `internal/native/cgo_notlinux.go`
  - `internal/native/empty.go`
  - `internal/native/grpc_clientmethods.go`
  - `internal/native/grpc_servermethods.go`
  - `internal/native/interface.go`
  - `internal/native/proxy.go`
  - `internal/native/video.go`
  - `internal/native/proto/native.proto`
  - `internal/native/proto/native.pb.go`
  - `internal/native/proto/native_grpc.pb.go`
- **What:** the encoder sends an IDR frame when a client asks for one:
  - on RTCP PLI or FIR from the browser or app (upstream reads RTCP only to drain it);
  - when a WebRTC session reaches `connected`, since a handoff joins a running encoder and the first IDR may beat
    DTLS;
  - through the JSON-RPC method `requestKeyframe` (no parameters, returns `null`).
  - Requests are rate-limited to two per second; requests inside the window coalesce into one at its end. PLI/FIR
    and `requestKeyframe` count only from the current session (a replaced one lingers for a second).
  - The native side (`video_request_keyframe`) verifies each request by watching the encoder output for an IDR, and
    keeps the first path that works: `RK_MPI_VENC_RequestIDR` instant (this rockit build rejects it), non-instant (it
    returns success even when the encoder ignores it), then a one-frame change of the GOP length, which makes MPP
    resend SPS/PPS with an IDR. A new gRPC method `VideoRequestKeyframe` carries the request to the native process.
    The wait runs on the monotonic clock, so a wall-clock step (NTP at boot) cannot stretch it.
  - The still refresh (D07) uses the same paths but cannot wait for its own keyframe: a path that is rejected, or
    whose refresh produces no keyframe, moves the shared choice on to the next one.
- **Why:** without it, a decoder that lost a frame shows corruption until the next periodic keyframe; upstream's 0.5 s
  GOP bounds that by resending a full keyframe twice a second, which a long GOP cannot afford.
- **Remove when:** upstream answers PLI/FIR with keyframes.

### D05 — Encoder tuned for desktop content

- **Since:** 2026-10-11
- **Kind:** feature
- **Upstream:** not proposed
- **Files:**
  - `internal/native/cgo/ctrl.h`
  - `internal/native/cgo/log.h`
  - `internal/native/cgo/video.c`
  - `internal/native/chan.go`
- **What:**
  - **QP range 18–42** (keyframes up to 40) and **texture-adaptive QP off**: AQ steps and row QP deltas are zeroed,
    so the bitrate target decides quality uniformly.
  - **GOP 4 s** instead of 0.5 s (D07 raises it to 60 s); keyframes come on demand (D04).
  - **Super-frame re-encode** of any frame above 800 KiB (2.5 MiB since D07), below the native frame socket's limit
    in `internal/native/proxy.go` (then 1,036,800 bytes; D07 raises it), which would otherwise end the stream.
  - Env overrides, read once when the native process starts: `JETKVM_VENC_MIN_QP` (8–40), `JETKVM_VENC_MAX_QP`,
    `JETKVM_VENC_GOP_SECONDS` (0.5–60; 0.5–300 since D07) and `JETKVM_VENC_AQ` (0/1).
  - `log_notice` is always emitted (rendered as info, whatever the native log level), and the encoder's configuration
    lines use it, so the active settings are visible in every log.
- **Why:** upstream never sets rate-control parameters, so Rockchip's defaults apply: a frame-level QP floor of about
  26–28 plus up to +8 on high-texture blocks, which is exactly where text is. Text stayed at QP 32–36 whatever the
  bitrate: 32.5 dB on a test image, against 46.4 dB at the QP 18 floor.
- **Remove when:** upstream configures rate control for desktop content.

### D06 — Runtime encoder control

- **Since:** 2026-10-11
- **Kind:** feature
- **Upstream:** not proposed
- **Files:**
  - `cloud.go`
  - `jsonrpc.go`
  - `native.go`
  - `internal/native/cgo/ctrl.c`
  - `internal/native/cgo/ctrl.h`
  - `internal/native/cgo/video.c`
  - `internal/native/cgo_linux.go`
  - `internal/native/cgo_notlinux.go`
  - `internal/native/empty.go`
  - `internal/native/encoder_params.go`
  - `internal/native/grpc_clientmethods.go`
  - `internal/native/grpc_servermethods.go`
  - `internal/native/interface.go`
  - `internal/native/proxy.go`
  - `internal/native/video.go`
  - `internal/native/proto/native.proto`
  - `internal/native/proto/native.pb.go`
  - `internal/native/proto/native_grpc.pb.go`
- **What:** two JSON-RPC methods let a client tune the encoder while it streams:
  - `getVideoEncoderParams` returns the status object below, with the running channel's frame rate, GOP and
    bitrates while it streams.
  - `setVideoEncoderParams {"params": {...}}` merges a partial update (omitted fields keep their value), validates the
    result as a whole (an invalid one changes nothing) and returns the new status. Fields: `minQp` (8–40), `maxQp`
    (`minQp`–51), `gopSeconds`, `aq`, `maxFps` (0 = source rate, up to 120), `bitrateKbps` and `maxBitrateKbps`
    (100–50,000; 0 = derived from the quality factor, max 1.5× the target).
  - Bitrate, QP and AQ apply to the running channel without a keyframe; frame rate and GOP apply live and cost one
    keyframe. If a live change fails, the stream restarts the way a quality-factor change does (capture and encoder
    only; EDID and HDMI hot-plug are untouched).
  - `maxFps` drops captured frames evenly before the encoder (`JETKVM_VENC_MAX_FPS`, 0–120, sets its default).
    Rate control keeps the frame rate the channel was created with, and the bitrates are scaled to it: changing it
    live made rockit reprogram the encoder's input stride in pixels instead of bytes (packed YUV 4:2:2), and the
    kernel then dropped every frame (`frame info no equal set drop`) until the stream restarted (fixed in
    `release/0.5.9-fork.3`; before, the video stopped after any quality change between 60 and 30 fps). A scaled
    bitrate above 200,000 kbps takes the restart path instead, which recreates the channel at the new rate.
  - Overrides live in memory only. They reset to the defaults (D05, env included) when the stream stops (the last
    session leaves, or a session pauses it) and when the stream quality factor changes.
  - Overrides belong to the session that set them, and only the current session may set them. The stream keeps
    running across a session handoff, so the main app then restores the defaults it read when the native process
    started; the next client never inherits the previous one's settings. Partial updates are serialised.
  - Status object: the parameters, plus `sourceFps`, `encodeFps`, `gopFrames`, `effectiveBitrateKbps`,
    `effectiveMaxBitrateKbps`, `codec` and `streaming`.
- **Why:** the BlitzKVM app adapts quality to the network (frame rate, bitrate, QP floor) without re-plugging
  HDMI or restarting the stream.
- **Remove when:** upstream offers equivalent runtime control.

### D07 — Still-screen refresh

- **Since:** 2026-10-11
- **Kind:** feature
- **Upstream:** not proposed
- **Files:**
  - `go.mod`
  - `webrtc.go`
  - `internal/native/cgo/ctrl.h`
  - `internal/native/cgo/video.c`
  - `internal/native/cgo_linux.go`
  - `internal/native/encoder_params.go`
  - `internal/native/proxy.go`
  - `internal/native/proto/native.proto`
  - `internal/native/proto/native.pb.go`
- **What:** half a second after the screen stops changing, one keyframe at QP `stillQp` (default 8) makes the still
  picture near-lossless; the empty P-frames that follow keep it at no cost.
  - **Detection** reads the encoder's own output, never frame pixels. Each P-frame's cost above the empty-frame
    baseline (the smallest frame of the last one to two seconds, plus 150 bytes) feeds two decaying sums: over about
    3 s, 2,000 bytes mark the picture as changed (typing does, a blinking caret does not); over about 0.1 s, the sum
    tells when the changes have stopped. A frame above an eighth of the per-frame budget (1,500 bytes at least) and
    any other keyframe count as changes.
  - **Refresh:** a whole-frame ROI with absolute QP `stillQp` plus a keyframe request (D04's paths). The keyframe QP
    floor is permanently `stillQp`, since the encoder clamps ROI QPs to the frame type's floor; P-frames keep
    `minQp`. Nothing touches rate-control parameters at runtime (changing them restarts MPP rate control, which costs
    the next moving frames 2–7 dB), except D04's GOP-nudge path where RequestIDR does not work.
  - **Pacing:** refreshes are at least 1.5 s apart. A keyframe within 2 s after a refresh is taken as a receiver
    repairing a loss in it: it does not count as a change, and the interval doubles (up to 16 times, 24 s) until the
    picture changes. A refresh larger than a second of the target bitrate (at least 256 KiB, at most 1.5 MiB) raises
    the QP of the next ones by 6, up to +24, and one below half of that lowers it again, so a slow link does not
    stall behind near-lossless keyframes. A ROI that cannot be set is retried at the refresh interval.
  - **Parameters:** `stillQp` (1–51; at or above `minQp` turns the refresh off) joins D06's parameters and
    `JETKVM_VENC_STILL_QP` the env overrides. The GOP default becomes 60 s (`gopSeconds` up to 300).
  - **Frame size:** the encoder's stream buffer is 2 bytes per pixel of the mode, capped at 6 MiB, which is also the
    native frame socket's limit (`maxFrameSize`; modes up to 2176×1200 fit), and super frames re-encode above
    2.5 MiB: a refresh sets its QP per block, which the re-encode cannot raise.
  - **Loss repair:** the NACK responder keeps 4096 packets instead of pion's 1024 (about 1.2 MB, shorter than a
    refresh keyframe), so a loss early in a large keyframe can still be repaired.
- **Why:** P-frames never refine a still picture: the encoder skips blocks whose residual is small, at any QP. Measured
  on the BlitzKVM app's maximum level: light text 53.2 → 60.5 dB, colored text 28.1 → 51.5 dB (with the app's
  color reconstruction), a still screen 0.54 → 0.08 Mbit/s.
- **Trade-off:** other keyframes may go below `minQp` too. On a still desktop, keyframes from a PLI measured 103 KB
  instead of 87 KB (default parameters) and 165 KB instead of 136 KB (minimum QP 10).
- **Remove when:** upstream refines still screens.

### D08 — No HDMI re-plug when the app restarts

- **Since:** 2026-10-11
- **Kind:** fix
- **Upstream:** not proposed
- **Files:**
  - `native.go`
  - `video.go`
- **What:** each time the native process starts (app start, update, crash recovery), upstream rewrites the EDID
  (`reapplyHostDisplayAdvertisement("native_started")`, since upstream #1476), and every EDID write pulses HDMI
  hot-plug. When the native process (re)starts, the fork reads the bridge's EDID first and skips the write when it
  already matches; a different or missing EDID is still written. Explicit changes (`setEDID`, also with the same
  EDID) always write and re-plug, as upstream does, so re-applying an EDID still makes a host re-detect its display.
- **Why:** a host with several displays rearranges them on every hot-plug. Seen on a Mac mini with a second monitor:
  after an app restart its full-screen spaces moved and the JetKVM's absolute pointer was bound to the other monitor
  until USB re-enumerated. With the fix a restart leaves the host alone; video resumes within 2 s instead of after a
  re-detection of over 10 s.
- **Remove when:** upstream skips unchanged EDID writes.

### D09 — Data channels recover within a second after a network hiccup

- **Since:** 2026-10-11
- **Kind:** fix
- **Upstream:** not proposed
- **Files:**
  - `webrtc.go`
- **What:** the SCTP retransmission timeout is capped at 1 s (`SettingEngine.SetSCTPRTOMax`), pion's minimum RTO.
- **Why:** SCTP carries every data channel (HID, RPC, serial) and retransmits a lost packet after an RTO that doubles
  on every miss, up to 60 s by default. After a network hiccup the video resumes at once, but the data channels stay
  stuck until the backed-off timer fires: keyboard and pointer seem dead for seconds, or minutes after a longer
  outage. Measured with 3.3 s outages: 3.4–3.6 s of extra stall with the default, at most 0.7 s with the cap. The JetKVM
  Desktop app caps its own side the same way (it sends the input).
- **Remove when:** upstream caps the SCTP RTO.

### D10 — Capture buffers for modes up to 4.19 megapixels

- **Since:** 2026-10-11
- **Kind:** feature
- **Upstream:** not proposed
- **Files:**
  - `internal/native/cgo/video.c`
  - `internal/native/encoder_params.go`
- **What:** the three capture buffers hold 8 MiB each instead of upstream's 1920×1080×3 bytes, so modes up to 4.19
  megapixels at 2 bytes per pixel (YUYV) capture, e.g. 3008×1268 or 2560×1600. `getVideoEncoderParams` reports the size
  as `captureMaxBytes`. A mode that does not fit logs its size against the buffers' instead of a bare failure.
- **Why:** the TC358749's driver runs the CSI link at 109.7 Mpx/s for modes of 33 Hz or less (745 Mbit/s a lane above,
  438.75 at or below). A pixel clock just under that rate drains each line about as fast as it arrives, so any width up to
  the bridge's 4000-pixel lines streams, where the fast link stops near 2500 pixels. The BlitzKVM app uses this to
  give a large window its full resolution: 3008×1268 at 25 Hz for a 40" 21:9 display (measured clean, 0 decode errors).
  Upstream's buffers capped those modes at 3.1 megapixels. Video CMA: 42.6 of 48 MiB in use at 3008×1268 (the buffers
  are preallocated, 24.6 MiB idle).
- **Remove when:** upstream sizes the capture buffers for modes above 1080p.

### D11 — Early submit: encoding overlaps the capture

- **Since:** 2026-10-13
- **Kind:** feature
- **Upstream:** not proposed
- **Files:**
  - `internal/native/cgo/video.c`
- **What:** after dequeuing frame k, the capture loop hands the encoder the buffer VICAP is writing frame k+1 into (the
  next one in its ping-pong order), timed so the encoder, which is faster than the HDMI scan-out, trails the capture's
  writes: at frame k+1's start plus the active picture time (height × line time) minus the fastest recent encode
  (I-frames encode ~2 ms faster than P-frames), plus 2 ms. The frame then leaves the encoder about when its capture ends,
  instead of one encode time later. A dequeued buffer that is not the predicted one goes the normal way, logged.
  `JETKVM_EARLY_SUBMIT_US` overrides the offset (0 turns it off). `JETKVM_TIMING=1` logs capture, frame start → bitstream,
  handoff, sizes and the encode times and offset every 2 s. The frame's PTS is now its start time.
- **Why:** on the RV1106 the encoder takes about as long as a frame (15.2 ms at 2400×1012), and it only started once the
  frame was complete. Measured on a Mac host: frame start → bitstream 31.9 → 19.9 ms at 2400×1012@60 and 10.6 ms at
  1280×720@120; key press → screen 89 → 76 ms, pointer 67 → 50 ms (full screen, 40 samples each). A tear probe (frame
  counters at the top, middle and bottom of a page that changes every frame) found no torn frame in ~2000 frames with a
  keyframe every 0.5 s; tearing starts ~1.3 ms below the automatic offset (P-frames ~2 ms below).
- **Remove when:** never (the hardware path, an ISP/DVBM wrap, needs device-tree changes outside this fork).

### D12 — Wheel reports over HID-RPC

- **Since:** 2026-10-13
- **Kind:** feature
- **Upstream:** not proposed
- **Files:**
  - `hidrpc.go`
  - `internal/hidrpc/message.go`
  - `jsonrpc.go`
- **What:** HID-RPC message 0x04 (vertical, horizontal wheel step; one byte is vertical only) goes to the pointer's queue
  and the wheel report. The `getFirmwareFeatures` RPC lists the fork's protocol additions (`hidrpcWheel`), so a client uses
  them only where they exist. Upstream defines 0x04 but never handles it.
- **Why:** wheel steps went over JSON-RPC: each one a goroutine, JSON parsing and a reply on the single core, and no
  order between steps or against pointer moves. Single steps measured the same to the screen on a Mac host (83 ms
  either way, full screen); the gain is order and device CPU while scrolling fast.
- **Remove when:** upstream handles wheel reports over HID-RPC.

### D13 — Client-driven region refresh

- **Since:** 2026-10-13
- **Kind:** feature
- **Upstream:** not proposed
- **Files:**
  - `jsonrpc.go`
  - `internal/native/cgo/ctrl.c`
  - `internal/native/cgo/ctrl.h`
  - `internal/native/cgo/video.c`
  - `internal/native/cgo_linux.go`
  - `internal/native/cgo_notlinux.go`
  - `internal/native/empty.go`
  - `internal/native/encoder_params.go`
  - `internal/native/grpc_clientmethods.go`
  - `internal/native/grpc_servermethods.go`
  - `internal/native/interface.go`
  - `internal/native/proxy.go`
  - `internal/native/video.go`
  - `internal/native/proto/native.proto`
  - `internal/native/proto/native.pb.go`
  - `internal/native/proto/native_grpc.pb.go`
  - `internal/native/refresh/`
- **What:** the client decides when and where the picture is refreshed (it diffs decoded frames and knows its input
  timing and the link); the device executes.
  - `refreshRegions {"rects": [{"x", "y", "w", "h"}, ...], "qp": 1–51, "frames": 1–4}` returns `null`; `frames`
    defaults to 2 (omitted or 0). With 1–6 rects, each becomes a forced-intra ROI (indexes 0–5; D07's whole-frame ROI
    keeps 7) at absolute QP `qp`, aligned outward to 16 px and clipped to the frame, for the next `frames` encoded
    frames (frames already in the encoder when the ROIs are set do not count), then turned off (retried every frame if
    that fails). No keyframe; the encoder clamps `qp` to the P-frame floor, `minQp`. Without rects it is a full refresh
    now: D07's refresh (whole-frame ROI at `qp`, keyframe by D04's paths, its size and QP-boost bookkeeping) without its
    quiet and interval gating; the keyframe floor stays the lower of `stillQp` and `minQp`.
  - The encoder output thread applies a request right after a frame came out, when the next one has usually not
    reached the encoder. A request waits while a D07 refresh is in progress; a new one replaces the active one. Only
    the current session may call it, only while the stream runs; invalid parameters, a rect outside the frame and a
    stopped stream are JSON-RPC errors. Each request logs one always-on line:
    `region refresh: 2 rects, 215296 px, qp 8, 2 frames` or `region refresh: full frame, qp 8`.
  - `autoRefresh` (default `true`) joins D06's parameters: `false` keeps the device from starting D07 refreshes
    itself, while its change tracking runs on. It resets with the other overrides when the stream stops or another
    session takes over. The web UI never sets it, so it keeps D07's automatic refresh.
  - `getFirmwareFeatures` (D12) lists `refreshRegions`. The JSON-RPC dispatcher decodes a parameter that is a slice
    of structs from its JSON, as it did for a struct.
- **Why:** big frames block everything behind them on this device: Go's SRTP and the UDP send cost about 0.47 ms of CPU
  per 1200-byte packet, so a keystroke 10–60 ms after a keyframe is delayed by 23–61 ms (p90 +125 ms), and D07 sends a
  near-lossless whole-frame keyframe after every typing pause (250–350 KB, 120–230 ms to send). A forced-intra ROI
  refreshes only what changed: an 880×224 text rect went from 44.3 to 49.3 dB in one 41.5 KB P-frame, the rest
  untouched. Without forced intra it reached 46.0 dB; QP 1 and QP 8 gave the same result at `minQp` 18. Verified on the device (2400×1012, busy desktop page, `minQp` 10): an 880×224 text rect went from 50.2 to 55.3 dB in one 54 KB P-frame, leaving the rest untouched; a full refresh was 355 KB (59.1 dB).
- **Remove when:** upstream lets a client drive the encoder's refreshes.

## Repository settings

Settings that live in GitHub rather than in files:

- GitHub Actions are enabled, for upstream's `build` and `lint` workflows and the fork's `fork-checks`.
- Merge methods: squash for fork changes, merge commits for upstream syncs.

### Workflow inventory

Every file in `.github/workflows/` must be listed here (the divergence audit enforces it), so a workflow added by an
upstream sync cannot start running in this fork unnoticed.

| Workflow          | Origin     | State in this fork |
| ----------------- | ---------- | ------------------ |
| `build.yml`       | upstream   | enabled            |
| `lint.yml`        | upstream   | enabled            |
| `fork-checks.yml` | fork (D02) | enabled            |

## Releases

| Fork tag               | Upstream base                                         | `jetkvm_app` SHA-256                                               | Divergences | Notes                                                                                     |
| ---------------------- | ----------------------------------------------------- | ------------------------------------------------------------------ | ----------- | ----------------------------------------------------------------------------------------- |
| `release/0.5.9-fork.1` | `8f6c300` (`release/0.5.9` plus upstream `dev` to #1693) | `578ed5f58523cd5a5d40833061b093104edbe9ba48f4075767c69985c59b453e` | D01–D08     | First release, of `8f7e586`; 31,534,728 bytes; installed on the maintainer's device 2026-10-11 |
| `release/0.5.9-fork.2` | `8f6c300` (`release/0.5.9` plus upstream `dev` to #1693) | `a6d3fbf7d2cba58261c066d70ad5fb2b2b6c6d75834185f755decc8106849488` | D01–D09     | Adds D09 (SCTP RTO cap), of `6120af0`; 31,534,728 bytes; installed on the maintainer's device 2026-10-11 |
| `release/0.5.9-fork.3` | `8f6c300` (`release/0.5.9` plus upstream `dev` to #1693) | `88dbed94a6fd1c185e7adcaa47da94d23a42151503ebaeded39aab7d0b6fd7a1` | D01–D09     | D06 live frame-rate fix (#11) and review fixes for D04, D06–D08 (#12), of `5fc9be5`; 31,547,016 bytes; installed on the maintainer's device 2026-10-11 |
| `release/0.5.9-fork.4` | `8f6c300` (`release/0.5.9` plus upstream `dev` to #1693) | `9052cccccf3dd4355a4292728574f8d2ef21a1e2596d27dbce9923acfdc05d65` | D01–D10     | Adds D10 (capture buffers for modes up to 4.19 megapixels, #14), of `4a68d63`; 31,547,016 bytes; installed on the maintainer's device 2026-10-11 |

Fork releases are tagged `release/<upstream version>-fork.<n>`, for example `release/0.5.9-fork.1`, where
`<upstream version>` is the latest upstream release in the merge base. The binary itself is built with upstream's
`VERSION` (`make build_release VERSION=0.5.9`): a `-dev` or other pre-release version sorts below the release, and the
device's auto-update would replace it at once. With auto-update on, the next upstream release replaces a fork build.

## Procedures

### Set up a clone

```sh
git clone https://github.com/wottpal/kvm.git && cd kvm
git remote add --no-tags upstream https://github.com/jetkvm/kvm.git
git remote set-url --push upstream DISABLED
git fetch upstream dev
```

### Build

On Apple silicon with Docker or OrbStack:

```sh
bash -c 'source scripts/build_utils.sh && build_docker_image'   # linux/amd64 buildkit image (Rosetta)
(cd ui && nvm use) && make frontend                              # Node 22, embedded via go:embed
make build_release VERSION=0.5.9                                 # bin/jetkvm_app, ARM uClibc
```

`ghcr.io/jetkvm/buildkit` refuses anonymous pulls, so the image is built locally from `Dockerfile.build`; pass its tag
as `DOCKER_BUILD_TAG` if it differs. After a change to `internal/native/proto/native.proto`, regenerate the Go code
with `scripts/generate_proto.sh`, using the generator versions named in the generated files' headers (protoc 3.21.12,
protoc-gen-go v1.36.11, protoc-gen-go-grpc v1.6.1).

### Install on a device

Enable **Developer Mode** in the device's web UI and add an SSH key (user `root`). The device has no SFTP server;
copy files with `ssh ... "cat > file" < file`.

- **Test without installing:** copy the binary to `/userdata/jetkvm/bin/jetkvm_app_debug`, stop the installed app
  (`killall jetkvm_app`) and start the copy. A reboot brings back the installed app. Stop the copy first
  (`killall jetkvm_app_debug`): the shutdown script waits for a process named `jetkvm_app`, so `reboot` hangs while
  the copy runs.
- **Install:** back up `/userdata/jetkvm/bin/jetkvm_app`, copy the build to `/userdata/jetkvm/jetkvm_app.update`,
  check its SHA-256, `sync` and reboot. At boot the device moves the update into place, as an OTA update does.

### Sync with upstream

1. `git fetch upstream`, then create `sync/upstream-<version-or-sha>` from `origin/dev` and run `git merge upstream/dev`
   (or an upstream release tag). Do not use GitHub's "Sync fork" button.
2. Resolve conflicts by keeping upstream's change and re-applying the divergence on top of it. Drop a divergence only
   on purpose, and remove its section in the same pull request.
3. Review every change under `.github/workflows/` and add new workflows to the inventory.
4. Update **Base**, the upstream status of each divergence, and remove divergences that upstream has shipped. Open a
   pull request and merge it with a merge commit; squashing a sync loses the merge base.

## Rollback notes

- **Firmware:** install upstream's `jetkvm_app` the same way (the backup, or the release binary with its `.sha256`
  and `.sig`). The fork persists nothing of its own (encoder overrides live in memory, no configuration keys are
  added), so the stock app starts from the same configuration.
- **D04, D06:** clients that call `requestKeyframe`, `getVideoEncoderParams` or `setVideoEncoderParams` get
  "method not found" from stock firmware; the BlitzKVM app then falls back to the stream quality factor.
