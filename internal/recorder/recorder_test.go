package recorder_test

import (
	"compress/gzip"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/pacenote-sim/clientplugin"

	"github.com/pacenote-sim/client/internal/recorder"
)

func TestARecordingIsWrittenAndReadBack(t *testing.T) {
	t.Parallel()
	r := require.New(t)
	dir := t.TempDir()
	t0 := time.Date(2026, 9, 18, 10, 0, 0, 0, time.UTC)

	path := recorder.FileName(filepath.Join(dir, "recordings"), "iracing", t0)
	r.Equal("iracing-20260918-100000.jsonl.gz", filepath.Base(path))
	w, err := recorder.Create(path, recorder.Header{Sim: "iracing", StartedAt: t0, Version: "1.0.0"})
	r.NoError(err, "the directory is created")
	for i := range 50 {
		r.NoError(w.Write(clientplugin.Sample{At: t0.Add(time.Duration(i) * time.Second), Lap: 1 + i/25, Track: "Spa", SpeedKmh: float64(i)}))
	}
	r.Equal(50, w.Samples())
	r.NoError(w.Close())
	_, err = recorder.Create(path, recorder.Header{})
	r.Error(err, "a recording is never overwritten")

	rd, err := recorder.Open(path)
	r.NoError(err)
	r.Equal(recorder.Header{Recording: 1, Sim: "iracing", StartedAt: t0, Version: "1.0.0"}, rd.Header)
	n := 0
	for {
		var s clientplugin.Sample
		s, err = rd.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		r.NoError(err)
		r.Equal("Spa", s.Track)
		r.InDelta(float64(n), s.SpeedKmh, 0)
		n++
	}
	r.Equal(50, n)
	r.NoError(rd.Close())
	r.Error(rd.Close(), "closing twice is an error, not a panic")

	// After Close, a Write is an error, and so is another Close.
	closed, err := recorder.Create(filepath.Join(dir, "closed.jsonl.gz"), recorder.Header{Sim: "x"})
	r.NoError(err)
	r.NoError(closed.Close())
	r.ErrorContains(closed.Write(clientplugin.Sample{}), "writing")
	r.Error(closed.Close())
}

func TestWhatIsNotARecording(t *testing.T) {
	t.Parallel()
	r := require.New(t)
	dir := t.TempDir()

	_, err := recorder.Open(filepath.Join(dir, "missing"))
	r.Error(err)

	plain := filepath.Join(dir, "plain.txt")
	r.NoError(os.WriteFile(plain, []byte("hello"), 0o600))
	_, err = recorder.Open(plain)
	r.ErrorIs(err, recorder.ErrNotARecording, "not gzip")

	gz := func(name string, lines ...string) string {
		p := filepath.Join(dir, name)
		f, ferr := os.Create(p)
		r.NoError(ferr)
		zw := gzip.NewWriter(f)
		for _, l := range lines {
			_, werr := zw.Write([]byte(l + "\n"))
			r.NoError(werr)
		}
		r.NoError(zw.Close())
		r.NoError(f.Close())
		return p
	}
	_, err = recorder.Open(gz("empty.jsonl.gz"))
	r.ErrorIs(err, recorder.ErrNotARecording, "gzip of nothing")
	_, err = recorder.Open(gz("other.jsonl.gz", `{"something":"else"}`))
	r.ErrorIs(err, recorder.ErrNotARecording, "a header that is not one")
	_, err = recorder.Open(gz("old.jsonl.gz", `{"pacenote_recording":9}`))
	r.ErrorIs(err, recorder.ErrNotARecording, "a format from another time")

	rd, err := recorder.Open(gz("badline.jsonl.gz", `{"pacenote_recording":1,"sim":"x"}`, `not a sample`))
	r.NoError(err)
	_, err = rd.Next()
	r.ErrorContains(err, "not a sample")
	r.NoError(rd.Close())

	long, err := recorder.Open(gz("long.jsonl.gz", `{"pacenote_recording":1,"sim":"x"}`, strings.Repeat("x", recorder.MaxLineBytes+1)))
	r.NoError(err)
	_, err = long.Next()
	r.ErrorContains(err, "reading", "a line past the limit is a read error, not a hang")
	r.NoError(long.Close())

	blocked := filepath.Join(dir, "file")
	r.NoError(os.WriteFile(blocked, nil, 0o600))
	_, err = recorder.Create(filepath.Join(blocked, "under", "x.jsonl.gz"), recorder.Header{})
	r.Error(err)
}
