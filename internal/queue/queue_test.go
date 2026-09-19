package queue_test

import (
	"context"
	"errors"
	"net/http"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/pacenote-sim/protocol/wire"

	"github.com/pacenote-sim/client/internal/api"
	"github.com/pacenote-sim/client/internal/queue"
)

func TestWritesWaitInOrderAndSurviveARestart(t *testing.T) {
	t.Parallel()
	r := require.New(t)
	dir := filepath.Join(t.TempDir(), "queue")

	q, err := queue.Open(dir)
	r.NoError(err)
	r.Equal(0, q.Len())

	s, err := q.Push(queue.KindStint, "s-1", map[string]any{"sim": "iracing"})
	r.NoError(err)
	l1, err := q.Push(queue.KindLaps, "s-1", map[string]any{"laps": []int{1}})
	r.NoError(err)
	sum1, err := q.Push(queue.KindSummary, "s-1", map[string]any{"laps": 1})
	r.NoError(err)
	l2, err := q.Push(queue.KindLaps, "s-1", map[string]any{"laps": []int{2}})
	r.NoError(err)
	sum2, err := q.Push(queue.KindSummary, "s-1", map[string]any{"laps": 2})
	r.NoError(err)
	other, err := q.Push(queue.KindSummary, "s-2", map[string]any{"laps": 9})
	r.NoError(err)
	r.NotEqual(s.Key, l1.Key, "every write has its own key")
	r.Less(s.Seq, l1.Seq)

	pending := q.Pending()
	r.Len(pending, 5, "the first summary was replaced by the second")
	r.Equal([]uint64{s.Seq, l1.Seq, l2.Seq, sum2.Seq, other.Seq}, seqs(pending))
	_, err = os.Stat(filepath.Join(dir, "000000000003.json"))
	r.ErrorIs(err, os.ErrNotExist, "the replaced summary's file is gone")
	r.Equal(uint64(3), sum1.Seq)

	// A restart reads the same queue back, in order, and numbers on from there.
	again, err := queue.Open(dir)
	r.NoError(err)
	r.Equal(seqs(pending), seqs(again.Pending()))
	next, err := again.Push(queue.KindLaps, "s-2", map[string]any{})
	r.NoError(err)
	r.Equal(other.Seq+1, next.Seq)
	r.JSONEq(`{"sim":"iracing"}`, string(again.Pending()[0].Body))

	n, ok := queue.Seq(filepath.Join(dir, "000000000007.json"))
	r.True(ok)
	r.Equal(uint64(7), n)
	_, ok = queue.Seq("nope.json")
	r.False(ok)
}

func TestDrainStopsOnNotNowAndDropsOnNever(t *testing.T) {
	t.Parallel()
	r := require.New(t)
	q, err := queue.Open(t.TempDir())
	r.NoError(err)
	var dropped []queue.Entry
	q.Dropped = func(e queue.Entry, _ error) { dropped = append(dropped, e) }
	for i := range 5 {
		_, err = q.Push(queue.KindLaps, "s-1", map[string]int{"lap": i + 1})
		r.NoError(err)
	}
	ctx := context.Background()

	// The server is away after the second entry: two sent, three waiting.
	var got []uint64
	sent, err := q.Drain(ctx, func(_ context.Context, e queue.Entry) error {
		got = append(got, e.Seq)
		if e.Seq >= 3 {
			return api.ErrUnreachable
		}
		return nil
	})
	r.ErrorIs(err, api.ErrUnreachable)
	r.Equal(2, sent)
	r.Equal([]uint64{1, 2, 3}, got)
	r.Equal(3, q.Len())

	// Rate limited, a server error, a lost token, a cancelled context: also "not now".
	for _, notNow := range []error{
		&api.Error{Status: http.StatusTooManyRequests, Code: wire.CodeRateLimited},
		&api.Error{Status: http.StatusInternalServerError, Code: wire.CodeServerError},
		&api.Error{Status: http.StatusBadGateway},
		&api.Error{Status: http.StatusUnauthorized, Code: wire.CodeUnauthorized},
		&api.Error{Status: http.StatusUpgradeRequired, Code: wire.CodeClientTooOld},
		context.Canceled,
	} {
		sent, err = q.Drain(ctx, func(context.Context, queue.Entry) error { return notNow })
		r.ErrorIs(err, notNow)
		r.Equal(0, sent)
		r.Equal(3, q.Len(), "nothing was dropped for %v", notNow)
	}

	// Refused for good: dropped, and the next one goes.
	sent, err = q.Drain(ctx, func(_ context.Context, e queue.Entry) error {
		if e.Seq == 3 {
			return &api.Error{Status: http.StatusUnprocessableEntity, Code: wire.CodeInvalid, Message: "no"}
		}
		if e.Seq == 4 {
			return &api.Error{Status: http.StatusConflict, Code: wire.CodeConflict}
		}
		return nil
	})
	r.NoError(err)
	r.Equal(1, sent)
	r.Len(dropped, 2)
	r.Equal(uint64(3), dropped[0].Seq)
	r.Equal(0, q.Len())

	// An error that is neither the API's nor a context's is "not now".
	_, err = q.Push(queue.KindStint, "s-2", map[string]any{})
	r.NoError(err)
	_, err = q.Drain(ctx, func(context.Context, queue.Entry) error { return errors.New("disk on fire") })
	r.ErrorContains(err, "disk on fire")
	r.Equal(1, q.Len())
}

func TestADamagedEntryIsSetAsideNotFatal(t *testing.T) {
	t.Parallel()
	r := require.New(t)
	dir := t.TempDir()
	r.NoError(os.WriteFile(filepath.Join(dir, "000000000001.json"), []byte("{broken"), 0o600))
	r.NoError(os.WriteFile(filepath.Join(dir, "000000000002.json"), []byte(`{"seq":2,"kind":"laps","stint_id":"s","key":"k","body":{}}`), 0o600))
	r.NoError(os.WriteFile(filepath.Join(dir, "000000000003.json"), []byte(`{"seq":0}`), 0o600))

	q, err := queue.Open(dir)
	r.ErrorIs(err, queue.ErrDamaged)
	r.NotNil(q)
	r.Equal(1, q.Len())
	_, err = os.Stat(filepath.Join(dir, "000000000001.json.bad"))
	r.NoError(err, "set aside, not deleted: a support case may want it")
	e, err := q.Push(queue.KindLaps, "s", map[string]any{})
	r.NoError(err)
	r.Equal(uint64(4), e.Seq, "numbering continues past the damaged ones")

	_, err = q.Push(queue.KindLaps, "s", make(chan int))
	r.ErrorContains(err, "encoding")

	blocked := filepath.Join(t.TempDir(), "file")
	r.NoError(os.WriteFile(blocked, nil, 0o600))
	_, err = queue.Open(filepath.Join(blocked, "under"))
	r.Error(err)
}

func seqs(es []queue.Entry) []uint64 {
	out := make([]uint64, len(es))
	for i, e := range es {
		out[i] = e.Seq
	}
	return out
}
