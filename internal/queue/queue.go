// Package queue keeps the writes the server has not accepted yet: a stint, a
// batch of laps, a summary. A driver's connection drops, the server restarts,
// a rate limit bites; the lap is on disk and goes when the server is back, in
// the order it happened, with the idempotency key it was first sent with, so
// that a retry can never store a lap twice.
//
// One file per entry, written whole and renamed into place, so that a crash
// mid-write costs at most the entry being written and never the queue. The
// file's name carries the sequence number, which is the order.
package queue

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/pacenote-sim/client/internal/api"
	"github.com/pacenote-sim/client/internal/ids"
)

// Kind is what an entry is.
type Kind string

// The kinds, which are also the routes.
const (
	KindStint   Kind = "stint"
	KindLaps    Kind = "laps"
	KindSummary Kind = "summary"
)

// Entry is one write waiting to be accepted.
type Entry struct {
	Seq       uint64          `json:"seq"`
	Kind      Kind            `json:"kind"`
	StintID   string          `json:"stint_id"`
	Key       string          `json:"key"`
	Body      json.RawMessage `json:"body"`
	CreatedAt time.Time       `json:"created_at"`
}

// Queue is the on-disk queue. It is safe for concurrent use: the capture
// goroutine pushes while the upload goroutine drains.
type Queue struct {
	dir string
	mu  sync.Mutex
	// Dropped is told about an entry the server refused for good, which is
	// removed rather than retried. Optional.
	Dropped func(Entry, error)

	next    uint64
	entries []Entry
}

// Open loads the queue in dir, creating it. Files it cannot read are renamed
// aside with a ".bad" suffix and reported, so that one damaged entry never
// stops the ones behind it.
func Open(dir string) (*Queue, error) {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, fmt.Errorf("queue: creating %s: %w", dir, err)
	}
	q := &Queue{dir: dir, next: 1}
	names, err := filepath.Glob(filepath.Join(dir, "*.json"))
	if err != nil {
		return nil, fmt.Errorf("queue: %w", err)
	}
	var bad []string
	for _, name := range names {
		raw, err := os.ReadFile(name) //nolint:gosec // G304: the queue's own directory.
		var e Entry
		if err == nil {
			err = json.Unmarshal(raw, &e)
		}
		if err != nil || e.Seq == 0 || e.Kind == "" {
			_ = os.Rename(name, name+".bad")
			bad = append(bad, filepath.Base(name))
			if n, ok := Seq(name); ok && n >= q.next {
				q.next = n + 1 // a damaged entry's number is not reused
			}
			continue
		}
		q.entries = append(q.entries, e)
		if e.Seq >= q.next {
			q.next = e.Seq + 1
		}
	}
	sort.Slice(q.entries, func(i, j int) bool { return q.entries[i].Seq < q.entries[j].Seq })
	if len(bad) > 0 {
		return q, fmt.Errorf("%w: %s", ErrDamaged, strings.Join(bad, ", "))
	}
	return q, nil
}

// ErrDamaged reports entries that could not be read at Open; the queue is
// usable and they were set aside.
var ErrDamaged = errors.New("queue: some entries could not be read and were set aside")

// Push adds a write. A summary replaces any summary already waiting for the
// same stint, since a summary is a whole document and only the latest counts.
func (q *Queue) Push(kind Kind, stintID string, body any) (Entry, error) {
	raw, err := json.Marshal(body)
	if err != nil {
		return Entry{}, fmt.Errorf("queue: encoding a %s: %w", kind, err)
	}
	q.mu.Lock()
	defer q.mu.Unlock()
	if kind == KindSummary {
		kept := q.entries[:0]
		for _, e := range q.entries {
			if e.Kind == KindSummary && e.StintID == stintID {
				_ = os.Remove(q.path(e.Seq))
				continue
			}
			kept = append(kept, e)
		}
		q.entries = kept
	}
	e := Entry{Seq: q.next, Kind: kind, StintID: stintID, Key: ids.Key(), Body: raw, CreatedAt: time.Now().UTC()}
	if err := q.write(e); err != nil {
		return Entry{}, err
	}
	q.next++
	q.entries = append(q.entries, e)
	return e, nil
}

// Len is how many writes are waiting.
func (q *Queue) Len() int {
	q.mu.Lock()
	defer q.mu.Unlock()
	return len(q.entries)
}

// Pending is what is waiting, in order. The caller's copy.
func (q *Queue) Pending() []Entry {
	q.mu.Lock()
	defer q.mu.Unlock()
	return append([]Entry(nil), q.entries...)
}

// Drain sends what is waiting, in order, until everything is gone or a send
// fails in a way that says "not now": unreachable, rate limited, a server
// error, or a token the server no longer accepts. It returns how many were
// accepted and that error. A send the server refuses for good — invalid,
// conflict — removes the entry, tells Dropped, and carries on: retrying it
// would fail the same way for ever and block every lap behind it.
func (q *Queue) Drain(ctx context.Context, send func(context.Context, Entry) error) (int, error) {
	sent := 0
	for {
		q.mu.Lock()
		if len(q.entries) == 0 {
			q.mu.Unlock()
			return sent, nil
		}
		e := q.entries[0]
		q.mu.Unlock()

		err := send(ctx, e)
		switch {
		case err == nil:
			q.remove(e.Seq)
			sent++
		case !final(err):
			return sent, err
		default:
			q.remove(e.Seq)
			if q.Dropped != nil {
				q.Dropped(e, err)
			}
		}
	}
}

// final reports that the server has refused this write for good.
func final(err error) bool {
	if errors.Is(err, api.ErrUnauthorized) || errors.Is(err, api.ErrTooOld) || errors.Is(err, api.ErrUnreachable) || errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return false
	}
	var e *api.Error
	if errors.As(err, &e) {
		return !e.Retryable()
	}
	return false
}

func (q *Queue) remove(seq uint64) {
	q.mu.Lock()
	defer q.mu.Unlock()
	_ = os.Remove(q.path(seq))
	for i, e := range q.entries {
		if e.Seq == seq {
			q.entries = append(q.entries[:i], q.entries[i+1:]...)
			break
		}
	}
}

func (q *Queue) path(seq uint64) string {
	return filepath.Join(q.dir, fmt.Sprintf("%012d.json", seq))
}

func (q *Queue) write(e Entry) error {
	raw, err := json.Marshal(e)
	if err != nil {
		return fmt.Errorf("queue: %w", err)
	}
	path := q.path(e.Seq)
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, raw, 0o600); err != nil {
		return fmt.Errorf("queue: writing: %w", err)
	}
	if err := os.Rename(tmp, path); err != nil {
		_ = os.Remove(tmp)
		return fmt.Errorf("queue: writing: %w", err)
	}
	return nil
}

// Seq parses a sequence number back out of a file name, for a test or a
// support case reading the directory.
func Seq(name string) (uint64, bool) {
	base := strings.TrimSuffix(filepath.Base(name), ".json")
	n, err := strconv.ParseUint(base, 10, 64)
	return n, err == nil
}
