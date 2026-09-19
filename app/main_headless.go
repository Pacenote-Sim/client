//go:build !wails

// The headless build of this package: the same runner as the window, driven
// from the command line, for development, for a support case and for the
// server's own tests of the exe it builds. The window comes with the wails
// build tag, in main_wails.go.

package app

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/signal"
	"strings"
	"sync"
	"syscall"

	"github.com/pacenote-sim/protocol/wire"

	"github.com/pacenote-sim/client/internal/runner"
)

// Main runs the client and returns the process's exit code. args are the
// command line without the program's own name.
func Main(args []string, stdout, stderr io.Writer) int {
	set, code := parseFlags(args, stderr)
	if code != 0 {
		return code
	}
	file, log, err := openLog(set, stderr)
	if err != nil {
		fmt.Fprintln(stderr, err)
		return 1
	}
	defer func() { _ = file.Close() }()

	out := &printer{w: stdout}
	out.say("pacenote %s · server %s · data %s", set.version, set.address, set.dataDir)
	r, err := runner.New(runner.Config{
		Version: set.version, DataDir: set.dataDir, Address: set.address, Sources: set.sources(), Record: set.record, StopAfterLaps: set.laps, Log: log,
		OnPairing: func(p wire.PairStart) {
			out.say("\nNot paired yet. Open %s and enter the code  %s\n", p.VerificationURI, p.UserCode)
		},
		OnStatus: out.status,
	})
	if err != nil {
		fmt.Fprintln(stderr, err)
		return 1
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	switch err := r.Run(ctx); {
	case err == nil:
		out.say("done")
		return 0
	case errors.Is(err, runner.ErrUnpaired):
		out.say("this client is no longer paired; run it again to pair")
		return 3
	case errors.Is(err, context.Canceled):
		out.say("stopped")
		return 0
	default:
		fmt.Fprintln(stderr, err)
		return 1
	}
}

// printer is what the run prints through. The status arrives from whichever
// of the runner's goroutines noticed the change, and the rest from the one
// that started the run, so both the writer and the line last printed are held
// while they are used: a status that has not changed is not printed twice, and
// two goroutines never write a line each into the same one.
type printer struct {
	mu   sync.Mutex
	w    io.Writer
	last string
}

// status prints a status line when it says something the last one did not.
func (p *printer) status(s runner.Status) {
	line := statusLine(s)
	p.mu.Lock()
	defer p.mu.Unlock()
	if line == p.last {
		return
	}
	p.last = line
	fmt.Fprintln(p.w, line)
}

// say prints one line of this program's own.
func (p *printer) say(format string, args ...any) {
	p.mu.Lock()
	defer p.mu.Unlock()
	fmt.Fprintf(p.w, format+"\n", args...)
}

func statusLine(s runner.Status) string {
	var b strings.Builder
	switch {
	case !s.Paired:
		b.WriteString("not paired")
	default:
		b.WriteString(s.Driver)
	}
	if s.Source != "" {
		b.WriteString(" · " + s.Source)
	}
	if s.Track != "" {
		b.WriteString(" · " + s.Track)
	}
	if s.Stint != "" {
		fmt.Fprintf(&b, " · lap %d", s.Lap)
	}
	if s.LastLapMs > 0 {
		fmt.Fprintf(&b, " · last %s", lapTime(s.LastLapMs))
	}
	if s.Queued > 0 {
		fmt.Fprintf(&b, " · %d waiting", s.Queued)
	}
	if len(s.Companions) > 0 {
		b.WriteString(" · " + strings.Join(s.Companions, ", "))
	}
	if s.Message != "" {
		b.WriteString(" · " + s.Message)
	}
	return b.String()
}

func lapTime(ms int) string {
	return fmt.Sprintf("%d:%02d.%03d", ms/60000, ms%60000/1000, ms%1000)
}
