# Testing the client

`make` runs what CI runs, in order: format, build for this machine and for Windows, vet both builds,
lint both builds, the suite with the race detector and shuffled order, coverage over 90 % in every
package, every benchmark once, and `go mod tidy` a no-op. CI runs the suite on Linux, Windows and
macOS, and cross-compiles the Windows exe on each.

```
make            # everything, in order
make test       # the suite alone
make cover      # the suite with the coverage floor
make bench      # the benchmarks, properly
make demo       # drive the demo circuit through the lap and corner code, fast
```

## What the suite asserts

| Package | |
|---|---|
| `app` | the program from its command line to its exit code: two laps of the demo against a stand-in server, a recording made and played back, pairing and the token kept, a token the server has forgotten, and what each flag decides |
| `internal/api` | every route against the server's own golden fixtures, copied into `testdata/api-v1`: if the server changes a shape, this fails |
| `internal/lap` | a lap from samples: the crossing interpolated, the kinds, the sectors, the thinning |
| `internal/corner` | corners found as they are driven, from traces built for the purpose: braking, trail braking, an early apex, a corner taken without brakes, and the cue for the corner ahead |
| `internal/stint` | a stint from the demo circuit end to end: what is uploaded, what the companions are told, and when |
| `internal/queue` | an outage: nothing lost, the order kept, a damaged entry set aside rather than retried for ever |
| `internal/events` | a slow companion delays only itself; events queued at a stop are delivered before it |
| `internal/runner` | the whole app against a stand-in server: pairing, an outage and the drain after it, a forgotten token, closing mid-stint, and what a companion may and may not do |
| `internal/stamp` | a real executable is built and scanned: exactly one blank region, and what is written into it reads back |
| `internal/desktop` | what the window's page may ask for, including which addresses it may open |

The stand-in servers answer from the protocol's own types. Nothing in the suite needs a network, a
simulator, a database or a browser.

## What a sample and a lap cost

The client's one hot path is the sample: a simulator publishes sixty a second and every one goes
through the tracker, the lap accumulator, the corner detector and the bus. On an Apple M5:

| | |
|---|---|
| one sample through the tracker | 343 ns, 2 allocations |
| one point through the corner detector | 12 ns, no allocations |
| one sample published to one companion | 55 ns, no allocations |
| one sample published to four | 101 ns, no allocations |
| one sample nobody asked for | 9 ns, no allocations |
| a whole lap, 2 400 samples, including the lap it closes | 564 µs |
| thinning a 6 000-point lap to the 300 a server takes | 16 µs, one allocation |

At sixty samples a second that is about 20 µs of work a second, and the allocations per sample are
the event a sample produces. The numbers that matter are the allocations: a sample that allocates
does it sixty times a second for as long as the car is on track.

`make bench` runs them properly; `make bench-smoke` runs each once, which is what CI does, so a
benchmark that stops compiling is noticed the day it happens rather than the day somebody needs it.

## Building one with plugins in it

A client built from this repository has no plugins: this module requires none, and `cmd/pacenote`
imports none. To test the whole product, build one with them:

```
make official        # dist/pacenote-window and dist/pacenote.exe, with the official four
```

which writes `dist/build/` — the `main.go` and `go.mod` of a build — and compiles it. That is the
same thing a server generates for a team, from the same recipe, so the instructions in the README
are the ones that are used rather than a description of them. `make official OFFICIAL="client-iracing"`
builds a client with one plugin, which is how a plugin is tested on its own.

## Driving a real telemetry file through it

The demo circuit is a made-up lap. To run the client against a real one, use an iRacing `.ibt` file
through the iRacing plugin, which plays it as if the game were running:

```
make official
PACENOTE_IRACING_IBT=/path/to/session.ibt \
  ./dist/pacenote-window --server http://localhost:8080 --data ./try/client-data
```

It plays at the pace it was driven; `PACENOTE_IRACING_SPEED=3` is quicker, at the cost of the audio
falling behind, which is real and not a fault. `client-iracing`'s own `cmd/ibt-dump` prints what is
in a file when something reads wrongly.

## Looking at the window

```
make build-window   # no plugins: the pairing card, the status, and no simulator
make official       # the four, which is what a driver's window looks like
./dist/pacenote-window --server http://localhost:8080 --data ./try/client-data
```

The window is a view: every decision is the runner's, so anything it shows wrongly is either the
page (`internal/frontend/index.html`, one file) or the service that feeds it
(`internal/desktop`), and both are tested without a browser.
