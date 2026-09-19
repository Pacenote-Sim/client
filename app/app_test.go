//go:build !wails

package app_test

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/pacenote-sim/protocol/wire"

	"github.com/pacenote-sim/client/app"
	"github.com/pacenote-sim/client/internal/config"
	"github.com/pacenote-sim/client/internal/runner"
	"github.com/pacenote-sim/client/internal/stamp"
)

// The program a driver runs, run. The runner has its own tests; what is tested
// here is the entry point a build calls: the flags, the log, the sources the
// flags choose, and what the process exits with.

// aServer is enough of a server for a client to drive against: the discovery
// document, who the driver is, and somewhere to put a stint.
type aServer struct {
	mu      sync.Mutex
	stints  int
	laps    int
	summary int
}

func (s *aServer) handler() http.Handler {
	mux := http.NewServeMux()
	send := func(w http.ResponseWriter, v any) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(v)
	}
	mux.HandleFunc("GET /.well-known/sim-telemetry.json", func(w http.ResponseWriter, _ *http.Request) {
		send(w, wire.Discovery{API: "/api/v1", Name: "Test", MinClient: "1.0.0", Limits: wire.Limits{TracePoints: 300}})
	})
	mux.HandleFunc("GET /api/v1/me", func(w http.ResponseWriter, _ *http.Request) {
		send(w, wire.Me{Driver: wire.Driver{Slug: "mihai", Name: "Mihai"}})
	})
	mux.HandleFunc("PUT /api/v1/stints/{id}", func(w http.ResponseWriter, r *http.Request) {
		s.mu.Lock()
		s.stints++
		s.mu.Unlock()
		send(w, wire.StintResult{StintID: r.PathValue("id")})
	})
	mux.HandleFunc("POST /api/v1/stints/{id}/laps", func(w http.ResponseWriter, _ *http.Request) {
		s.mu.Lock()
		s.laps++
		s.mu.Unlock()
		send(w, wire.LapBatchResult{Accepted: 1})
	})
	mux.HandleFunc("PUT /api/v1/stints/{id}/summary", func(w http.ResponseWriter, _ *http.Request) {
		s.mu.Lock()
		s.summary++
		s.mu.Unlock()
		send(w, struct{}{})
	})
	mux.HandleFunc("POST /api/v1/live", func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusNoContent) })
	mux.HandleFunc("POST /api/v1/pair/start", func(w http.ResponseWriter, _ *http.Request) {
		send(w, wire.PairStart{DeviceCode: "dev-1", UserCode: "AB-12", VerificationURI: "https://team.example/pair", IntervalS: 1, ExpiresInS: 600})
	})
	mux.HandleFunc("POST /api/v1/pair/poll", func(w http.ResponseWriter, _ *http.Request) {
		send(w, wire.PairPoll{Status: wire.StatusApproved, Token: "tok-paired", Driver: &wire.Driver{Slug: "mihai", Name: "Mihai"}})
	})
	return mux
}

func (s *aServer) counts() (stints, laps, summaries int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.stints, s.laps, s.summary
}

// The whole program, from the command line to the exit code: two laps of the
// demo circuit against a server, and everything sent.
func TestMainDrivesAndStops(t *testing.T) {
	t.Parallel()
	r := require.New(t)

	srv := &aServer{}
	ts := httptest.NewServer(srv.handler())
	defer ts.Close()
	dir := t.TempDir()
	r.NoError(config.Save(dir, config.Config{Token: "tok-test"}))

	var out, errs strings.Builder
	code := app.Main([]string{
		"--server", ts.URL, "--data", dir, "--demo", "--speed", "600", "--laps", "2", "--log", "info",
	}, &out, &errs)
	r.Equal(0, code, errs.String())

	stints, laps, summaries := srv.counts()
	r.Equal(1, stints)
	r.GreaterOrEqual(laps, 2)
	r.Positive(summaries)

	// What it printed: the header, the status as it changed, and the end.
	r.Contains(out.String(), "server "+ts.URL)
	r.Contains(out.String(), "Mihai")
	r.Contains(out.String(), "Demo Circuit")
	r.Contains(out.String(), "done")

	// And the log went to the data directory with the token kept out of it.
	written, err := os.ReadFile(filepath.Join(dir, "pacenote.log"))
	r.NoError(err)
	r.NotEmpty(written)
	r.NotContains(string(written), "tok-test")
}

// A recording is a source like any other, and a client with neither a
// simulator nor a recording waits for one rather than inventing a lap.
func TestMainPlaysARecording(t *testing.T) {
	t.Parallel()
	r := require.New(t)

	srv := &aServer{}
	ts := httptest.NewServer(srv.handler())
	defer ts.Close()

	// A recording made by the demo, then played back.
	made := t.TempDir()
	r.NoError(config.Save(made, config.Config{Token: "tok-test"}))
	var out, errs strings.Builder
	r.Equal(0, app.Main([]string{
		"--server", ts.URL, "--data", made, "--demo", "--speed", "600", "--laps", "1", "--record", "--log", "warn",
	}, &out, &errs), errs.String())

	files, err := filepath.Glob(filepath.Join(made, "recordings", "*.jsonl.gz"))
	r.NoError(err)
	r.NotEmpty(files, "--record wrote nothing")

	played := t.TempDir()
	r.NoError(config.Save(played, config.Config{Token: "tok-test"}))
	out.Reset()
	errs.Reset()
	r.Equal(0, app.Main([]string{
		"--server", ts.URL, "--data", played, "--replay", files[0], "--speed", "600", "--log", "warn",
	}, &out, &errs), errs.String())
	r.Contains(out.String(), "demo", "a replay is filed under the simulator that was recorded")
}

// What the command line refuses, and what it says about it.
func TestMainRefusesWhatItCannotRun(t *testing.T) {
	t.Parallel()
	r := require.New(t)

	var out, errs strings.Builder
	r.Equal(2, app.Main([]string{"--nonsense"}, &out, &errs))
	r.Contains(errs.String(), "nonsense")

	// No server, and this build was not stamped by one.
	errs.Reset()
	r.Equal(2, app.Main([]string{"--data", t.TempDir()}, &out, &errs))
	r.Contains(errs.String(), "--server")

	// A data directory that cannot be written to.
	errs.Reset()
	file := filepath.Join(t.TempDir(), "a-file")
	r.NoError(os.WriteFile(file, []byte("not a directory"), 0o600))
	r.Equal(1, app.Main([]string{"--server", "http://localhost:1", "--data", file}, &out, &errs))
	r.NotEmpty(errs.String())

	// A recording that is not there ends the run with a reason, not a panic.
	errs.Reset()
	dir := t.TempDir()
	r.NoError(config.Save(dir, config.Config{Token: "tok-test"}))
	srv := &aServer{}
	ts := httptest.NewServer(srv.handler())
	defer ts.Close()
	code := app.Main([]string{
		"--server", ts.URL, "--data", dir, "--replay", filepath.Join(dir, "no-such.jsonl.gz"), "--laps", "1", "--log", "warn",
	}, &out, &errs)
	r.Equal(0, code, "a source that will not open leaves the client waiting for one, which is not a failure")
}

// The flags, read: what each one decides and what the program drives when it
// is told nothing.
func TestTheFlagsDecideWhatIsDriven(t *testing.T) {
	t.Parallel()
	r := require.New(t)

	var errs strings.Builder
	set, code := app.ParseFlags([]string{
		"--server", "https://team.example", "--data", "/tmp/x", "--demo", "--speed", "3",
		"--laps", "5", "--record", "--log", "debug",
	}, &errs)
	r.Zero(code)
	r.Equal("https://team.example", set.Address())
	r.Equal("/tmp/x", set.DataDir())
	r.Equal(5, set.Laps())
	r.True(set.Record())
	r.NotEmpty(app.Version())

	// --demo drives the demo circuit; --replay a recording; neither leaves the
	// compiled-in sources to find a simulator themselves.
	demo := app.Sources(set)
	r.Len(demo, 1)
	r.Equal("demo", demo[0].Name())
	r.True(demo[0].Running(), "the demo the flag asked for is running")

	play, code := app.ParseFlags([]string{"--server", "https://x.example", "--data", "/tmp/x", "--replay", "a.jsonl.gz"}, &errs)
	r.Zero(code)
	replays := app.Sources(play)
	r.Len(replays, 1)
	r.Equal("replay", replays[0].Name())

	plain, code := app.ParseFlags([]string{"--server", "https://x.example", "--data", "/tmp/x"}, &errs)
	r.Zero(code)
	r.Empty(app.Sources(plain), "with no flag the client waits for a simulator it was built with")
}

// The one line a run prints when something changes, which is the whole of a
// headless client's interface.
func TestTheStatusLine(t *testing.T) {
	t.Parallel()
	r := require.New(t)

	r.Equal("not paired", app.StatusLine(runner.Status{}))
	r.Equal("Mihai", app.StatusLine(runner.Status{Paired: true, Driver: "Mihai"}))
	r.Equal("Mihai · waiting for a simulator",
		app.StatusLine(runner.Status{Paired: true, Driver: "Mihai", Message: "waiting for a simulator"}))
	full := runner.Status{
		Paired: true, Driver: "Mihai", Source: "iracing", Track: "Okayama", Stint: "s-1", Lap: 4,
		LastLapMs: 98456, Queued: 2, Companions: []string{"engineer", "voice"},
	}
	r.Equal("Mihai · iracing · Okayama · lap 4 · last 1:38.456 · 2 waiting · engineer, voice",
		app.StatusLine(full))
}

// A client nobody has paired yet prints the code to enter, pairs, and keeps
// the token for next time.
func TestMainPairsAndKeepsTheToken(t *testing.T) {
	t.Parallel()
	r := require.New(t)

	srv := &aServer{}
	ts := httptest.NewServer(srv.handler())
	defer ts.Close()
	dir := t.TempDir()

	var out, errs strings.Builder
	code := app.Main([]string{
		"--server", ts.URL, "--data", dir, "--demo", "--speed", "600", "--laps", "1", "--log", "warn",
	}, &out, &errs)
	r.Equal(0, code, errs.String())
	r.Contains(out.String(), "Not paired yet")
	r.Contains(out.String(), "AB-12")
	r.Contains(out.String(), "https://team.example/pair")

	saved, err := config.Load(dir)
	r.NoError(err)
	r.Equal("tok-paired", saved.Token, "the token was not kept for next time")
}

// A token the server no longer knows ends the run, says so, and leaves with a
// code of its own so that whatever started the client can tell.
func TestMainSaysWhenThePairingIsGone(t *testing.T) {
	t.Parallel()
	r := require.New(t)

	mux := http.NewServeMux()
	mux.HandleFunc("GET /.well-known/sim-telemetry.json", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(wire.Discovery{API: "/api/v1", MinClient: "1.0.0", Limits: wire.Limits{TracePoints: 300}})
	})
	mux.HandleFunc("GET /api/v1/me", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusUnauthorized)
		_ = json.NewEncoder(w).Encode(wire.NewError(wire.CodeUnauthorized, "who?"))
	})
	ts := httptest.NewServer(mux)
	defer ts.Close()
	dir := t.TempDir()
	r.NoError(config.Save(dir, config.Config{Token: "tok-forgotten"}))

	var out, errs strings.Builder
	code := app.Main([]string{"--server", ts.URL, "--data", dir, "--demo", "--laps", "1", "--log", "warn"}, &out, &errs)
	r.Equal(3, code)
	r.Contains(out.String(), "no longer paired")

	saved, err := config.Load(dir)
	r.NoError(err)
	r.Empty(saved.Token, "a token the server has forgotten is forgotten here too")
}

// Which server a build talks to: the one stamped into it when a server built
// it, and otherwise the one the flag gives. A stamped client ignores the flag,
// because it belongs to a team.
func TestWhichServerAClientTalksTo(t *testing.T) {
	t.Parallel()
	r := require.New(t)

	stamped := func() (stamp.Stamp, error) { return stamp.Stamp{Address: "https://team.example"}, nil }
	blank := func() (stamp.Stamp, error) { return stamp.Stamp{}, stamp.ErrBlank }
	broken := func() (stamp.Stamp, error) { return stamp.Stamp{}, errors.New("the stamp is damaged") }

	got, err := app.AddressFrom(stamped, "https://somewhere.else")
	r.NoError(err)
	r.Equal("https://team.example", got, "a stamped client belongs to the team that built it")

	got, err = app.AddressFrom(blank, "http://localhost:8080")
	r.NoError(err)
	r.Equal("http://localhost:8080", got)

	_, err = app.AddressFrom(blank, "")
	r.ErrorContains(err, "--server")

	_, err = app.AddressFrom(broken, "http://localhost:8080")
	r.ErrorContains(err, "damaged", "a damaged stamp is not quietly replaced by the flag")
}

// The demo circuit stops when the run does, rather than driving on.
func TestTheDemoStopsWithTheRun(t *testing.T) {
	t.Parallel()
	r := require.New(t)

	src := app.DemoSource(600)
	ctx, cancel := context.WithCancel(context.Background())
	r.NoError(src.Open(ctx))
	_, err := src.Read(ctx)
	r.NoError(err)
	cancel()
	_, err = src.Read(ctx)
	r.ErrorIs(err, context.Canceled)
	r.NoError(src.Close())
}
