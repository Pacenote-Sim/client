package api_test

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/pacenote-sim/protocol/wire"

	"github.com/pacenote-sim/client/internal/api"
)

// golden reads one of the server's committed fixtures: the bytes the real
// server produces, so this client is tested against them and not against a
// fake of my own devising.
func golden(t *testing.T, name string) []byte {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join("testdata", "api-v1", name+".json"))
	require.NoError(t, err)
	return raw
}

// fakeServer answers every route with the goldens and records what it saw.
type fakeServer struct {
	t        *testing.T
	mu       sync.Mutex
	requests []*http.Request
	bodies   [][]byte
	polls    []string // the status each poll answers with, in order
	fail     map[string]string
}

func (f *fakeServer) handler() http.Handler {
	mux := http.NewServeMux()
	serve := func(name string, status int) http.HandlerFunc {
		return func(w http.ResponseWriter, r *http.Request) {
			body, _ := io.ReadAll(r.Body)
			f.mu.Lock()
			f.requests = append(f.requests, r)
			f.bodies = append(f.bodies, body)
			failWith := f.fail[r.Method+" "+r.URL.Path]
			f.mu.Unlock()
			if failWith != "" {
				var env wire.ErrorEnvelope
				raw := golden(f.t, "error-"+failWith+".response")
				_ = json.Unmarshal(raw, &env)
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(env.Error.Code.HTTPStatus())
				_, _ = w.Write(raw)
				return
			}
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(status)
			if name != "" {
				_, _ = w.Write(golden(f.t, name))
			}
		}
	}
	mux.HandleFunc("GET "+api.DiscoveryPath, serve("discovery.response", http.StatusOK))
	mux.HandleFunc("POST "+api.Prefix+"/pair/start", serve("pair-start.response", http.StatusOK))
	mux.HandleFunc("POST "+api.Prefix+"/pair/poll", func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		f.requests = append(f.requests, r)
		status := "pending"
		if len(f.polls) > 0 {
			status, f.polls = f.polls[0], f.polls[1:]
		}
		f.mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		if strings.HasPrefix(status, "error-") {
			w.WriteHeader(http.StatusTooManyRequests)
			_, _ = w.Write(golden(f.t, status+".response"))
			return
		}
		_, _ = w.Write(golden(f.t, "pair-poll-"+status+".response"))
	})
	mux.HandleFunc("GET "+api.Prefix+"/me", serve("me.response", http.StatusOK))
	mux.HandleFunc("GET "+api.Prefix+"/reference", serve("reference.response", http.StatusOK))
	mux.HandleFunc("PUT "+api.Prefix+"/stints/{id}", serve("stint.response", http.StatusOK))
	mux.HandleFunc("POST "+api.Prefix+"/stints/{id}/laps", serve("laps.response", http.StatusOK))
	mux.HandleFunc("PUT "+api.Prefix+"/stints/{id}/summary", serve("summary.response", http.StatusOK))
	mux.HandleFunc("POST "+api.Prefix+"/live", serve("", http.StatusNoContent))
	mux.HandleFunc("POST "+api.Prefix+"/field", serve("field.response", http.StatusOK))
	mux.HandleFunc("/plugin/engineer/laps", func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		f.requests = append(f.requests, r)
		f.mu.Unlock()
		w.WriteHeader(http.StatusTeapot)
		_, _ = w.Write([]byte("brewed for " + r.Header.Get("Authorization")))
	})
	return mux
}

func (f *fakeServer) last() *http.Request {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.requests[len(f.requests)-1]
}

func (f *fakeServer) lastBody() []byte {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.bodies[len(f.bodies)-1]
}

func newClient(t *testing.T, f *fakeServer) *api.Client {
	t.Helper()
	srv := httptest.NewServer(f.handler())
	t.Cleanup(srv.Close)
	c, err := api.New(srv.URL+"/", "1.2.3", nil)
	require.NoError(t, err)
	c.Sleep = func(context.Context, time.Duration) error { return nil }
	return c
}

func TestEveryRouteSpeaksTheProtocol(t *testing.T) {
	t.Parallel()
	r := require.New(t)
	f := &fakeServer{t: t}
	c := newClient(t, f)
	ctx := context.Background()

	d, err := c.Discover(ctx)
	r.NoError(err)
	r.Equal("Iberian GT Championship", d.Name)
	r.Equal(300, d.Limits.TracePoints)
	r.True(strings.HasPrefix(f.last().Header.Get("User-Agent"), "Pacenote/1.2.3 ("), f.last().Header.Get("User-Agent"))
	r.Equal("1.2.3", f.last().Header.Get("X-Client-Version"))
	r.Empty(f.last().Header.Get("Authorization"), "discovery carries no token")

	c.SetToken("tok-secret")
	me, err := c.Me(ctx)
	r.NoError(err)
	r.Equal("ana-ruiz", me.Driver.Slug)
	r.Equal("Bearer tok-secret", f.last().Header.Get("Authorization"))

	var stintReq wire.Stint
	r.NoError(json.Unmarshal(golden(t, "stint.request"), &stintReq))
	res, err := c.PutStint(ctx, "01a09580-0818-7a2a-aa2a-2a2a2a2a2a2a", stintReq, "key-1")
	r.NoError(err)
	r.Equal("01a09580-0818-7a2a-aa2a-2a2a2a2a2a2a", res.StintID)
	r.Equal("key-1", f.last().Header.Get("Idempotency-Key"))
	r.Equal("application/json", f.last().Header.Get("Content-Type"))
	r.JSONEq(string(golden(t, "stint.request")), string(f.lastBody()), "the body is the golden, byte for byte in meaning")

	var laps wire.LapBatch
	r.NoError(json.Unmarshal(golden(t, "laps.request"), &laps))
	lr, err := c.PostLaps(ctx, "s-1", laps, "key-2")
	r.NoError(err)
	r.Equal(1, lr.Accepted)
	r.Equal(91234, lr.BestLapMs)
	r.JSONEq(string(golden(t, "laps.request")), string(f.lastBody()))

	var sum wire.Summary
	r.NoError(json.Unmarshal(golden(t, "summary.request"), &sum))
	r.NoError(c.PutSummary(ctx, "s-1", sum, "key-3"))
	r.JSONEq(string(golden(t, "summary.request")), string(f.lastBody()))

	var live wire.LiveSample
	r.NoError(json.Unmarshal(golden(t, "live.request"), &live))
	r.NoError(c.PostLive(ctx, live, "key-4"))
	r.JSONEq(string(golden(t, "live.request")), string(f.lastBody()))

	var field wire.FieldReport
	r.NoError(json.Unmarshal(golden(t, "field.request"), &field))
	fr, err := c.PostField(ctx, field, "key-5")
	r.NoError(err)
	r.True(fr.Armed)
	r.JSONEq(string(golden(t, "field.request")), string(f.lastBody()))

	ref, err := c.Reference(ctx, "iracing", "barcelona gp", "Ferrari 296 GT3", "gt3", wire.ScopeCar)
	r.NoError(err)
	r.NotEmpty(ref.Trace)
	r.Equal("barcelona gp", f.last().URL.Query().Get("track_id"))
	r.Equal("car", f.last().URL.Query().Get("scope"))

	// A companion's door: the token goes, the path stays under the mount.
	res2, err := c.Plugin(ctx, "engineer", http.MethodPost, "/laps?x=1", strings.NewReader("{}"), http.Header{"Content-Type": {"application/json"}})
	r.NoError(err)
	body, _ := io.ReadAll(res2.Body)
	r.NoError(res2.Body.Close())
	r.Equal(http.StatusTeapot, res2.StatusCode)
	r.Equal("brewed for Bearer tok-secret", string(body))
	r.Equal("/plugin/engineer/laps", f.last().URL.Path)
	for _, bad := range []string{"laps", "/../../api/v1/me", "//evil", "http://x/"} {
		_, err := c.Plugin(ctx, "engineer", http.MethodGet, bad, nil, nil) //nolint:bodyclose // refused before a request exists.
		r.ErrorIs(err, api.ErrOutsideMount, bad)
	}
	r.Equal(c.Address(), strings.TrimSuffix(c.Address(), "/"))
}

func TestPairingRunsToADecision(t *testing.T) {
	t.Parallel()
	r := require.New(t)
	ctx := context.Background()

	f := &fakeServer{t: t, polls: []string{"pending", "pending", "approved"}}
	c := newClient(t, f)
	var shown wire.PairStart
	poll, err := c.Pair(ctx, func(p wire.PairStart) { shown = p })
	r.NoError(err)
	r.Equal("H4T-9KQ", shown.UserCode)
	r.Equal(wire.StatusApproved, poll.Status)
	r.NotEmpty(poll.Token)
	r.Equal(poll.Token, c.Token(), "the token is kept")
	r.Equal("ana-ruiz", poll.Driver.Slug)
	var polls int
	for _, req := range f.requests {
		if strings.HasSuffix(req.URL.Path, "/pair/poll") {
			polls++
		}
	}
	r.Equal(3, polls)

	denied := newClient(t, &fakeServer{t: t, polls: []string{"denied"}})
	_, err = denied.Pair(ctx, nil)
	r.ErrorIs(err, api.ErrPairingDenied)
	r.Empty(denied.Token())

	expired := newClient(t, &fakeServer{t: t, polls: []string{"expired"}})
	_, err = expired.Pair(ctx, nil)
	r.ErrorIs(err, api.ErrPairingExpired)

	cancelled, cancel := context.WithCancel(ctx)
	cancel()
	waiting := newClient(t, &fakeServer{t: t})
	waiting.Sleep = func(ctx context.Context, _ time.Duration) error { return ctx.Err() }
	_, err = waiting.Pair(cancelled, nil)
	r.ErrorIs(err, context.Canceled)

	// A server that rate-limits a poll is polled again.
	limited := newClient(t, &fakeServer{t: t, polls: []string{"error-rate_limited", "approved"}})
	poll, err = limited.Pair(ctx, nil)
	r.NoError(err)
	r.Equal(wire.StatusApproved, poll.Status)

	// One that refuses to open a pairing ends it.
	refusing := newClient(t, &fakeServer{t: t, fail: map[string]string{"POST " + api.Prefix + "/pair/start": "invalid"}})
	_, err = refusing.Pair(ctx, nil)
	var e *api.Error
	r.ErrorAs(err, &e)
	r.Equal(wire.CodeInvalid, e.Code)
	r.False(e.Retryable())

	// A poll that stays pending past the code's life expires.
	slow := newClient(t, &fakeServer{t: t})
	now := time.Now()
	slow.Now = func() time.Time { now = now.Add(20 * time.Minute); return now }
	_, err = slow.Pair(ctx, nil)
	r.ErrorIs(err, api.ErrPairingExpired)

	// A poll the server answers with a final refusal ends it.
	broken := newClient(t, &fakeServer{t: t, polls: []string{"error-invalid"}})
	_, err = broken.Pair(ctx, nil)
	r.ErrorAs(err, &e)
}

func TestTheErrorEnvelopeBecomesAnError(t *testing.T) {
	t.Parallel()
	r := require.New(t)
	ctx := context.Background()

	for _, tc := range []struct {
		code      string
		retryable bool
		is        error
	}{
		{"unauthorized", false, api.ErrUnauthorized},
		{"forbidden", false, nil},
		{"not_found", false, nil},
		{"conflict", false, nil},
		{"invalid", false, nil},
		{"rate_limited", true, nil},
		{"server_error", true, nil},
		{"client_too_old", false, api.ErrTooOld},
	} {
		f := &fakeServer{t: t, fail: map[string]string{"GET " + api.Prefix + "/me": tc.code}}
		c := newClient(t, f)
		_, err := c.Me(ctx)
		var e *api.Error
		r.ErrorAs(err, &e, tc.code)
		r.Equal(wire.Code(tc.code), e.Code)
		r.Equal(e.Code.HTTPStatus(), e.Status)
		r.NotEmpty(e.Message, "the message is written for the driver")
		r.Equal(e.Message, e.Error())
		r.Equal(tc.retryable, e.Retryable(), tc.code)
		if tc.is != nil {
			r.ErrorIs(err, tc.is, tc.code)
		} else {
			r.NotErrorIs(err, api.ErrUnauthorized)
		}
		if tc.code == "rate_limited" {
			r.Equal(time.Second, e.RetryAfter)
		}
	}

	// An answer with no envelope is judged by its status; no answer at all is unreachable.
	bare := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Retry-After", "7")
		w.WriteHeader(http.StatusBadGateway)
		_, _ = w.Write([]byte("<html>gateway</html>"))
	}))
	t.Cleanup(bare.Close)
	c, err := api.New(bare.URL, "1.0.0", nil)
	r.NoError(err)
	_, err = c.Me(ctx)
	var e *api.Error
	r.ErrorAs(err, &e)
	r.Equal(http.StatusBadGateway, e.Status)
	r.True(e.Retryable())
	r.Equal(7*time.Second, e.RetryAfter)
	r.Equal("the server answered 502", e.Error())
	r.False(e.Is(errors.New("other")))

	gone, err := api.New("http://127.0.0.1:1", "1.0.0", nil)
	r.NoError(err)
	_, err = gone.Me(ctx)
	r.ErrorIs(err, api.ErrUnreachable)

	// A 2xx that is not JSON is an error too.
	junk := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write([]byte("<html>")) }))
	t.Cleanup(junk.Close)
	j, err := api.New(junk.URL, "1.0.0", nil)
	r.NoError(err)
	_, err = j.Me(ctx)
	r.ErrorContains(err, "not the JSON it should be")

	for _, bad := range []string{"", "team.example", "ftp://x", "https://", "::"} {
		_, err := api.New(bad, "1.0.0", nil)
		r.Error(err, bad)
	}
}
