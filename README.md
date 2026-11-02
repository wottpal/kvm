> **Fork notice:** this repository is a fork of [jetkvm/kvm](https://github.com/jetkvm/kvm), tuned for
> desktop content and the BlitzKVM app. Every difference from upstream is documented in [FORK.md](FORK.md).

## Fork guide (maintainers and coding agents)

This is the BlitzKVM firmware: JetKVM's device app with an encoder tuned for desktop content, Opus audio, the control
API the [BlitzKVM](https://github.com/wottpal/blitzkvm) app uses, a USB host link and an approval broker. `AGENTS.md`
links to this file. [FORK.md](FORK.md) is the source of truth for every divergence from upstream `dev` (D01–D21), the
merge base, the releases and the procedures: read the sections of the divergences you touch before changing code.
Upstream's README starts after this guide; its build and development notes ([DEVELOPMENT.md](DEVELOPMENT.md)) apply
unless FORK.md says otherwise.

### Rules (mandatory)

- **Record every divergence.** A change that adds, changes or removes a difference from upstream `dev` updates its
  `### Dnn` section in FORK.md in the same commit, the **Files** list included (one backticked path per nested
  bullet). The divergence audit (`python3 scripts/fork/audit_divergence.py`, CI `fork-checks`) fails otherwise.
  Divergence IDs are never reused.
- **Stay in the firmware's lanes.** Never change the web UI (`ui/`) or the device's persisted configuration (the
  `Config` keys in `config.go`). The update mechanism changes only as D21 says: a fork build never updates itself.
  The fork's own state lives in flag files and `/userdata/jetkvm/approvals/`, which stock firmware ignores.
- **Upstream is read-only.** Work on this fork's `dev`. Propose nothing to jetkvm/kvm without the maintainer's
  go-ahead; the `upstream` remote's push URL is `DISABLED`. Upstream syncs land as merge commits: squashing one loses
  the merge base the audit diffs against.
- **Generated and vendored code.** After editing `internal/native/proto/native.proto`, run `scripts/generate_proto.sh`
  with the generator versions in the generated files' headers (protoc 3.21.12, protoc-gen-go v1.36.11,
  protoc-gen-go-grpc v1.6.1). Never edit `third_party/blitzkvm-approval/` by hand: `scripts/fork/sync_approval.sh
  <blitzkvm checkout>` copies BlitzKVM's `approval/` module from a pushed commit, and CI compares the copy.
- **Version.** Release builds use the latest upstream release whose changes the merge base holds (`VERSION=0.5.10`
  now), never a `-dev` or other pre-release version, which sorts below the release.

Do: `fix(video): keep the EDID when the app restarts (D08)` with FORK.md's D08 updated in the same commit.
Don't: edit `ui/src/…`, add a config key, `git push --tags` (it pushes every local tag), or build
`VERSION=0.5.10-dev`.

### Playbook

From the repository root on Apple silicon with Docker or OrbStack. `ghcr.io/jetkvm/buildkit` refuses anonymous pulls,
so the build image is built locally from `Dockerfile.build` (Go 1.26.10); rebuild it when that file changes.

| Task                          | Command                                                                                                       |
| ----------------------------- | ------------------------------------------------------------------------------------------------------------- |
| Set up a clone                | FORK.md, **Procedures › Set up a clone** (adds `upstream` without tags and with pushing disabled)             |
| Build image                   | `DOCKER_BUILD_TAG=jetkvm-buildkit:go1.26.10 bash -c 'source scripts/build_utils.sh && build_docker_image'`    |
| Web UI, embedded by `go:embed` | `nvm use "$(cat ui/.nvmrc)" && make frontend` (Node 22)                                                       |
| Release binary                | `make build_release VERSION=0.5.10 DOCKER_BUILD_TAG=jetkvm-buildkit:go1.26.10` → `bin/jetkvm_app`            |
| Vet and test, as CI's `build` | `docker run --rm --platform linux/amd64 --entrypoint sh -v "$PWD":/src -w /src -e GOFLAGS=-buildvcs=false jetkvm-buildkit:go1.26.10 -c 'go vet ./... && go test ./...'` |
| 32-bit tests, as `fork-checks` | the same container with `GOARCH=386 CGO_ENABLED=0 go test ./internal/hidrpc/ ./internal/webguard/ ./internal/redact/ ./internal/websecure/ ./internal/native/refresh/ ./internal/hostlink/` |
| Divergence audit              | `git fetch upstream dev && python3 scripts/fork/audit_divergence.py` (`--staged` checks the index before a commit) |

The root package's tests (`webrtc_test.go`, `stun_servers_test.go`, …) build for the device only; `go test ./...` on a
host skips them.

**Release** (see FORK.md, **Releases**):

1. Push the change to `dev` and wait for CI (`build`, `lint`, `fork-checks`) to pass.
2. Build the release binary from that clean commit: the binary embeds the commit it was built from.
3. Tag the commit `release/<upstream version>-fork.<n>` and push only that tag: `git push origin <tag>`.
4. Record the release in FORK.md (**Latest release**, a **Releases** row with the binary's SHA-256, size and
   divergences) in a `docs: record release/…` commit.
5. BlitzKVM ships the build: in the BlitzKVM repository, `macos/Support/Firmware.json` pins its tag, SHA-256, size and
   `getFirmwareFeatures` list, and `make -C macos firmware` bundles `../kvm/bin/jetkvm_app` (`FORK_BINARY`).

**Upstream sync:** FORK.md, **Procedures › Sync with upstream**. Keep upstream's side of every conflict and re-apply
the divergence on top; add new workflows to FORK.md's workflow inventory; update **Base** and each divergence's
upstream status.

### Device safety and escalation

- Ask the maintainer before installing a build on their device, rebooting it or changing its settings: it controls a
  computer that may be reachable only through it. Prefer **Test without installing**; an install over the network
  needs the boot guard (FORK.md, **Install on a device**).
- Never factory-reset a device or remove `/userdata/jetkvm/approvals/` (the broker's keys and the owner's roster).
- Never log or print passwords, keys or tokens; D15's redaction keeps them out of the app log, keep it that way.
- When FORK.md and the code disagree, or an upstream change rewrites code a divergence depends on in a way that isn't
  mechanical, stop and ask the maintainer instead of choosing a side silently.

**Done means:** the audit passes, `gofmt` is clean, vet and the tests above pass, CI on `dev` is green, and for a
release the SHA-256 in FORK.md matches `shasum -a 256 bin/jetkvm_app`.

---

<div align="center">
    <img alt="JetKVM logo" src="https://jetkvm.com/logo-blue.png" height="28">

### KVM

[Discord](https://jetkvm.com/discord) | [Website](https://jetkvm.com) | [Issues](https://github.com/jetkvm/cloud-api/issues) | [Docs](https://jetkvm.com/docs)

[![Twitter](https://img.shields.io/twitter/url/https/twitter.com/jetkvm.svg?style=social&label=Follow%20%40JetKVM)](https://twitter.com/jetkvm)

[![Go Report Card](https://goreportcard.com/badge/github.com/jetkvm/kvm)](https://goreportcard.com/report/github.com/jetkvm/kvm)

</div>

JetKVM is a high-performance, open-source KVM over IP (Keyboard, Video, Mouse) solution designed for efficient remote management of computers, servers, and workstations. Whether you're dealing with boot failures, installing a new operating system, adjusting BIOS settings, or simply taking control of a machine from afar, JetKVM provides the tools to get it done effectively.

## Features

- **Ultra-low Latency** - 1080p@60FPS video with 30-60ms latency using H.264 encoding. Smooth mouse and keyboard interaction for responsive remote control.
- **Free & Optional Remote Access** - Remote management via JetKVM Cloud using WebRTC.
- **Optional Tailscale Networking** - Built-in Tailscale status and control-server configuration, including custom [Headscale](https://headscale.net/)-compatible endpoints.
- **Open-source software** - Written in Golang on Linux. Easily customizable through SSH access to the JetKVM device.

## Contributing

We welcome contributions from the community! Whether it's improving the firmware, adding new features, or enhancing documentation, your input is valuable. We also have some rules and taboos here, so please read this page and our [Code of Conduct](/CODE_OF_CONDUCT.md) carefully.

## I need help

The best place to search for answers is our [Documentation](https://jetkvm.com/docs). If you can't find the answer there, check our [Discord Server](https://jetkvm.com/discord).

## I want to report an issue

If you've found an issue and want to report it, please check our [Issues](https://github.com/jetkvm/kvm/issues) page. Make sure the description contains information about the firmware version you're using, your platform, and a clear explanation of the steps to reproduce the issue.

# Development

JetKVM is written in Go & TypeScript. with some bits and pieces written in C. An intermediate level of Go & TypeScript knowledge is recommended for comfortable programming.

The project contains two main parts, the backend software that runs on the KVM device and the frontend software that is served by the KVM device, and also the cloud.

For comprehensive development information, including setup, testing, debugging, and contribution guidelines, see **[DEVELOPMENT.md](DEVELOPMENT.md)**.

For quick device development, use the `./dev_deploy.sh` script. It will build the frontend and backend and deploy them to the local KVM device. Run `./dev_deploy.sh --help` for more information.

## Backend

The backend is written in Go and is responsible for the KVM device management, the cloud API and the cloud web.

## Frontend

The frontend is written in React and TypeScript and is served by the KVM device. It has three build targets: `device`, `development` and `production`. Development is used for development of the cloud version on your local machine, device is used for building the frontend for the KVM device and production is used for building the frontend for the cloud.
