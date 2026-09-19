package logx_test

import (
	"bytes"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/pacenote-sim/client/internal/logx"
)

func TestTheTokenNeverReachesTheFile(t *testing.T) {
	t.Parallel()
	r := require.New(t)

	var buf bytes.Buffer
	token := "tok-abcdef123456"
	l := logx.New(&buf, slog.LevelDebug, token, "short")
	l.Info("paired", slog.String("token", token))
	l.Warn("request failed", slog.String("url", "https://x/api?auth="+token))
	logx.WithPlugin(l, "engineer").Debug("posted")
	l.Info("a short word stays")
	out := buf.String()
	r.NotContains(out, token)
	r.Equal(2, strings.Count(out, "[token]"))
	r.Contains(out, `"plugin":"engineer"`)
	r.Contains(out, "short", "a secret under eight characters is not scrubbed: it would blank ordinary words")

	quiet := logx.New(&buf, slog.LevelWarn)
	before := buf.Len()
	quiet.Info("nothing")
	r.Equal(before, buf.Len())

	r.Equal(slog.LevelDebug, logx.Level(" debug "))
	r.Equal(slog.LevelWarn, logx.Level("WARN"))
	r.Equal(slog.LevelInfo, logx.Level("loud"))
	r.NotNil(logx.Discard())
}

func TestTheFileRollsOverBySize(t *testing.T) {
	t.Parallel()
	r := require.New(t)
	dir := filepath.Join(t.TempDir(), "Pacenote")

	w, err := logx.OpenRotating(dir, 200)
	r.NoError(err)
	line := []byte(strings.Repeat("x", 90) + "\n")
	for range 5 {
		_, err = w.Write(line)
		r.NoError(err)
	}
	r.NoError(w.Close())

	current, err := os.ReadFile(filepath.Join(dir, logx.FileName))
	r.NoError(err)
	previous, err := os.ReadFile(filepath.Join(dir, logx.FileName+".1"))
	r.NoError(err)
	r.Len(current, 91, "the fifth line started a new file")
	r.Len(previous, 2*91, "the previous file holds what fitted")

	// Reopening appends and remembers the size.
	w, err = logx.OpenRotating(dir, 200)
	r.NoError(err)
	_, err = w.Write(line)
	r.NoError(err)
	r.NoError(w.Close())
	current, err = os.ReadFile(filepath.Join(dir, logx.FileName))
	r.NoError(err)
	r.Len(current, 2*91)

	blocked := filepath.Join(t.TempDir(), "file")
	r.NoError(os.WriteFile(blocked, nil, 0o600))
	_, err = logx.OpenRotating(filepath.Join(blocked, "under"), 200)
	r.Error(err)

	// After Close, writing and closing again are errors and not panics.
	closed, err := logx.OpenRotating(t.TempDir(), 1<<20)
	r.NoError(err)
	r.NoError(closed.Close())
	_, err = closed.Write(line)
	r.Error(err)
	r.Error(closed.Close())

	// A writer whose destination fails reports it.
	failing := logx.New(failWriter{}, slog.LevelInfo)
	failing.Info("lost")
}

type failWriter struct{}

func (failWriter) Write([]byte) (int, error) { return 0, os.ErrClosed }
