// Package recorder writes a session to disk as it is captured, and reads one
// back. A recording is what the replay source plays: the tester's first real
// session becomes the fixture every later test runs on, and a support case
// can be replayed exactly as the driver saw it.
//
// The format is deliberately dull: gzip over JSON lines, one Sample per line,
// a header line first. Any language reads it.
package recorder

import (
	"bufio"
	"compress/gzip"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"time"

	"github.com/pacenote-sim/clientplugin"
)

// Extension is the file suffix.
const Extension = ".jsonl.gz"

// Format is the header's format number.
const Format = 1

// Header is the first line of a recording.
type Header struct {
	Recording int       `json:"pacenote_recording"`
	Sim       string    `json:"sim"`
	StartedAt time.Time `json:"started_at"`
	Version   string    `json:"client_version,omitempty"`
}

// Writer writes one recording.
type Writer struct {
	f  *os.File
	gz *gzip.Writer
	n  int
}

// FileName is where a recording of this simulator started at this time goes
// under dir.
func FileName(dir, sim string, started time.Time) string {
	return filepath.Join(dir, sim+"-"+started.UTC().Format("20060102-150405")+Extension)
}

// Create opens a new recording at path, creating the directory.
func Create(path string, h Header) (*Writer, error) {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return nil, fmt.Errorf("recorder: creating %s: %w", filepath.Dir(path), err)
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600) //nolint:gosec // G304: the path the app or the test chose.
	if err != nil {
		return nil, fmt.Errorf("recorder: %w", err)
	}
	w := &Writer{f: f, gz: gzip.NewWriter(f)}
	h.Recording = Format
	if err := w.line(h); err != nil {
		_ = f.Close()
		_ = os.Remove(path)
		return nil, err
	}
	return w, nil
}

// Write appends one sample. Writing to a closed recording is an error.
func (w *Writer) Write(s clientplugin.Sample) error {
	if err := w.line(s); err != nil {
		return err
	}
	w.n++
	return nil
}

// Samples is how many were written.
func (w *Writer) Samples() int { return w.n }

func (w *Writer) line(v any) error {
	if err := json.NewEncoder(w.gz).Encode(v); err != nil {
		return fmt.Errorf("recorder: writing: %w", err)
	}
	return nil
}

// Close flushes and closes the file. A recording that is not closed is still
// readable up to the last flushed block, which is the point of gzip's framing.
// Closing twice is an error.
func (w *Writer) Close() error {
	if err := errors.Join(w.gz.Close(), w.f.Close()); err != nil {
		return fmt.Errorf("recorder: closing: %w", err)
	}
	return nil
}

// Reader reads one recording.
type Reader struct {
	f      *os.File
	gz     *gzip.Reader
	sc     *bufio.Scanner
	Header Header
}

// ErrNotARecording reports a file that does not start with the header.
var ErrNotARecording = errors.New("recorder: not a Pacenote recording")

// MaxLineBytes bounds one line: a sample with a full field is a few kilobytes.
const MaxLineBytes = 1 << 20

// Open opens a recording and reads its header.
func Open(path string) (*Reader, error) {
	f, err := os.Open(path) //nolint:gosec // G304: the path the driver or the test chose.
	if err != nil {
		return nil, fmt.Errorf("recorder: %w", err)
	}
	gz, err := gzip.NewReader(f)
	if err != nil {
		_ = f.Close()
		return nil, fmt.Errorf("%w: %w", ErrNotARecording, err)
	}
	sc := bufio.NewScanner(gz)
	sc.Buffer(make([]byte, 0, 64<<10), MaxLineBytes)
	r := &Reader{f: f, gz: gz, sc: sc}
	if !sc.Scan() {
		_ = r.Close()
		return nil, fmt.Errorf("%w: empty", ErrNotARecording)
	}
	if err := json.Unmarshal(sc.Bytes(), &r.Header); err != nil || r.Header.Recording != Format {
		_ = r.Close()
		return nil, fmt.Errorf("%w: the header is not one", ErrNotARecording)
	}
	return r, nil
}

// Next reads the next sample, or io.EOF at the end.
func (r *Reader) Next() (clientplugin.Sample, error) {
	if !r.sc.Scan() {
		if err := r.sc.Err(); err != nil {
			return clientplugin.Sample{}, fmt.Errorf("recorder: reading: %w", err)
		}
		return clientplugin.Sample{}, io.EOF
	}
	var s clientplugin.Sample
	if err := json.Unmarshal(r.sc.Bytes(), &s); err != nil {
		return clientplugin.Sample{}, fmt.Errorf("recorder: a line is not a sample: %w", err)
	}
	return s, nil
}

// Close closes the file.
func (r *Reader) Close() error {
	if err := errors.Join(r.gz.Close(), r.f.Close()); err != nil {
		return fmt.Errorf("recorder: closing: %w", err)
	}
	return nil
}
