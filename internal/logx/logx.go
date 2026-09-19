// Package logx is the client's log: JSON lines to a file under the
// configuration directory, rotated by size, with the device token never
// written whatever a caller logs. A desktop program built without a console
// has no stdout; a support question needs a file.
package logx

import (
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync"
)

// FileName is the log file; the previous one is FileName + ".1".
const FileName = "pacenote.log"

// MaxBytes is where the file rolls over. Ten megabytes is weeks of a client's
// own lines.
const MaxBytes = 10 << 20

// Rotating is an io.Writer over a file that is renamed to ".1" and started
// again when it passes a size. One writer per file; it is safe for concurrent
// use, as slog requires.
type Rotating struct {
	mu   sync.Mutex
	path string
	max  int64
	size int64
	f    *os.File
}

// OpenRotating opens the log file in dir, creating the directory.
func OpenRotating(dir string, maxBytes int64) (*Rotating, error) {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, fmt.Errorf("logx: creating %s: %w", dir, err)
	}
	r := &Rotating{path: filepath.Join(dir, FileName), max: maxBytes}
	if err := r.open(); err != nil {
		return nil, err
	}
	return r, nil
}

func (r *Rotating) open() error {
	f, err := os.OpenFile(r.path, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		return fmt.Errorf("logx: opening %s: %w", r.path, err)
	}
	info, err := f.Stat()
	if err != nil {
		_ = f.Close()
		return fmt.Errorf("logx: %w", err)
	}
	r.f, r.size = f, info.Size()
	return nil
}

// Write implements io.Writer.
func (r *Rotating) Write(p []byte) (int, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.size+int64(len(p)) > r.max && r.size > 0 {
		_ = r.f.Close()
		_ = os.Rename(r.path, r.path+".1")
		if err := r.open(); err != nil {
			return 0, err
		}
	}
	n, err := r.f.Write(p)
	r.size += int64(n)
	if err != nil {
		return n, fmt.Errorf("logx: %w", err)
	}
	return n, nil
}

// Close closes the file.
func (r *Rotating) Close() error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if err := r.f.Close(); err != nil {
		return fmt.Errorf("logx: %w", err)
	}
	return nil
}

// New is a JSON logger to w at the given level, with every occurrence of each
// secret replaced before it is written. The token is the one secret a client
// holds; nothing that logs, ours or a plugin's, can leak it.
func New(w io.Writer, level slog.Level, secrets ...string) *slog.Logger {
	var kept []string
	for _, s := range secrets {
		if len(s) >= 8 {
			kept = append(kept, s)
		}
	}
	h := slog.NewJSONHandler(&scrubber{w: w, secrets: kept}, &slog.HandlerOptions{Level: level})
	return slog.New(h)
}

// scrubber replaces secrets in the bytes a handler writes. It works on the
// rendered line rather than on attributes so that a secret inside a longer
// string — a URL, an error message — is caught too.
type scrubber struct {
	w       io.Writer
	secrets []string
}

func (s *scrubber) Write(p []byte) (int, error) {
	line := string(p)
	for _, secret := range s.secrets {
		line = strings.ReplaceAll(line, secret, "[token]")
	}
	if _, err := s.w.Write([]byte(line)); err != nil {
		return 0, err //nolint:wrapcheck // the writer's own error is the error.
	}
	return len(p), nil
}

// Level reads a level from a word, for a setting or an environment variable:
// debug, info, warn, error. Anything else is info.
func Level(word string) slog.Level {
	var l slog.Level
	if err := l.UnmarshalText([]byte(strings.ToUpper(strings.TrimSpace(word)))); err != nil {
		return slog.LevelInfo
	}
	return l
}

// Discard is a logger that writes nothing.
func Discard() *slog.Logger { return slog.New(slog.DiscardHandler) }

// WithPlugin is l with the plugin's name on every line.
func WithPlugin(l *slog.Logger, name string) *slog.Logger { return l.With(slog.String("plugin", name)) }
