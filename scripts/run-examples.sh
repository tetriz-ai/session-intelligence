#!/usr/bin/env bash
# Copyright 2026 Workloom Solutions Private Limited
# SPDX-License-Identifier: MIT
#
# Runs every example and compares its output to its README. CI and contributors use this same
# script. For each examples/<name>/README.md it extracts the first ```sh block (the command) and
# the last ```text block (the expected output), runs the command from the repo root, and diffs.
# It also fails if the index lists a folder that does not exist or misses one that does.
set -euo pipefail
cd "$(dirname "$0")/.."

go vet ./examples/... >/dev/null

dirs="$(find examples -mindepth 1 -maxdepth 1 -type d | sort)"
indexed="$(grep -oE '\]\(([a-z0-9-]+)/\)' examples/README.md | sed -E 's/\]\(([a-z0-9-]+)\/\)/\1/' | sort -u)"

# Cross-check the index against disk, both directions.
for d in $dirs; do
	name="${d#examples/}"
	if ! printf '%s\n' "$indexed" | grep -qx "$name"; then
		echo "example $name is not listed in examples/README.md" >&2
		exit 1
	fi
done
for name in $indexed; do
	if [ ! -d "examples/$name" ]; then
		echo "examples/README.md lists $name, which does not exist" >&2
		exit 1
	fi
done

extract_block() { # <file> <lang> <which: first|last>
	awk -v lang="$2" -v which="$3" '
		$0 == "```"lang { inblk=1; buf=""; next }
		inblk && $0 == "```" { inblk=0; last=buf; if (which=="first" && !seen) { print buf; seen=1 } next }
		inblk { buf = buf $0 "\n" }
		END { if (which=="last") printf "%s", last }
	' "$1"
}

fail=0
for d in $dirs; do
	name="${d#examples/}"
	readme="$d/README.md"
	cmd="$(extract_block "$readme" sh first)"
	want="$(extract_block "$readme" text last)"
	got="$(eval "$cmd" 2>/dev/null)"
	if [ "$got" != "$want" ]; then
		echo "FAIL $name: output does not match its README" >&2
		diff <(printf '%s' "$want") <(printf '%s' "$got") >&2 || true
		fail=1
	else
		echo "ok   $name"
	fi
done

# Also verify the root README's quickstart output blocks so a copy there cannot drift from
# the real example output. For each `go run ./examples/<name>` command shown in README.md, run
# it and diff against the ```text block that immediately follows.
root_expected() { # <name> -> the text block following that example's go-run command in README.md
	awk -v cmd="go run ./examples/$1" '
		$0 == "```sh" { insh=1; shbuf=""; next }
		insh && $0 == "```" { insh=0; if (shbuf ~ cmd) armed=1; next }
		insh { shbuf = shbuf $0 "\n"; next }
		armed && $0 == "```text" { intext=1; tbuf=""; next }
		intext && $0 == "```" { printf "%s", tbuf; exit }
		intext { tbuf = tbuf $0 "\n" }
	' README.md
}

root_names="$(grep -oE 'go run \./examples/[a-z0-9-]+' README.md | sed -E 's#.*/##' | sort -u)"
for name in $root_names; do
	[ -d "examples/$name" ] || continue
	want="$(root_expected "$name")"
	[ -n "$want" ] || continue
	got="$(eval "go run ./examples/$name" 2>/dev/null)"
	if [ "$got" != "$want" ]; then
		echo "FAIL root README: $name output does not match README.md" >&2
		diff <(printf '%s' "$want") <(printf '%s' "$got") >&2 || true
		fail=1
	else
		echo "ok   root README: $name"
	fi
done
exit "$fail"
