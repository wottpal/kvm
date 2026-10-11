# Fork instructions

This repository is a fork of [jetkvm/kvm](https://github.com/jetkvm/kvm). Every difference from upstream `dev` is a
divergence recorded in [FORK.md](FORK.md).

- A change that adds, changes or removes a divergence updates its FORK.md section in the same pull request, the
  **Files** list included. The divergence audit (`python3 scripts/fork/audit_divergence.py`, CI `fork-checks`) fails
  otherwise.
- Follow the policy in FORK.md: no changes to the web UI (`ui/`), the persisted configuration or the update mechanism.
- Pull requests go to this fork's `dev`. Nothing is proposed upstream without the maintainer's go-ahead.
- After editing `internal/native/proto/native.proto`, regenerate with the pinned generator versions (FORK.md, **Build**).
