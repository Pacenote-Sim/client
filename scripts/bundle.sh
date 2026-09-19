#!/usr/bin/env bash
# Write a client build: a main package that imports the plugins it was given
# and calls app.Main.
#
#   scripts/bundle.sh <dir> [plugin...]
#
# That is all a build is, and it is what a team's server generates from the
# plugins an operator ticked. The difference here is where the modules come
# from: a development tree has them checked out beside this one, so they are
# pointed at with replace directives; a server takes them from the module
# proxy at the version it was asked for, and writes no replace at all.
#
# The client module requires no plugin. A build takes what it asks for and
# nothing else, which is why this script exists rather than a list of imports
# in the repository's own command.
set -euo pipefail

dir=${1:?usage: bundle.sh <dir> [plugin...]}
shift
root=$(cd "$(dirname "$0")/.." && pwd)
siblings=$(cd "$root/.." && pwd)

rm -rf "$dir"
mkdir -p "$dir"

{
	echo "module pacenote-client-build"
	echo
	echo "go 1.26"
	echo
	echo "require github.com/pacenote-sim/client v0.0.0"
	for p in "$@"; do echo "require github.com/pacenote-sim/$p v0.0.0"; done
	echo
	echo "replace github.com/pacenote-sim/client => $root"
	echo "replace github.com/pacenote-sim/clientplugin => $siblings/clientplugin"
	for p in "$@"; do echo "replace github.com/pacenote-sim/$p => $siblings/$p"; done
} >"$dir/go.mod"

{
	echo "// Written by scripts/bundle.sh. A client is its plugins and this one call."
	echo "package main"
	echo
	echo "import ("
	echo '	"os"'
	echo
	echo '	"github.com/pacenote-sim/client/app"'
	if [ "$#" -gt 0 ]; then
		echo
		# Sorted, because a Go file's imports are.
		for p in $(printf '%s\n' "$@" | LC_ALL=C sort); do echo "	_ \"github.com/pacenote-sim/$p\""; done
	fi
	echo ")"
	echo
	echo "func main() { os.Exit(app.Main(os.Args[1:], os.Stdout, os.Stderr)) }"
} >"$dir/main.go"

cd "$dir"
GOWORK=off go mod tidy
