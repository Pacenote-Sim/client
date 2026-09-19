// Package runner is the app: it owns the goroutines, wires the packages
// together and decides what happens on each error. The headless command and
// the desktop window both run one of these; the window is a view of it.
//
// Six loops under one context: capture (a source into the tracker into the
// queue and the bus), drain (the queue into the server), live, field, summary,
// and the refresh of GET /me that switches companions on and off. Every loop
// ends when the context does, and Run returns when they all have.
package runner

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"path/filepath"
	"sort"
	"sync"
	"time"

	"github.com/pacenote-sim/protocol/wire"

	"github.com/pacenote-sim/clientplugin"

	"github.com/pacenote-sim/client/internal/api"
	"github.com/pacenote-sim/client/internal/config"
	"github.com/pacenote-sim/client/internal/events"
	"github.com/pacenote-sim/client/internal/host"
	"github.com/pacenote-sim/client/internal/ids"
	"github.com/pacenote-sim/client/internal/lap"
	"github.com/pacenote-sim/client/internal/queue"
	"github.com/pacenote-sim/client/internal/recorder"
	"github.com/pacenote-sim/client/internal/stint"
)

// FlushTimeout is how long the last drain gets when a run ends: the stint's
// summary and whatever else is queued, sent before the app closes. Long enough
// for a server that is slow, short enough that one that is down does not hold
// a window open — what is left is on disk and goes out next time.
const FlushTimeout = 10 * time.Second

// Config is what a Runner is made of.
type Config struct {
	// Version is this client's version, sent to the server.
	Version string
	// DataDir holds config.json, the queue and the recordings.
	DataDir string
	// Address is the server.
	Address string
	// Registry is what was compiled in. Default is the app's.
	Registry *clientplugin.Registry
	// Sources are tried before the registry's: a replay, the demo.
	Sources []clientplugin.Source
	// Record writes every session to DataDir/recordings.
	Record bool
	// StopAfterLaps ends the run after that many completed laps; 0 never.
	StopAfterLaps int
	// MeInterval is how often GET /me is read again. A minute by default.
	MeInterval time.Duration

	Log     *slog.Logger
	Player  host.Player
	Speaker host.Speaker
	Board   *host.Board
	// OnPairing is called with the code to show when the client is not paired.
	OnPairing func(wire.PairStart)
	// OnStatus is called whenever the status changes.
	OnStatus func(Status)
}

// Status is what the window shows about the run.
type Status struct {
	Server string
	Paired bool
	Driver string
	Source string
	Stint  string
	Track  string
	// Car is what the simulator says the driver is in, and CarClass its class
	// where the simulator names one. Both are empty until a stint begins.
	Car        string
	CarClass   string
	Lap        int
	LastLapMs  int
	Laps       int
	Queued     int
	Companions []string
	// Sound reports that a running companion plays audio: the voice plugin's
	// client half. Without one nothing is heard and there is no volume to set.
	Sound   bool
	Message string
}

// ErrUnpaired is what Run returns when the server no longer accepts the token:
// the token was forgotten, and a new Run pairs again.
var ErrUnpaired = errors.New("runner: the server no longer knows this client; pair it again")

// Runner is one run of the app.
type Runner struct {
	cfg     Config
	log     *slog.Logger
	client  *api.Client
	file    config.Config
	q       *queue.Queue
	bus     *events.Bus
	board   *host.Board
	hosts   map[string]*host.Host
	tracker *stint.Tracker
	limits  wire.Limits

	mu       sync.Mutex
	status   Status
	me       wire.Me
	last     clientplugin.Sample
	lastLap  int
	lastOff  int
	hasLast  bool
	stintID  string
	unpaired bool

	kick   chan struct{}
	cancel context.CancelFunc
}

// New prepares a run: reads the configuration, opens the queue, builds the
// client. Nothing talks to the server yet.
func New(cfg Config) (*Runner, error) {
	if cfg.Log == nil {
		cfg.Log = slog.New(slog.DiscardHandler)
	}
	if cfg.Registry == nil {
		cfg.Registry = clientplugin.Default
	}
	if cfg.Board == nil {
		cfg.Board = host.NewBoard(nil)
	}
	if cfg.MeInterval <= 0 {
		cfg.MeInterval = time.Minute
	}
	file, err := config.Load(cfg.DataDir)
	if err != nil {
		return nil, err
	}
	client, err := api.New(cfg.Address, cfg.Version, cfg.Log)
	if err != nil {
		return nil, err
	}
	client.SetToken(file.Token)
	q, err := queue.Open(filepath.Join(cfg.DataDir, "queue"))
	if err != nil && !errors.Is(err, queue.ErrDamaged) {
		return nil, err
	}
	if err != nil {
		cfg.Log.Warn("some queued writes could not be read and were set aside", slog.String("reason", err.Error()))
	}
	r := &Runner{
		cfg: cfg, log: cfg.Log, client: client, file: file, q: q, bus: events.New(cfg.Log), board: cfg.Board,
		hosts: map[string]*host.Host{}, tracker: stint.New(), kick: make(chan struct{}, 1),
	}
	q.Dropped = func(e queue.Entry, err error) {
		r.log.Warn("the server refused a write for good; it was dropped", slog.String("kind", string(e.Kind)), slog.String("reason", err.Error()))
	}
	r.status = Status{Server: client.Address(), Paired: file.Paired(), Queued: q.Len()}
	return r, nil
}

// Status is the run's status now.
func (r *Runner) Status() Status {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.status
}

// Run runs until ctx ends, the configured laps are done, or the server
// forgets this client. It pairs first when there is no token.
func (r *Runner) Run(ctx context.Context) error {
	ctx, cancel := context.WithCancel(ctx)
	r.cancel = cancel
	defer cancel()

	if !r.file.Paired() {
		if err := r.pair(ctx); err != nil {
			return err
		}
	}
	if d, err := r.client.Discover(ctx); err == nil {
		r.limits = d.Limits
	} else {
		r.log.Warn("the server's limits could not be read; using the defaults", slog.String("reason", err.Error()))
	}
	r.defaults()
	if err := r.refreshMe(ctx); err != nil {
		if errors.Is(err, api.ErrUnauthorized) {
			return r.forget()
		}
		r.log.Warn("GET /me failed; companions wait for the next try", slog.String("reason", err.Error()))
	}

	var wg sync.WaitGroup
	loops := []func(context.Context){r.drainLoop, r.meLoop, r.liveLoop, r.fieldLoop, r.summaryLoop, r.captureLoop}
	wg.Add(len(loops))
	for _, loop := range loops {
		go func(l func(context.Context)) {
			defer wg.Done()
			l(ctx)
		}(loop)
	}
	wg.Wait()

	// The last word. The stint the driver was in has just been ended by the
	// capture loop and its summary written, so what is queued now is
	// everything there is; it gets one more try, and the companions are given
	// their chance to finish after it. What will not go now is on disk and
	// goes out the next time the app opens, which is why this ends rather
	// than waiting for a server that is down.
	flush, stop := context.WithTimeout(context.Background(), FlushTimeout)
	defer stop()
	r.drain(flush)
	if left := r.q.Len(); left > 0 {
		r.log.Warn("closing with uploads still waiting; they are kept and go out next time",
			slog.Int("waiting", left))
	} else {
		r.log.Info("everything was sent")
	}
	if err := r.bus.StopAll(); err != nil {
		r.log.Warn("a companion did not stop cleanly", slog.String("reason", err.Error()))
	}
	r.mu.Lock()
	unpaired := r.unpaired
	r.mu.Unlock()
	if unpaired {
		return r.forget()
	}
	return nil
}

func (r *Runner) pair(ctx context.Context) error {
	r.set(func(s *Status) { s.Message = "pairing" })
	poll, err := r.client.Pair(ctx, r.cfg.OnPairing)
	if err != nil {
		return err
	}
	r.file.Token = poll.Token
	if err := config.Save(r.cfg.DataDir, r.file); err != nil {
		return err
	}
	name := ""
	if poll.Driver != nil {
		name = poll.Driver.Name
	}
	r.set(func(s *Status) { s.Paired, s.Driver, s.Message = true, name, "" })
	r.log.Info("paired", slog.String("driver", name))
	return nil
}

// forget drops a token the server no longer accepts.
func (r *Runner) forget() error {
	r.file.Token = ""
	r.client.SetToken("")
	if err := config.Save(r.cfg.DataDir, r.file); err != nil {
		return err
	}
	r.set(func(s *Status) { s.Paired, s.Driver, s.Message = false, "", "not paired" })
	return ErrUnpaired
}

func (r *Runner) defaults() {
	if r.limits.TracePoints <= 0 {
		r.limits.TracePoints = 300
	}
	if r.limits.LapsPerRequest <= 0 {
		r.limits.LapsPerRequest = 50
	}
	if r.limits.LiveIntervalMs <= 0 {
		r.limits.LiveIntervalMs = 1000
	}
	if r.limits.FieldIntervalMs <= 0 {
		r.limits.FieldIntervalMs = 2000
	}
	if r.limits.SummaryIntervalMs <= 0 {
		r.limits.SummaryIntervalMs = 30000
	}
}

// refreshMe reads GET /me and starts or stops companions to match.
func (r *Runner) refreshMe(ctx context.Context) error {
	me, err := r.client.Me(ctx)
	if err != nil {
		return err
	}
	r.mu.Lock()
	r.me = me
	r.mu.Unlock()
	for _, h := range r.hosts {
		h.SetDriver(me.Driver.Slug, me.Driver.Name)
	}
	for _, c := range r.cfg.Registry.Companions() {
		if _, running := me.Plugin(c.Name()); running {
			h, ok := r.hosts[c.Name()]
			if !ok {
				h = host.New(c.Name(), r.client, r.player(), r.speaker(), r.board, r, r.log)
				h.SetDriver(me.Driver.Slug, me.Driver.Name)
				r.hosts[c.Name()] = h
			}
			if err := r.bus.Start(ctx, c, h); err != nil {
				r.log.Warn("a companion could not start", slog.String("reason", err.Error()))
			}
		} else if err := r.bus.Stop(c.Name()); err != nil {
			r.log.Warn("a companion did not stop cleanly", slog.String("reason", err.Error()))
		}
	}
	r.bus.Publish(&clientplugin.ServerChanged{At: time.Now(), Me: me})
	running := r.bus.Running()
	_, sound := audioRouter{r}.find()
	r.set(func(s *Status) { s.Paired, s.Driver, s.Companions, s.Sound = true, me.Driver.Name, running, sound })
	return nil
}

func (r *Runner) meLoop(ctx context.Context) {
	t := time.NewTicker(r.cfg.MeInterval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			if err := r.refreshMe(ctx); err != nil {
				r.fail(err, "GET /me")
			}
		}
	}
}

// fail handles an error from the server: a lost token ends the run, anything
// else is logged and life goes on.
func (r *Runner) fail(err error, what string) {
	if errors.Is(err, api.ErrUnauthorized) {
		r.mu.Lock()
		already := r.unpaired
		r.unpaired = true
		r.mu.Unlock()
		if !already {
			r.log.Warn("the server no longer accepts this client's token")
			r.cancel()
		}
		return
	}
	if errors.Is(err, context.Canceled) {
		return
	}
	r.log.Debug(what+" failed", slog.String("reason", err.Error()))
}

// captureLoop finds a running source, reads it into the tracker and sends
// what comes out. When a source ends it looks for the next; an explicit
// source (a replay, the demo) that ends ends the run.
func (r *Runner) captureLoop(ctx context.Context) {
	for ctx.Err() == nil {
		src := r.pick(ctx)
		if src == nil {
			return
		}
		explicit := r.isExplicit(src)
		r.capture(ctx, src)
		if explicit {
			r.cancel()
			return
		}
	}
}

func (r *Runner) pick(ctx context.Context) clientplugin.Source {
	t := time.NewTicker(time.Second)
	defer t.Stop()
	for {
		for _, s := range r.candidates() {
			if s.Running() {
				return s
			}
		}
		r.set(func(s *Status) { s.Source, s.Message = "", "waiting for a simulator" })
		select {
		case <-ctx.Done():
			return nil
		case <-t.C:
		}
	}
}

func (r *Runner) candidates() []clientplugin.Source {
	return append(append([]clientplugin.Source(nil), r.cfg.Sources...), r.cfg.Registry.Sources()...)
}

func (r *Runner) isExplicit(src clientplugin.Source) bool {
	for _, s := range r.cfg.Sources {
		if s == src {
			return true
		}
	}
	return false
}

func (r *Runner) capture(ctx context.Context, src clientplugin.Source) {
	if err := src.Open(ctx); err != nil {
		r.log.Warn("a source could not be opened", slog.String("source", src.Name()), slog.String("reason", err.Error()))
		return
	}
	defer func() {
		if err := src.Close(); err != nil {
			r.log.Warn("a source did not close cleanly", slog.String("reason", err.Error()))
		}
	}()
	r.tracker.SetSim(src.Name())
	r.set(func(s *Status) { s.Source, s.Message = src.Name(), "" })
	var rec *recorder.Writer
	defer func() {
		if rec != nil {
			if err := rec.Close(); err != nil {
				r.log.Warn("the recording did not close cleanly", slog.String("reason", err.Error()))
			}
		}
	}()
	for {
		s, err := src.Read(ctx)
		if err != nil {
			if ctx.Err() == nil {
				r.log.Info("the source ended", slog.String("source", src.Name()), slog.String("reason", err.Error()))
			}
			r.endStint(time.Now())
			return
		}
		if r.cfg.Record && rec == nil {
			rec, err = recorder.Create(recorder.FileName(filepath.Join(r.cfg.DataDir, "recordings"), src.Name(), s.At), recorder.Header{Sim: src.Name(), StartedAt: s.At, Version: r.cfg.Version})
			if err != nil {
				r.log.Warn("the session could not be recorded", slog.String("reason", err.Error()))
				r.cfg.Record = false
			}
		}
		if rec != nil {
			if err := rec.Write(s); err != nil {
				r.log.Warn("the recording stopped", slog.String("reason", err.Error()))
				rec, r.cfg.Record = nil, false
			}
		}
		r.handle(r.tracker.Add(s))
		if r.cfg.StopAfterLaps > 0 && r.tracker.Laps() >= r.cfg.StopAfterLaps {
			r.endStint(s.At)
			r.cancel()
			return
		}
	}
}

// handle sends what one sample produced.
func (r *Runner) handle(out stint.Output) {
	if out.Ended != "" && out.EndedSummary != nil {
		r.push(queue.KindSummary, out.Ended, r.trim(*out.EndedSummary))
	}
	if out.Began != nil {
		r.push(queue.KindStint, out.BeganID, out.Began)
		r.mu.Lock()
		r.stintID = out.BeganID
		r.mu.Unlock()
		r.set(func(st *Status) {
			st.Stint, st.Track, st.Lap, st.Laps, st.LastLapMs = out.BeganID, out.Began.Track, 0, 0, 0
			st.Car, st.CarClass = out.Began.Car, out.Began.CarClass
		})
	}
	for _, e := range out.Events {
		if sm, ok := e.(*clientplugin.Sampled); ok {
			r.mu.Lock()
			r.last, r.lastLap, r.lastOff, r.hasLast = sm.Sample, sm.Lap, sm.OffsetMs, true
			r.mu.Unlock()
			if sm.Lap != r.Status().Lap {
				r.set(func(st *Status) { st.Lap = sm.Lap })
			}
		}
		r.bus.Publish(e)
	}
	if out.Lap != nil {
		l := *out.Lap
		l.Trace = lap.Resample(l.Trace, r.limits.TracePoints)
		r.push(queue.KindLaps, r.tracker.Stint().ID, wire.LapBatch{Laps: []wire.Lap{l}})
		laps := r.tracker.Laps()
		r.set(func(st *Status) { st.LastLapMs, st.Laps = l.LapMs, laps })
	}
}

func (r *Runner) endStint(at time.Time) {
	id, sum, ev, ok := r.tracker.Finish(at)
	if !ok {
		return
	}
	for _, e := range ev {
		r.bus.Publish(e)
	}
	r.push(queue.KindSummary, id, r.trim(sum))
	r.mu.Lock()
	r.stintID, r.hasLast = "", false
	r.mu.Unlock()
	r.set(func(s *Status) { s.Stint = "" })
}

func (r *Runner) trim(sum wire.Summary) wire.Summary {
	sum.BestTrace = lap.Resample(sum.BestTrace, r.limits.TracePoints)
	if sum.BestTrace == nil {
		sum.BestTrace = []wire.TracePoint{}
	}
	return sum
}

func (r *Runner) push(kind queue.Kind, stintID string, body any) {
	if _, err := r.q.Push(kind, stintID, body); err != nil {
		r.log.Error("a write could not be queued; it is lost", slog.String("kind", string(kind)), slog.String("reason", err.Error()))
		return
	}
	n := r.q.Len()
	r.set(func(s *Status) { s.Queued = n })
	select {
	case r.kick <- struct{}{}:
	default:
	}
}

// drainLoop sends the queue whenever something is pushed, and otherwise
// retries with a growing pause while the server is away.
func (r *Runner) drainLoop(ctx context.Context) {
	pause := time.Second
	for {
		if r.drain(ctx) {
			pause = time.Second
		} else {
			pause = min(pause*2, time.Minute)
		}
		t := time.NewTimer(pause)
		select {
		case <-ctx.Done():
			t.Stop()
			return
		case <-r.kick:
			t.Stop()
		case <-t.C:
		}
	}
}

// drain sends what is queued. It reports whether the queue is empty or the
// server accepted everything it was offered.
func (r *Runner) drain(ctx context.Context) bool {
	_, err := r.q.Drain(ctx, r.send)
	n := r.q.Len()
	r.set(func(s *Status) { s.Queued = n })
	if err != nil {
		r.fail(err, "sending a queued write")
		return false
	}
	return true
}

func (r *Runner) send(ctx context.Context, e queue.Entry) error {
	switch e.Kind {
	case queue.KindStint:
		var s wire.Stint
		if err := unmarshal(e.Body, &s); err != nil {
			return err
		}
		_, err := r.client.PutStint(ctx, e.StintID, s, e.Key)
		return err
	case queue.KindLaps:
		var b wire.LapBatch
		if err := unmarshal(e.Body, &b); err != nil {
			return err
		}
		_, err := r.client.PostLaps(ctx, e.StintID, b, e.Key)
		return err
	case queue.KindSummary:
		var s wire.Summary
		if err := unmarshal(e.Body, &s); err != nil {
			return err
		}
		return r.client.PutSummary(ctx, e.StintID, s, e.Key)
	}
	return &api.Error{Code: wire.CodeInvalid, Message: fmt.Sprintf("a queued write of kind %q is not one this client sends", e.Kind)}
}

func (r *Runner) liveLoop(ctx context.Context) {
	t := time.NewTicker(time.Duration(r.limits.LiveIntervalMs) * time.Millisecond)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			r.mu.Lock()
			id, s, lapNo, off, ok := r.stintID, r.last, r.lastLap, r.lastOff, r.hasLast
			r.mu.Unlock()
			if !ok || id == "" {
				continue
			}
			pt := s.LivePoint(off)
			pt.Lap = lapNo
			err := r.client.PostLive(ctx, wire.LiveSample{
				StintID: id, At: s.At, Sample: pt, CurLap: lap.Resample(r.tracker.Current(), r.limits.TracePoints),
			}, ids.Key())
			if err != nil {
				r.fail(err, "POST /live")
			}
		}
	}
}

func (r *Runner) fieldLoop(ctx context.Context) {
	t := time.NewTicker(time.Duration(r.limits.FieldIntervalMs) * time.Millisecond)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			r.mu.Lock()
			id, s, ok := r.stintID, r.last, r.hasLast
			r.mu.Unlock()
			if !ok || id == "" || len(s.Field) == 0 {
				continue
			}
			_, err := r.client.PostField(ctx, wire.FieldReport{
				StintID: id, SessionType: r.tracker.Stint().Session, Flag: s.Flag, LapsTotal: s.LapsTotal, Cars: s.Field,
			}, ids.Key())
			if err != nil {
				r.fail(err, "POST /field")
			}
		}
	}
}

func (r *Runner) summaryLoop(ctx context.Context) {
	t := time.NewTicker(time.Duration(r.limits.SummaryIntervalMs) * time.Millisecond)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			r.mu.Lock()
			id, ok := r.stintID, r.hasLast
			r.mu.Unlock()
			if !ok || id == "" || !r.tracker.Active() {
				continue
			}
			r.push(queue.KindSummary, id, r.trim(r.tracker.Summary(time.Now(), false)))
		}
	}
}

func (r *Runner) set(change func(*Status)) {
	r.mu.Lock()
	change(&r.status)
	s := r.status
	s.Companions = append([]string(nil), s.Companions...)
	sort.Strings(s.Companions)
	r.mu.Unlock()
	if r.cfg.OnStatus != nil {
		r.cfg.OnStatus(s)
	}
}

// player is what companions play through: the configured one, or the router
// that finds the running companion that is a Player.
func (r *Runner) player() host.Player {
	if r.cfg.Player != nil {
		return r.cfg.Player
	}
	return audioRouter{r}
}

func (r *Runner) speaker() host.Speaker {
	if r.cfg.Speaker != nil {
		return r.cfg.Speaker
	}
	return audioRouter{r}
}

// audioRouter hands audio and words to the running companion that plays them.
// Sound is a plugin: without one running, a line is logged and lost.
type audioRouter struct{ r *Runner }

func (a audioRouter) find() (clientplugin.Player, bool) {
	for _, name := range a.r.bus.Running() {
		c, ok := a.r.cfg.Registry.Companion(name)
		if !ok {
			continue
		}
		if p, ok := c.(clientplugin.Player); ok {
			return p, true
		}
	}
	return nil, false
}

// Audible implements host.Muted: whether the companion that plays is running
// and would be heard.
func (a audioRouter) Audible() bool {
	p, ok := a.find()
	if !ok {
		return false
	}
	if m, ok := p.(clientplugin.Muted); ok {
		return m.Audible()
	}
	return true
}

// Play implements host.Player.
func (a audioRouter) Play(ctx context.Context, audio []byte, contentType string) error {
	if p, ok := a.find(); ok {
		return p.Play(ctx, audio, contentType) //nolint:wrapcheck // the player's own words.
	}
	a.r.log.Info("no plugin plays audio; a line was lost", slog.Int("bytes", len(audio)), slog.String("type", contentType))
	return nil
}

// Say implements host.Speaker.
func (a audioRouter) Say(ctx context.Context, text string) error {
	if p, ok := a.find(); ok {
		return p.Say(ctx, text) //nolint:wrapcheck // the player's own words.
	}
	a.r.log.Info("no plugin speaks; a line was lost", slog.String("text", text))
	return nil
}

// Setting implements host.Settings: a companion's kept value.
func (r *Runner) Setting(plugin, key string) string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.file.Setting(plugin, key)
}

// SetSetting implements host.Settings: the value is written to the
// configuration file with everything else the client keeps.
func (r *Runner) SetSetting(plugin, key, value string) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	next := r.file.WithSetting(plugin, key, value)
	if err := config.Save(r.cfg.DataDir, next); err != nil {
		return err
	}
	r.file = next
	return nil
}

// ErrNoControls is what Act returns for a companion whose page has none, or
// one that is not running.
var ErrNoControls = errors.New("runner: that plugin has no controls on its page")

// Act delivers a page action to the running companion that owns the page.
func (r *Runner) Act(ctx context.Context, plugin, action string, values map[string]string) error {
	running := false
	for _, name := range r.bus.Running() {
		if name == plugin {
			running = true
		}
	}
	if !running {
		return ErrNoControls
	}
	c, ok := r.cfg.Registry.Companion(plugin)
	if !ok {
		return ErrNoControls
	}
	a, ok := c.(clientplugin.Actor)
	if !ok {
		return ErrNoControls
	}
	if err := a.Act(ctx, action, values); err != nil {
		return fmt.Errorf("%s: %w", plugin, err)
	}
	return nil
}
