// Package app is the Pacenote client as a program: the flags, the log, the
// runner and, with the wails build tag, the window. It is a package rather
// than a command so that a build can choose its own plugins — a plugin
// registers itself when it is imported, and a command that imports some and
// calls [Main] is a client with those plugins in it.
//
// That is all a build is:
//
//	package main
//
//	import (
//		"os"
//
//		"github.com/pacenote-sim/client/app"
//
//		_ "github.com/pacenote-sim/client-iracing"
//		_ "github.com/pacenote-sim/client-engineer"
//	)
//
//	func main() { os.Exit(app.Main(os.Args[1:], os.Stdout, os.Stderr)) }
//
// The server builds a driver's exe exactly this way, from the plugins the
// operator ticked. The command in this repository does it with the official
// ones, for development.
//
//	pacenote                       run against the stamped server with the compiled-in sources
//	pacenote --demo --laps 3       drive the demo circuit, three laps, then stop
//	pacenote --replay f.jsonl.gz   play a recording as if the simulator were running
//	pacenote --record              write every session to the data directory
package app

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"runtime/debug"
	"strings"
	"time"

	"github.com/pacenote-sim/clientplugin"
	demo "github.com/pacenote-sim/clientplugin/examples/source-demo"

	"github.com/pacenote-sim/client/internal/config"
	"github.com/pacenote-sim/client/internal/logx"
	"github.com/pacenote-sim/client/internal/replay"
	"github.com/pacenote-sim/client/internal/stamp"
)

// settings is what the flags and the stamp decide.
type settings struct {
	version    string
	address    string
	dataDir    string
	demo       bool
	replayPath string
	speed      float64
	laps       int
	record     bool
	logLevel   string
}

// parseFlags reads the command line. A non-zero code is the exit code.
func parseFlags(args []string, stderr io.Writer) (settings, int) {
	fs := flag.NewFlagSet("pacenote", flag.ContinueOnError)
	fs.SetOutput(stderr)
	server := fs.String("server", "", "the server, when this client was not built by one")
	dataDir := fs.String("data", "", "where the token, the queue and the recordings live (default: the user's configuration directory)")
	useDemo := fs.Bool("demo", false, "drive the demo circuit instead of a simulator")
	replayPath := fs.String("replay", "", "play this recording instead of a simulator")
	speed := fs.Float64("speed", 1, "how many times faster than real time the demo or the replay runs")
	laps := fs.Int("laps", 0, "stop after this many completed laps (0: run until stopped)")
	record := fs.Bool("record", false, "write every session to the data directory")
	level := fs.String("log", "info", "log level: debug, info, warn, error")
	if err := fs.Parse(args); err != nil {
		return settings{}, 2
	}
	set := settings{version: buildVersion(), demo: *useDemo, replayPath: *replayPath, speed: *speed, laps: *laps, record: *record, logLevel: *level}
	var err error
	if set.address, err = serverAddress(*server); err != nil {
		fmt.Fprintln(stderr, err)
		return settings{}, 2
	}
	set.dataDir = *dataDir
	if set.dataDir == "" {
		if set.dataDir, err = config.DefaultDir(); err != nil {
			fmt.Fprintln(stderr, err)
			return settings{}, 1
		}
	}
	return set, 0
}

// openLog opens the log file and returns a logger that writes to it and to w,
// with the token scrubbed.
func openLog(set settings, w io.Writer) (*logx.Rotating, *slog.Logger, error) {
	file, err := logx.OpenRotating(set.dataDir, logx.MaxBytes)
	if err != nil {
		return nil, nil, err
	}
	cfg, _ := config.Load(set.dataDir)
	return file, logx.New(io.MultiWriter(file, w), logx.Level(set.logLevel), cfg.Token), nil
}

// sources are the explicit sources the flags asked for: a replay or the demo.
func (s settings) sources() []clientplugin.Source {
	switch {
	case s.replayPath != "":
		return []clientplugin.Source{replay.New(s.replayPath, s.speed)}
	case s.demo:
		return []clientplugin.Source{demoSource(s.speed)}
	}
	return nil
}

// serverAddress is the stamped address, or the flag when the exe was not
// built by a server. A stamped client ignores the flag: it belongs to a team.
func serverAddress(flagValue string) (string, error) {
	return addressFrom(stamp.Read, flagValue)
}

// addressFrom is serverAddress with the stamp handed to it, so that the three
// answers can be told apart without building three executables.
func addressFrom(read func() (stamp.Stamp, error), flagValue string) (string, error) {
	s, err := read()
	switch {
	case err == nil:
		return s.Address, nil
	case errors.Is(err, stamp.ErrBlank) && flagValue != "":
		return flagValue, nil
	case errors.Is(err, stamp.ErrBlank):
		return "", errors.New("this client was not built by a server; give one with --server https://team.example")
	default:
		return "", err
	}
}

func buildVersion() string {
	if info, ok := debug.ReadBuildInfo(); ok && info.Main.Version != "" && info.Main.Version != "(devel)" {
		return strings.TrimPrefix(info.Main.Version, "v")
	}
	return "1.0.0-dev"
}

// demoSource is the demo circuit at 60 s laps, sped up.
func demoSource(speed float64) clientplugin.Source {
	src := demo.New()
	src.Enabled = true
	src.LapSeconds = 60
	clock := time.Now()
	src.Sleep = func(ctx context.Context, d time.Duration) error {
		clock = clock.Add(d)
		wait := d
		if speed > 1 {
			wait = time.Duration(float64(d) / speed)
		}
		t := time.NewTimer(wait)
		defer t.Stop()
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-t.C:
			return nil
		}
	}
	demo.SetNow(src, func() time.Time { return clock })
	return src
}
