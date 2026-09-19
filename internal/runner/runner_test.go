package runner_test

import (
	"context"
	"encoding/json"
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

	"github.com/pacenote-sim/clientplugin"
	demo "github.com/pacenote-sim/clientplugin/examples/source-demo"

	"github.com/pacenote-sim/client/internal/config"
	"github.com/pacenote-sim/client/internal/runner"
)

// server is a small stateful stand-in for the real one: pairing approves at
// once, /me lists the echo plugin, writes are kept, and the echo plugin's one
// route counts what it receives.
type server struct {
	mu          sync.Mutex
	down        bool
	noDiscovery bool
	token       string
	stints      map[string]wire.Stint
	laps        map[string][]wire.Lap
	summary     map[string]wire.Summary
	live        int
	field       int
	echoLaps    int
	keys        map[string]int
	me          int
}

func newServer() *server {
	return &server{token: "tok-test", stints: map[string]wire.Stint{}, laps: map[string][]wire.Lap{}, summary: map[string]wire.Summary{}, keys: map[string]int{}}
}

func (s *server) handler() http.Handler {
	mux := http.NewServeMux()
	ok := func(w http.ResponseWriter, v any) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(v)
	}
	fail := func(w http.ResponseWriter, code wire.Code) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(code.HTTPStatus())
		_ = json.NewEncoder(w).Encode(wire.NewError(code, "no"))
	}
	authed := func(next func(http.ResponseWriter, *http.Request)) http.HandlerFunc {
		return func(w http.ResponseWriter, r *http.Request) {
			s.mu.Lock()
			down, token := s.down, s.token
			s.mu.Unlock()
			if down {
				fail(w, wire.CodeServerError)
				return
			}
			if r.Header.Get("Authorization") != "Bearer "+token {
				fail(w, wire.CodeUnauthorized)
				return
			}
			if r.Method != http.MethodGet && r.Header.Get("Idempotency-Key") == "" {
				fail(w, wire.CodeInvalid)
				return
			}
			next(w, r)
		}
	}
	mux.HandleFunc("GET /.well-known/sim-telemetry.json", func(w http.ResponseWriter, _ *http.Request) {
		s.mu.Lock()
		none := s.noDiscovery
		s.mu.Unlock()
		if none {
			fail(w, wire.CodeServerError)
			return
		}
		ok(w, wire.Discovery{Limits: wire.Limits{TracePoints: 50, LapsPerRequest: 50, LiveIntervalMs: 20, FieldIntervalMs: 20, SummaryIntervalMs: 30, MaxBodyBytes: 2 << 20}})
	})
	mux.HandleFunc("POST /api/v1/pair/start", func(w http.ResponseWriter, _ *http.Request) {
		ok(w, wire.PairStart{DeviceCode: "dev", UserCode: "AB-12", IntervalS: 1, ExpiresInS: 60})
	})
	mux.HandleFunc("POST /api/v1/pair/poll", func(w http.ResponseWriter, _ *http.Request) {
		ok(w, wire.PairPoll{Status: wire.StatusApproved, Token: s.token, Driver: &wire.Driver{Slug: "mihai", Name: "Mihai"}})
	})
	mux.HandleFunc("GET /api/v1/me", authed(func(w http.ResponseWriter, _ *http.Request) {
		s.mu.Lock()
		s.me++
		s.mu.Unlock()
		ok(w, wire.Me{Driver: wire.Driver{Slug: "mihai", Name: "Mihai"}, Plugins: []wire.PluginInfo{
			{Name: "echo", Title: "Echo", Version: "1", Routes: []wire.PluginRoute{{Path: "/laps", Access: "driver"}}},
			{Name: "voice", Title: "Voice", Version: "1", Routes: []wire.PluginRoute{{Path: "/speak", Access: "driver"}}},
		}})
	}))
	mux.HandleFunc("PUT /api/v1/stints/{id}", authed(func(w http.ResponseWriter, r *http.Request) {
		var st wire.Stint
		_ = json.NewDecoder(r.Body).Decode(&st)
		s.mu.Lock()
		s.stints[r.PathValue("id")] = st
		s.keys[r.Header.Get("Idempotency-Key")]++
		s.mu.Unlock()
		ok(w, wire.StintResult{StintID: r.PathValue("id")})
	}))
	mux.HandleFunc("POST /api/v1/stints/{id}/laps", authed(func(w http.ResponseWriter, r *http.Request) {
		var b wire.LapBatch
		_ = json.NewDecoder(r.Body).Decode(&b)
		s.mu.Lock()
		// The real server replays the stored answer for a key it has seen:
		// a retry of the same write never stores a lap twice.
		key := r.Header.Get("Idempotency-Key")
		s.keys[key]++
		if s.keys[key] > 1 {
			s.mu.Unlock()
			ok(w, wire.LapBatchResult{Accepted: len(b.Laps)})
			return
		}
		s.laps[r.PathValue("id")] = append(s.laps[r.PathValue("id")], b.Laps...)
		s.mu.Unlock()
		ok(w, wire.LapBatchResult{Accepted: len(b.Laps)})
	}))
	mux.HandleFunc("PUT /api/v1/stints/{id}/summary", authed(func(w http.ResponseWriter, r *http.Request) {
		var sum wire.Summary
		_ = json.NewDecoder(r.Body).Decode(&sum)
		s.mu.Lock()
		s.summary[r.PathValue("id")] = sum
		s.mu.Unlock()
		ok(w, wire.OK{OK: true})
	}))
	mux.HandleFunc("POST /api/v1/live", authed(func(w http.ResponseWriter, _ *http.Request) {
		s.mu.Lock()
		s.live++
		s.mu.Unlock()
		w.WriteHeader(http.StatusNoContent)
	}))
	mux.HandleFunc("POST /api/v1/field", authed(func(w http.ResponseWriter, _ *http.Request) {
		s.mu.Lock()
		s.field++
		s.mu.Unlock()
		ok(w, wire.FieldResult{OK: true, Armed: true})
	}))
	mux.HandleFunc("POST /plugin/echo/laps", func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer "+s.token {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		s.mu.Lock()
		s.echoLaps++
		s.mu.Unlock()
		_, _ = w.Write([]byte("ok"))
	})
	return mux
}

// echo is the contract's example companion, made here rather than imported so
// that its init does not register into the default registry of every test.
type echo struct {
	host clientplugin.Host

	mu sync.Mutex
	// audible is what the app said about being heard, each lap: a companion
	// asks before its server half buys audio nobody would listen to.
	audible []bool
}

func (e *echo) heard() []bool {
	e.mu.Lock()
	defer e.mu.Unlock()
	return append([]bool(nil), e.audible...)
}

func (*echo) Name() string { return "echo" }
func (*echo) Wants() []clientplugin.EventKind {
	return []clientplugin.EventKind{clientplugin.KindLapCompleted, clientplugin.KindCornerApproaching}
}

func (e *echo) Start(_ context.Context, h clientplugin.Host) error {
	e.host = h
	h.Status("waiting")
	return nil
}

func (e *echo) Notify(ctx context.Context, ev clientplugin.Event) error {
	switch ev := ev.(type) {
	case *clientplugin.LapCompleted:
		res, err := e.host.Do(ctx, http.MethodPost, "/laps", strings.NewReader(`{}`), nil)
		if err != nil {
			return err
		}
		_, _ = io.Copy(io.Discard, res.Body)
		_ = res.Body.Close()
		e.host.Status("posted lap " + string(rune('0'+ev.Lap)))
		e.mu.Lock()
		e.audible = append(e.audible, e.host.Audible())
		e.mu.Unlock()
		return e.host.Play(ctx, []byte("clip"), "audio/wav")
	case *clientplugin.CornerApproaching:
		return e.host.Say(ctx, "Turn.")
	}
	return nil
}

func (*echo) Stop() error { return nil }

// speaker is the companion that makes sound: what the voice plugin's client
// half will be. It records what it was handed.
type speaker struct {
	mu     sync.Mutex
	said   []string
	played int
	acted  []string
	on     bool
}

// Act implements clientplugin.Actor: the page's volume slider.
func (s *speaker) Act(_ context.Context, action string, values map[string]string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.acted = append(s.acted, action+"="+values["level"])
	return nil
}

func (*speaker) Name() string                                   { return "voice" }
func (*speaker) Wants() []clientplugin.EventKind                { return nil }
func (*speaker) Start(context.Context, clientplugin.Host) error { return nil }
func (*speaker) Notify(context.Context, clientplugin.Event) error {
	return nil
}
func (*speaker) Stop() error { return nil }

// Audible is the driver's own switch: the stand-in starts muted and the test
// turns it on, as the voice companion's mode setting does.
func (s *speaker) Audible() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.on
}

func (s *speaker) Play(context.Context, []byte, string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.played++
	return nil
}

func (s *speaker) Say(_ context.Context, text string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.said = append(s.said, text)
	return nil
}

// fastDemo is the demo circuit at 60 s laps, played about 100 times faster,
// with a field so that POST /field has something to send.
func fastDemo() *demo.Source {
	src := demo.New()
	src.Enabled = true
	src.LapSeconds = 60
	clock := time.Now()
	src.Sleep = func(ctx context.Context, d time.Duration) error {
		clock = clock.Add(d)
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(d / 100):
			return nil
		}
	}
	demo.SetNow(src, func() time.Time { return clock })
	return src
}

type withField struct{ clientplugin.Source }

func (w withField) Read(ctx context.Context) (clientplugin.Sample, error) {
	s, err := w.Source.Read(ctx)
	s.Field = []wire.FieldCar{{Num: "1", IsPlayer: true}}
	return s, err
}

func TestARunPairsDrivesAndUploads(t *testing.T) {
	t.Parallel()
	r := require.New(t)
	srv := newServer()
	ts := httptest.NewServer(srv.handler())
	t.Cleanup(ts.Close)
	dir := t.TempDir()

	reg := clientplugin.NewRegistry()
	e := &echo{}
	reg.AddCompanion(e)
	voice := &speaker{on: true}
	reg.AddCompanion(voice)
	var shown wire.PairStart
	var statuses []runner.Status
	var mu sync.Mutex
	var run *runner.Runner
	var actErrs []error
	acted := false
	run, err := runner.New(runner.Config{
		Version: "1.0.0", DataDir: dir, Address: ts.URL, Registry: reg,
		Sources: []clientplugin.Source{withField{fastDemo()}}, Record: true, StopAfterLaps: 2,
		OnPairing: func(p wire.PairStart) { shown = p },
		OnStatus: func(s runner.Status) {
			mu.Lock()
			defer mu.Unlock()
			statuses = append(statuses, s)
			// While the companions run, the page uses the voice plugin's slider
			// and tries a control on a page that has none.
			if len(s.Companions) == 2 && !acted && run != nil {
				acted = true
				go func() {
					e1 := run.Act(context.Background(), "voice", "volume", map[string]string{"level": "70"})
					e2 := run.Act(context.Background(), "echo", "anything", nil)
					e3 := run.Act(context.Background(), "nobody", "anything", nil)
					mu.Lock()
					actErrs = append(actErrs, e1, e2, e3)
					mu.Unlock()
				}()
			}
		},
	})
	r.NoError(err)
	r.False(run.Status().Paired)

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	r.NoError(run.Run(ctx))

	// Paired, and the token kept for next time.
	r.Equal("AB-12", shown.UserCode)
	saved, err := config.Load(dir)
	r.NoError(err)
	r.Equal("tok-test", saved.Token)

	// One stint, two laps, a final summary, live and field samples, and the
	// echo companion posted every lap.
	srv.mu.Lock()
	defer srv.mu.Unlock()
	r.Len(srv.stints, 1)
	var id string
	for id = range srv.stints {
		break
	}
	r.Equal("demo", srv.stints[id].Sim)
	r.Equal("Demo Circuit", srv.stints[id].Track)
	r.Len(srv.laps[id], 2)
	r.LessOrEqual(len(srv.laps[id][0].Trace), 50, "resampled to the server's cap")
	r.Empty(srv.laps[id][0].Corners)
	sum, ok := srv.summary[id]
	r.True(ok)
	r.NotNil(sum.FinishedAt, "the last summary is final")
	r.Equal(2, sum.Laps)
	r.LessOrEqual(len(sum.BestTrace), 50)
	r.Positive(srv.live)
	r.Positive(srv.field)
	r.Equal(2, srv.echoLaps)
	r.GreaterOrEqual(srv.me, 1)

	st := run.Status()
	r.True(st.Paired)
	r.Equal("Mihai", st.Driver)
	// What the simulator says the driver is in, for the app to show beside
	// their name.
	r.Equal("Demo GT3", st.Car)
	r.Equal("gt3", st.CarClass)
	r.Equal("Demo Circuit", st.Track)
	r.Equal(2, st.Laps)
	r.Zero(st.Queued, "everything was sent")
	r.Equal([]string{"echo", "voice"}, st.Companions)
	r.True(st.Sound, "a companion that plays is running")
	r.NotEmpty(statuses)

	// Sound is a plugin: what echo said and played went to the voice companion.
	voice.mu.Lock()
	r.Contains(voice.said, "Turn.")
	r.Equal(2, voice.played, "one clip per lap")
	r.Equal([]string{"volume=70"}, voice.acted, "the page's slider reached the voice companion")
	voice.mu.Unlock()

	// And a companion could ask, before it went, whether anything it played
	// would be heard: the voice companion was running with its sound on.
	heard := e.heard()
	r.NotEmpty(heard)
	for _, ok := range heard {
		r.True(ok, "the app said nothing would be heard while the voice companion was playing")
	}
	mu.Lock()
	r.Len(actErrs, 3)
	r.NoError(actErrs[0])
	r.ErrorIs(actErrs[1], runner.ErrNoControls, "echo's page has no controls")
	r.ErrorIs(actErrs[2], runner.ErrNoControls, "a plugin that is not running")
	mu.Unlock()
	r.ErrorIs(run.Act(context.Background(), "voice", "volume", nil), runner.ErrNoControls, "after the run, nothing runs")

	// A companion's settings are kept in the client's file, by plugin.
	r.NoError(run.SetSetting("voice", "volume", "70"))
	r.Equal("70", run.Setting("voice", "volume"))
	kept, err := config.Load(dir)
	r.NoError(err)
	r.Equal("70", kept.Setting("voice", "volume"))
	r.Equal("tok-test", kept.Token)

	// The session was recorded.
	recs, err := filepath.Glob(filepath.Join(dir, "recordings", "demo-*.jsonl.gz"))
	r.NoError(err)
	r.Len(recs, 1)
}

func TestWritesWaitWhileTheServerIsDownAndALostTokenEndsTheRun(t *testing.T) {
	t.Parallel()
	r := require.New(t)
	srv := newServer()
	ts := httptest.NewServer(srv.handler())
	t.Cleanup(ts.Close)
	dir := t.TempDir()
	r.NoError(config.Save(dir, config.Config{Token: "tok-test"}))

	// The server is down for the whole run: nothing is lost, everything waits.
	srv.mu.Lock()
	srv.down = true
	srv.mu.Unlock()
	run, err := runner.New(runner.Config{
		Version: "1.0.0", DataDir: dir, Address: ts.URL, Registry: clientplugin.NewRegistry(),
		Sources: []clientplugin.Source{fastDemo()}, StopAfterLaps: 1,
	})
	r.NoError(err)
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	r.NoError(run.Run(ctx))
	r.Positive(run.Status().Queued, "the stint, the lap and the summary wait on disk")
	entries, err := os.ReadDir(filepath.Join(dir, "queue"))
	r.NoError(err)
	r.NotEmpty(entries)

	// The server is back: a new run drains the queue before its own laps.
	srv.mu.Lock()
	srv.down = false
	srv.mu.Unlock()
	run, err = runner.New(runner.Config{
		Version: "1.0.0", DataDir: dir, Address: ts.URL, Registry: clientplugin.NewRegistry(),
		Sources: []clientplugin.Source{fastDemo()}, StopAfterLaps: 1,
	})
	r.NoError(err)
	r.NoError(run.Run(ctx))
	r.Zero(run.Status().Queued)
	srv.mu.Lock()
	r.Len(srv.stints, 2, "the stint from the outage and the new one")
	srv.mu.Unlock()

	// A token the server has forgotten ends the run and is forgotten here too.
	srv.mu.Lock()
	srv.token = "another"
	srv.mu.Unlock()
	run, err = runner.New(runner.Config{
		Version: "1.0.0", DataDir: dir, Address: ts.URL, Registry: clientplugin.NewRegistry(),
		Sources: []clientplugin.Source{fastDemo()}, StopAfterLaps: 1,
	})
	r.NoError(err)
	r.ErrorIs(run.Run(ctx), runner.ErrUnpaired)
	saved, err := config.Load(dir)
	r.NoError(err)
	r.Empty(saved.Token)
	r.False(run.Status().Paired)
}

func TestARunnerRefusesWhatItCannotStartFrom(t *testing.T) {
	t.Parallel()
	r := require.New(t)
	dir := t.TempDir()
	_, err := runner.New(runner.Config{DataDir: dir, Address: "not a server"})
	r.Error(err)
	r.NoError(os.WriteFile(filepath.Join(dir, config.FileName), []byte("{"), 0o600))
	_, err = runner.New(runner.Config{DataDir: dir, Address: "http://localhost:1"})
	r.Error(err)

	// With no source running and no server, the run waits until told to stop.
	quiet := t.TempDir()
	run, err := runner.New(runner.Config{DataDir: quiet, Address: "http://127.0.0.1:1", Registry: clientplugin.NewRegistry()})
	r.NoError(err)
	ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
	defer cancel()
	err = run.Run(ctx)
	r.Error(err, "pairing needs a server")
}

// oneLap is a registry source: it runs once for a little over two laps, ends
// like a simulator that closed, and is not running again.
type oneLap struct {
	demo    *demo.Source
	runs    int
	read    int
	openErr error
}

func (o *oneLap) Name() string  { return "onelap" }
func (o *oneLap) Running() bool { return o.runs == 0 }
func (o *oneLap) Open(ctx context.Context) error {
	o.runs++
	if o.openErr != nil {
		return o.openErr
	}
	return o.demo.Open(ctx)
}

func (o *oneLap) Read(ctx context.Context) (clientplugin.Sample, error) {
	o.read++
	if o.read > 2500 {
		return clientplugin.Sample{}, io.EOF
	}
	return o.demo.Read(ctx)
}
func (o *oneLap) Close() error { return o.demo.Close() }

func TestASimulatorThatComesAndGoes(t *testing.T) {
	t.Parallel()
	r := require.New(t)
	srv := newServer()
	srv.noDiscovery = true
	ts := httptest.NewServer(srv.handler())
	t.Cleanup(ts.Close)
	dir := t.TempDir()
	r.NoError(config.Save(dir, config.Config{Token: "tok-test"}))

	// Two damaged and one unknown entry wait in the queue from before.
	qdir := filepath.Join(dir, "queue")
	r.NoError(os.MkdirAll(qdir, 0o700))
	r.NoError(os.WriteFile(filepath.Join(qdir, "000000000001.json"), []byte(`{"seq":1,"kind":"bogus","stint_id":"s","key":"k","body":{}}`), 0o600))
	r.NoError(os.WriteFile(filepath.Join(qdir, "000000000002.json"), []byte(`{"seq":2,"kind":"laps","stint_id":"s","key":"k","body":"nope"}`), 0o600))
	r.NoError(os.WriteFile(filepath.Join(qdir, "000000000003.json"), []byte(`{broken`), 0o600))
	// And the recordings folder is a file, so recording fails and is given up.
	r.NoError(os.WriteFile(filepath.Join(dir, "recordings"), nil, 0o600))

	reg := clientplugin.NewRegistry()
	src := &oneLap{demo: fastDemo()}
	reg.AddSource(src)
	reg.AddCompanion(&echo{}) // no Player runs: what it says and plays is logged and lost
	run, err := runner.New(runner.Config{Version: "1.0.0", DataDir: dir, Address: ts.URL, Registry: reg, Record: true, MeInterval: 50 * time.Millisecond})
	r.NoError(err)
	ctx, cancel := context.WithTimeout(context.Background(), 4*time.Second)
	defer cancel()
	r.NoError(run.Run(ctx), "a registry source ending is not the end of the run; the timeout is")

	srv.mu.Lock()
	r.Len(srv.stints, 1, "the lap from the simulator that came and went")
	for id := range srv.stints {
		r.NotNil(srv.summary[id].FinishedAt, "its stint was closed when the source ended")
	}
	r.Greater(srv.me, 1, "GET /me was read again on its interval")
	srv.mu.Unlock()
	st := run.Status()
	r.Zero(st.Queued, "the unknown and unreadable entries were dropped, not kept for ever")
	r.Equal("waiting for a simulator", st.Message)
	r.False(st.Sound, "echo runs, but nothing plays")

	// A source that cannot be opened ends an explicit run at once.
	broken := &oneLap{demo: fastDemo(), openErr: io.ErrUnexpectedEOF}
	run, err = runner.New(runner.Config{Version: "1.0.0", DataDir: t.TempDir(), Address: ts.URL, Registry: clientplugin.NewRegistry(), Sources: []clientplugin.Source{broken}})
	r.NoError(err)
	ctx2, cancel2 := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel2()
	// This run has no token: it pairs first, then fails to open, then ends.
	r.NoError(run.Run(ctx2))
	r.Equal(1, broken.runs)
}

// Closing the app in the middle of a stint sends everything before it goes:
// the stint is ended, its summary is written and sent as final, the laps are
// up, and nothing is left waiting on disk. A driver who quits after a session
// does not have to open the app again for the server to know the session
// happened.
func TestClosingMidStintSendsEverything(t *testing.T) {
	t.Parallel()
	r := require.New(t)

	srv := newServer()
	ts := httptest.NewServer(srv.handler())
	defer ts.Close()
	dir := t.TempDir()
	r.NoError(config.Save(dir, config.Config{Token: "tok-test"}))

	run, err := runner.New(runner.Config{
		Version: "1.0.0", DataDir: dir, Address: ts.URL, Registry: clientplugin.NewRegistry(),
		Sources: []clientplugin.Source{fastDemo()},
	})
	r.NoError(err)

	// The window closes two laps in: no stop-after, no source that ended, just
	// the context going away under a stint that is still being driven.
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- run.Run(ctx) }()
	deadline := time.After(30 * time.Second)
	for run.Status().Laps < 2 {
		select {
		case <-deadline:
			t.Fatal("the demo never completed two laps")
		case <-time.After(10 * time.Millisecond):
		}
	}
	cancel()
	select {
	case err = <-done:
	case <-time.After(30 * time.Second):
		t.Fatal("the run did not stop when the window closed")
	}
	if err != nil {
		r.ErrorIs(err, context.Canceled)
	}

	r.Zero(run.Status().Queued, "the app closed with uploads still waiting")
	entries, err := os.ReadDir(filepath.Join(dir, "queue"))
	r.NoError(err)
	r.Empty(entries, "nothing was left on disk")

	srv.mu.Lock()
	defer srv.mu.Unlock()
	r.Len(srv.stints, 1)
	var id string
	for id = range srv.stints {
		break
	}
	r.GreaterOrEqual(len(srv.laps[id]), 2, "the laps that were driven are up")
	sum, ok := srv.summary[id]
	r.True(ok, "the stint was left open")
	r.NotNil(sum.FinishedAt, "and it was closed as the app went")
	r.GreaterOrEqual(sum.Laps, 2)
}

// What a companion is told about being heard: nothing plays without a player,
// and nothing plays while the driver has the sound off. It is what stops a
// companion's server half buying audio for a driver who is reading.
func TestNothingIsHeardWithoutAPlayerOrWithTheSoundOff(t *testing.T) {
	t.Parallel()
	r := require.New(t)

	srv := newServer()
	ts := httptest.NewServer(srv.handler())
	defer ts.Close()

	run := func(voice *speaker) []bool {
		dir := t.TempDir()
		require.NoError(t, config.Save(dir, config.Config{Token: "tok-test"}))
		reg := clientplugin.NewRegistry()
		e := &echo{}
		reg.AddCompanion(e)
		if voice != nil {
			reg.AddCompanion(voice)
		}
		one, err := runner.New(runner.Config{
			Version: "1.0.0", DataDir: dir, Address: ts.URL, Registry: reg,
			Sources: []clientplugin.Source{fastDemo()}, StopAfterLaps: 1,
		})
		require.NoError(t, err)
		ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
		defer cancel()
		require.NoError(t, one.Run(ctx))
		return e.heard()
	}

	// No companion that plays at all.
	for _, ok := range run(nil) {
		r.False(ok, "a client with no voice plugin said it would be heard")
	}
	// One that is running with the sound turned off.
	for _, ok := range run(&speaker{}) {
		r.False(ok, "a muted voice plugin said it would be heard")
	}
	// One that is running with the sound on.
	heard := run(&speaker{on: true})
	r.NotEmpty(heard)
	for _, ok := range heard {
		r.True(ok)
	}
}
