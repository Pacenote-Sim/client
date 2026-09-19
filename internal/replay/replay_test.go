package replay_test

import (
	"context"
	"io"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/pacenote-sim/clientplugin"
	"github.com/pacenote-sim/clientplugin/clientplugintest"

	"github.com/pacenote-sim/client/internal/recorder"
	"github.com/pacenote-sim/client/internal/replay"
)

func TestARecordingPlaysBackAsItWasCaptured(t *testing.T) {
	t.Parallel()
	r := require.New(t)
	ctx := context.Background()
	dir := t.TempDir()
	t0 := time.Date(2026, 9, 18, 10, 0, 0, 0, time.UTC)

	path := recorder.FileName(dir, "iracing", t0)
	r.Equal(filepath.Join(dir, "iracing-20260918-100000.jsonl.gz"), path)
	w, err := recorder.Create(path, recorder.Header{Sim: "iracing", StartedAt: t0, Version: "1.0.0"})
	r.NoError(err)
	for i := range 100 {
		r.NoError(w.Write(clientplugin.Sample{At: t0.Add(time.Duration(i) * 50 * time.Millisecond), Lap: 1, SpeedKmh: float64(100 + i), Track: "Spa"}))
	}
	r.Equal(100, w.Samples())
	r.NoError(w.Close())
	_, err = recorder.Create(path, recorder.Header{})
	r.Error(err, "a recording is never overwritten")

	rd, err := recorder.Open(path)
	r.NoError(err)
	r.Equal("iracing", rd.Header.Sim)
	r.Equal(recorder.Format, rd.Header.Recording)
	first, err := rd.Next()
	r.NoError(err)
	r.InDelta(100.0, first.SpeedKmh, 0)
	r.Equal("Spa", first.Track)
	r.NoError(rd.Close())

	// Through the source, at 10x, the waits are the recorded gaps divided by ten.
	src := replay.New(path, 10)
	var waited time.Duration
	src.Sleep = func(_ context.Context, d time.Duration) error { waited += d; return nil }
	r.Equal("replay", src.Name(), "before opening, the source has no simulator")
	r.True(src.Running())
	got, err := clientplugintest.Drain(ctx, src, 100)
	r.NoError(err)
	r.Len(got, 100)
	r.Equal(t0, got[0].At, "the recorded clock is kept: lap times come from it")
	r.InDelta(float64(99*5*time.Millisecond), float64(waited), float64(time.Millisecond))

	// Opened, it is filed under the recorded simulator; at the end it is EOF,
	// like a simulator that closed.
	r.NoError(src.Open(ctx))
	r.Equal("iracing", src.Name())
	for range 100 {
		_, err = src.Read(ctx)
		r.NoError(err)
	}
	_, err = src.Read(ctx)
	r.ErrorIs(err, io.EOF)
	r.NoError(src.Close())
	r.NoError(src.Close(), "closing twice is nothing")
	_, err = src.Read(ctx)
	r.EqualError(err, "replay: read before open")

	// Real time: the sleep is the real gap, and a cancelled context ends it.
	live := replay.New(path, 1)
	cancelled, cancel := context.WithCancel(ctx)
	r.NoError(live.Open(cancelled))
	_, err = live.Read(cancelled)
	r.NoError(err, "the first sample waits for nothing")
	cancel()
	_, err = live.Read(cancelled)
	r.ErrorIs(err, context.Canceled)
	r.NoError(live.Close())

	// Two samples at the same instant wait for nothing between them.
	same := filepath.Join(dir, "same.jsonl.gz")
	sw, err := recorder.Create(same, recorder.Header{Sim: "x"})
	r.NoError(err)
	r.NoError(sw.Write(clientplugin.Sample{At: t0}))
	r.NoError(sw.Write(clientplugin.Sample{At: t0}))
	r.NoError(sw.Close())
	both, err := clientplugintest.Drain(ctx, replay.New(same, 1), 2)
	r.NoError(err)
	r.Len(both, 2)
}

func TestWhatIsNotARecording(t *testing.T) {
	t.Parallel()
	r := require.New(t)
	dir := t.TempDir()

	plain := filepath.Join(dir, "plain.txt")
	r.NoError(os.WriteFile(plain, []byte("hello"), 0o600))
	src := replay.New(plain, 1)
	r.ErrorIs(src.Open(context.Background()), recorder.ErrNotARecording)
	r.False(replay.New("", 1).Running(), "no file, nothing to play")
	r.NoError(src.Close(), "closing what never opened is nothing")
}
