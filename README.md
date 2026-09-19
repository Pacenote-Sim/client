# client

The Pacenote client: the program a driver runs beside the simulator. It reads the game through a
source plugin, turns the samples into laps and corners, sends them to the team's server, and lets
the companions of the server's plugins post to their plugins and play what comes back.

A team's server builds it: the operator picks the plugins, the server compiles them into one
`pacenote.exe` with its own address stamped inside, and a driver installs that one file. This
repository is the app as a package plus a command that builds it with no plugins at all; the plugins
are modules of their own, built on `github.com/pacenote-sim/clientplugin`, and this module requires
none of them.

## What is here

| Package | |
|---|---|
| `app` | the program: the flags, the log, the runner, and with the `wails` tag the window. `app.Main` is what a build calls |
| `cmd/pacenote` | a build with no plugins: `app.Main` and nothing else. It is what this repository tests |
| `internal/stamp` | reads the server address the server wrote into the executable |
| `internal/config` | the device token, and each plugin's own settings kept for it, under the user's configuration directory |
| `internal/logx` | JSON lines to a rotating file; the token never reaches it |
| `internal/api` | the server's API, one method per route, tested against the server's own golden fixtures; the door a companion's requests go through |
| `internal/ids` | a UUIDv7 for a stint, a UUIDv4 for an idempotency key |
| `internal/queue` | writes the server has not accepted yet, on disk, in order, drained when it is back |
| `internal/lap` | samples in, laps out: line-to-line times interpolated at the crossing, the trace at full rate, sector times, the lap's kind; `Resample` by distance for the wire |
| `internal/corner` | the corners of a lap as it is driven, each measured; the last-corner report; which corner is approaching |
| `internal/stint` | where a stint begins and ends, the documents the server wants, the events the companions want |
| `internal/events` | one goroutine per companion; a slow companion delays only itself |
| `internal/host` | what the app lends a companion, and only that |
| `internal/recorder`, `internal/replay` | a session written to disk as it is captured; played back as if the simulator were running |
| `internal/runner` | the app: six loops under one context, and every decision on an error |
| `internal/desktop`, `internal/frontend` | the window's service and its one page |

## Building it

```
make build-window    # the window for this machine, no plugins, dist/pacenote-window
make build-windows   # pacenote.exe and pacenote-headless.exe, no plugins
make official        # both, with the official plugins from the checkouts beside this one
make build           # compile everything, without writing a binary
```

A client built from this repository alone reads no simulator: it has the demo circuit and a
recording and nothing else, because the plugins are not this module's to require. `make official`
builds one with them, the way a server does.

The Windows build needs no cgo, no C toolchain and no Windows machine: Wails 3, the audio and the
shared-memory readers are all plain Go. That is what lets a server build a driver's exe.

By hand, if you would rather:

```
go build -o pacenote ./cmd/pacenote                                  # headless, this machine
go build -tags wails -o pacenote ./cmd/pacenote                      # the window, this machine
GOOS=windows GOARCH=amd64 CGO_ENABLED=0 go build -tags wails \
  -ldflags "-s -w -H windowsgui" -o pacenote.exe ./cmd/pacenote      # the exe a driver runs
```

`-H windowsgui` is what stops a console window opening behind the app.

## Building it with the plugins you want

A plugin is a Go module that registers itself when it is imported. A build is a `main` package that
imports the plugins it wants and calls `app.Main`; nothing else about it varies. This is a build with
four of them:

```go
package main

import (
	"os"

	"github.com/pacenote-sim/client/app"

	_ "github.com/pacenote-sim/client-iracing"
	_ "github.com/pacenote-sim/client-engineer"
	_ "github.com/pacenote-sim/client-visual-telemetry"
	_ "github.com/pacenote-sim/client-voice"
)

func main() { os.Exit(app.Main(os.Args[1:], os.Stdout, os.Stderr)) }
```

To build a client with a different set — your own plugin, or fewer of the official ones — write that
file somewhere of your own:

```
mkdir my-client && cd my-client
cat > go.mod <<'EOF'
module my-client

go 1.26
EOF
cat > main.go <<'EOF'
package main

import (
	"os"

	"github.com/pacenote-sim/client/app"

	_ "github.com/pacenote-sim/client-iracing"
	_ "github.com/example/your-plugin"
)

func main() { os.Exit(app.Main(os.Args[1:], os.Stdout, os.Stderr)) }
EOF
go mod tidy
go build -tags wails -ldflags "-H windowsgui" -o pacenote.exe .
```

That is all the server does, in a directory of its own, with the plugins the operator ticked and the
versions it was asked for. Nothing is loaded at runtime: a client is one executable and what is in it
was chosen when it was built. `scripts/bundle.sh` does the same thing here, from the checkouts beside
this one, and `make official` is that script and two `go build` lines — the recipe above is the one
that is actually used, not a description of it.

A client plugin is a source (it reads a simulator) or a companion (it talks to a server plugin and
uses the app). Both are described, with an example of each to copy, in
[`clientplugin`](https://github.com/Pacenote-Sim/clientplugin). A companion's server half has to be
installed on the server as well, or the companion never starts: a plugin is two halves that find
each other through `GET /me`.

## Running it

```
pacenote                                   # the stamped server, the compiled-in sources
pacenote --server http://localhost:8080    # a client not built by a server
pacenote --demo --speed 30 --laps 2        # the demo circuit, fast, two laps, then stop
pacenote --replay session.jsonl.gz         # a recording, at the pace it was driven
pacenote --record                          # write every session to the data directory
```

The window (`make build-window` here, `pacenote.exe` for a driver) takes the same flags and shows
the same things: the pairing code while not paired, the driver, the car, the simulator, the track,
the lap, the last lap time, what is waiting to be sent, and the plugins running with their status
lines and pages. The client has no settings of its own: a plugin's controls are on its page, and
what it keeps is kept for it. Links on a plugin's page open in the machine's own browser.

Headless and not paired, it prints the code to enter in the server's panel and waits. Paired, it
prints one line whenever something changes: the driver, the source, the track, the lap, the last lap
time, what is waiting to be sent, which companions are running. The data directory holds
`config.json`, the `queue`, the `recordings` and `pacenote.log`; `--data` moves it.

Closing it finishes the job: the stint being driven is ended and its summary written, the queue gets
its last try, and a companion still uploading a lap is waited for. What will not go in ten seconds
stays on disk and goes out the next time it is opened.

## Testing it

`make` runs what CI runs: format, build, vet, lint, tests with the race detector, coverage over 90 %,
benchmarks once, `go mod tidy` a no-op, and the Windows exe cross-compiled. CI runs the tests on
Linux, Windows and macOS. `TESTING.md` has what the suite asserts, what a sample and a lap cost, and
how to drive a real telemetry file through it.

## Licence

GNU General Public License, version 3 — see `LICENSE`. The contract it is built on
(`github.com/pacenote-sim/clientplugin`) is Apache-2.0.
