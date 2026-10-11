# Fork divergences

This repository is a fork of [jetkvm/kvm](https://github.com/jetkvm/kvm) (GPL-2.0), the JetKVM device firmware. It
tunes the video encoder for desktop content and adds the encoder control that the
[JetKVM Desktop](https://github.com/wottpal/jetkvm-desktop) app uses. This file lists every **divergence**: each change
that makes this fork differ from upstream `dev`. A pull request that adds, changes or removes a divergence updates this
file in the same pull request.

## Base

- **Upstream:** [jetkvm/kvm](https://github.com/jetkvm/kvm), branch `dev`
- **Merge base:** `8f6c3001b1064eb32b3f66afd04efbe536bbaf87` (`release/0.5.9` plus upstream `dev` up to "Revert \"Add Auto
  video quality using RTCP REMB\" (#1693)", 2026-10-05)
- **Fork created:** 2026-10-11. Last upstream sync: 2026-10-11.

## Policy

- **Firmware only.** The fork never changes the web UI (`ui/`), the device's persisted configuration or its update
  mechanism. Every divergence lives in the video path and its control API.
- **Defaults change for every client.** Encoder divergences change what every client gets, the web UI included.
  Runtime overrides stay in memory and drop back to those defaults whenever the stream stops.
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
  - Requests are rate-limited to two per second; requests inside the window coalesce into one at its end. Only the
    current session may ask.
  - The native side (`video_request_keyframe`) verifies each request by watching the encoder output for an IDR, and
    keeps the first path that works: `RK_MPI_VENC_RequestIDR` instant (this rockit build rejects it), non-instant (it
    returns success even when the encoder ignores it), then a one-frame change of the GOP length, which makes MPP
    resend SPS/PPS with an IDR. A new gRPC method `VideoRequestKeyframe` carries the request to the native process.
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
  - **GOP 4 s** instead of 0.5 s; keyframes come on demand (D04).
  - **Super-frame re-encode** of any frame above 800 KiB, below the 1,036,800-byte frame limit of
    `internal/native/proxy.go`, which would otherwise end the stream.
  - Env overrides, read once when the native process starts: `JETKVM_VENC_MIN_QP` (8–40), `JETKVM_VENC_MAX_QP`,
    `JETKVM_VENC_GOP_SECONDS` (0.5–60) and `JETKVM_VENC_AQ` (0/1).
  - `log_notice` is always emitted (rendered as info, whatever the native log level), and the encoder's configuration
    lines use it, so the active settings are visible in every log.
- **Why:** upstream never sets rate-control parameters, so Rockchip's defaults apply: a frame-level QP floor of about
  26–28 plus up to +8 on high-texture blocks, which is exactly where text is. Text stayed at QP 32–36 whatever the
  bitrate: 32.5 dB on a test image, against 46.4 dB at the QP 18 floor.
- **Remove when:** upstream configures rate control for desktop content.

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

| Fork tag | Upstream base | `jetkvm_app` SHA-256 | Divergences | Notes |
| -------- | ------------- | -------------------- | ----------- | ----- |

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
- **D04:** clients that call `requestKeyframe` get "method not found" from stock firmware.
