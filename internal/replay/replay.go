// Package replay is a source that plays a recording back: the samples a real
// simulator produced, at the pace they came or faster. It is how the client is
// developed and tested on a machine with no simulator, and how a support case
// is reproduced.
//
// It is a source like any other and registers under the name "replay" only
// when a file is given: a built client carries the code, but nothing plays
// unless asked.
package replay

import (
	"context"
	"fmt"
	"time"

	"github.com/pacenote-sim/clientplugin"

	"github.com/pacenote-sim/client/internal/recorder"
)

// Source plays one recording.
type Source struct {
	// Path is the recording. Speed is how many times faster than real time;
	// 0 or 1 is real time, and a very large value plays as fast as it reads.
	Path  string
	Speed float64
	// Sleep waits; a test replaces it.
	Sleep func(context.Context, time.Duration) error

	r    *recorder.Reader
	last time.Time
}

// New is a replay of path at speed.
func New(path string, speed float64) *Source {
	return &Source{Path: path, Speed: speed, Sleep: sleep}
}

// Name implements [clientplugin.Source]. It is the recorded simulator's name,
// once the file is open, so that a replayed stint is filed under the right
// simulator; "replay" before.
func (s *Source) Name() string {
	if s.r != nil && s.r.Header.Sim != "" {
		return s.r.Header.Sim
	}
	return "replay"
}

// Running implements [clientplugin.Source]: a recording is always ready.
func (s *Source) Running() bool { return s.Path != "" }

// Open implements [clientplugin.Source].
func (s *Source) Open(context.Context) error {
	r, err := recorder.Open(s.Path)
	if err != nil {
		return fmt.Errorf("replay: %w", err)
	}
	s.r, s.last = r, time.Time{}
	return nil
}

// Read implements [clientplugin.Source]: the next sample, after waiting the
// recorded gap divided by the speed. io.EOF at the end, as a simulator that
// closed would be.
func (s *Source) Read(ctx context.Context) (clientplugin.Sample, error) {
	if s.r == nil {
		return clientplugin.Sample{}, errNotOpen
	}
	sample, err := s.r.Next()
	if err != nil {
		return clientplugin.Sample{}, err
	}
	if !s.last.IsZero() && sample.At.After(s.last) {
		gap := sample.At.Sub(s.last)
		if speed := s.Speed; speed > 1 {
			gap = time.Duration(float64(gap) / speed)
		}
		if err := s.Sleep(ctx, gap); err != nil {
			return clientplugin.Sample{}, err
		}
	}
	s.last = sample.At
	return sample, nil
}

// Close implements [clientplugin.Source].
func (s *Source) Close() error {
	if s.r == nil {
		return nil
	}
	err := s.r.Close()
	s.r = nil
	if err != nil {
		return fmt.Errorf("replay: %w", err)
	}
	return nil
}

type notOpenError struct{}

func (notOpenError) Error() string { return "replay: read before open" }

var errNotOpen = notOpenError{}

func sleep(ctx context.Context, d time.Duration) error {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err() //nolint:wrapcheck // the context's own reason is the reason.
	case <-t.C:
		return nil
	}
}
