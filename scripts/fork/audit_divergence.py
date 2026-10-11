#!/usr/bin/env python3
"""Divergence audit (FORK.md, D02).

Every file that differs from the merge base with upstream `dev` must be listed
under a divergence in FORK.md, every listed path must still differ, and every
workflow file must be in the workflow inventory.

    git fetch upstream dev
    python3 scripts/fork/audit_divergence.py [--upstream-ref upstream/dev] [--staged]

--staged audits the index (staged changes) instead of HEAD, before committing.
"""
import argparse
import re
import subprocess
import sys
from pathlib import Path

ROOT = Path(__file__).resolve().parents[2]
FILES_ENTRY = re.compile(r"^  - `([^`]+)`$")
INVENTORY_ROW = re.compile(r"^\| `([^`]+\.ya?ml)` +\|")


def git(*args):
    return subprocess.run(["git", *args], cwd=ROOT, check=True, capture_output=True, text=True).stdout


def fork_md_lines():
    """FORK.md without fenced code blocks and HTML comments."""
    text = re.sub(r"<!--.*?-->", "", (ROOT / "FORK.md").read_text(), flags=re.S)
    lines, fenced = [], False
    for line in text.splitlines():
        if line.startswith("```"):
            fenced = not fenced
        elif not fenced:
            lines.append(line)
    return lines


def listed_paths(lines):
    """Paths from every divergence's **Files:** list, with their divergence."""
    paths, divergence, in_files = [], None, False
    for line in lines:
        if line.startswith("### D"):
            divergence, in_files = line[4:].split(" ")[0], False
        elif line.startswith("- **Files:**"):
            in_files = divergence is not None
        elif in_files and line.startswith("  - "):
            match = FILES_ENTRY.match(line)
            if not match:
                sys.exit(f"FORK.md: {divergence}: a Files entry must be one backticked path: {line.strip()}")
            paths.append((divergence, match.group(1)))
        elif in_files:
            in_files = False
    return paths


def main():
    parser = argparse.ArgumentParser()
    parser.add_argument("--upstream-ref", default="upstream/dev")
    parser.add_argument("--staged", action="store_true", help="audit the index instead of HEAD")
    args = parser.parse_args()

    base = git("merge-base", "HEAD", args.upstream_ref).strip()
    target = ["--cached"] if args.staged else ["HEAD"]
    changed = set(git("diff", "--name-only", "--no-renames", base, *target).split())
    lines = fork_md_lines()
    listed = listed_paths(lines)
    errors = []

    def covered(path):
        return any(p == path or (p.endswith("/") and path.startswith(p)) for _, p in listed)

    for path in sorted(changed):
        if not covered(path):
            errors.append(f"{path} differs from upstream but no divergence lists it")
    for divergence, path in listed:
        if path.endswith("/"):
            if git("ls-tree", "-d", "--name-only", base, path.rstrip("/")).strip():
                errors.append(f"{divergence}: {path} is an upstream directory; list its changed files instead")
            elif not any(c.startswith(path) for c in changed):
                errors.append(f"{divergence}: nothing under {path} differs from upstream")
        elif path not in changed:
            errors.append(f"{divergence}: {path} no longer differs from upstream")

    inventory = {m.group(1) for m in map(INVENTORY_ROW.match, lines) if m}
    for workflow in sorted((ROOT / ".github" / "workflows").glob("*.y*ml")):
        if workflow.name not in inventory:
            errors.append(f".github/workflows/{workflow.name} is missing from the workflow inventory")
    for name in sorted(inventory):
        if not (ROOT / ".github" / "workflows" / name).exists():
            errors.append(f"workflow inventory lists {name}, which does not exist")

    print(f"merge base {base[:7]}: {len(changed)} files differ, {len(listed)} listed")
    for error in errors:
        print(f"error: {error}")
    sys.exit(1 if errors else 0)


if __name__ == "__main__":
    main()
