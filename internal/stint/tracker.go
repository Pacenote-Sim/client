// Package stint turns the stream of samples into what the two sides want: for
// the server, the stint document, each completed lap and the summary; for the
// companions, the events. It owns the lap accumulator and the corner detector
// and decides where one stint ends and the next begins. It does no IO: the
// runner feeds it samples and sends what comes out.
package stint

import (
	"math"
	"sync"
	"time"

	"github.com/pacenote-sim/protocol/wire"

	"github.com/pacenote-sim/clientplugin"

	"github.com/pacenote-sim/client/internal/corner"
	"github.com/pacenote-sim/client/internal/ids"
	"github.com/pacenote-sim/client/internal/lap"
)

// GarageTimeout is how long a driver is out of the car before the stint is
// over. A pit stop is minutes; a coffee is longer.
const GarageTimeout = 5 * time.Minute

// LeadSeconds is how long before the braking point a corner's cue is raised.
//
// It is the room for the longest line a coach may write, not the moment it is
// spoken: a companion holds a shorter line and starts it at the last moment
// that still ends before the driver brakes. Raising the cue this early is what
// lets a long straight carry a sentence worth hearing, and costs nothing on a
// short one, where the companion finds no room and says less.
const LeadSeconds = 25

// MinLeadMetres is the least lead, for a slow car.
const MinLeadMetres = 100

// DefaultLeadPct is the lead when the track length is unknown.
const DefaultLeadPct = 60

// Output is what one sample produced.
type Output struct {
	// Events for the companions, in order.
	Events []clientplugin.Event
	// Began is the stint document to send when a stint began on this sample.
	Began *wire.Stint
	// BeganID is its id.
	BeganID string
	// Lap is a completed lap to send, at the source's full rate.
	Lap *wire.Lap
	// Ended is the id of a stint that ended on this sample; its final summary
	// is due. It is set before Began when one stint gives way to another.
	Ended        string
	EndedSummary *wire.Summary
}

// Tracker is the state of the current stint. It is safe for concurrent use:
// the capture loop adds samples while the live and summary loops read.
type Tracker struct {
	mu  sync.Mutex
	acc *lap.Accumulator
	det *corner.Detector
	sim string

	active    bool
	stint     clientplugin.Stint
	first     clientplugin.Sample
	last      clientplugin.Sample
	lastOnTrk time.Time
	laps      []lap.Completed
	best      *lap.Completed
	prev      []clientplugin.Corner
	// cued is the turns whose cue has been raised on the lap being driven,
	// cleared when a lap ends: a corner is cued once a lap.
	cued       map[int]bool
	lastPct    int
	reported   bool
	sumWeather clientplugin.Weather
	nWeather   int
	fuelStart  float64
	incidents  int
	topKmh     float64
}

// New is a tracker with no stint.
func New() *Tracker { return &Tracker{} }

// Active reports whether a stint is under way.
func (t *Tracker) Active() bool {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.active
}

// Stint is the current stint, meaningful when Active.
func (t *Tracker) Stint() clientplugin.Stint {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.stint
}

// Laps is how many laps the current stint completed.
func (t *Tracker) Laps() int {
	t.mu.Lock()
	defer t.mu.Unlock()
	return len(t.laps)
}

// Current is the trace of the lap so far, for the live stream.
func (t *Tracker) Current() []wire.TracePoint {
	t.mu.Lock()
	defer t.mu.Unlock()
	if !t.active || t.acc == nil {
		return nil
	}
	return t.acc.Current()
}

// Add feeds one sample.
func (t *Tracker) Add(s clientplugin.Sample) Output {
	t.mu.Lock()
	defer t.mu.Unlock()
	var out Output
	if t.active && t.changed(s) {
		id, sum, ev := t.finish(s.At)
		out.Ended, out.EndedSummary = id, &sum
		out.Events = append(out.Events, ev...)
	}
	if t.active && !s.OnTrack && s.At.Sub(t.lastOnTrk) > GarageTimeout {
		id, sum, ev := t.finish(s.At)
		out.Ended, out.EndedSummary = id, &sum
		out.Events = append(out.Events, ev...)
	}
	if !t.active {
		if !s.OnTrack {
			return out
		}
		doc, id := t.begin(s)
		out.Began, out.BeganID = &doc, id
		out.Events = append(out.Events, &clientplugin.StintStarted{At: s.At, Stint: t.stint})
	}
	if s.OnTrack {
		t.lastOnTrk = s.At
	}
	t.last = s
	t.observe(s)

	lapNo, offset, done := t.acc.Add(s)
	pct := int(math.Round(s.LapDistPct * 1000))
	out.Events = append(out.Events, &clientplugin.Sampled{Stint: t.stint, Sample: s, Lap: lapNo, OffsetMs: offset})
	if lapNo > 0 {
		rep := t.det.Add(s.TracePoint(offset))
		if rep.Passed != nil {
			out.Events = append(out.Events, &clientplugin.CornerPassed{At: s.At, Stint: t.stint, Lap: lapNo, Corner: *rep.Passed})
		}
		if rep.LastCorner {
			t.reported = true
			out.Events = append(out.Events, &clientplugin.LapLastCorner{At: s.At, Stint: t.stint, Lap: lapNo, ElapsedMs: offset, Corners: t.det.Corners()})
		}
		for _, c := range corner.Due(t.prev, pct, t.cued, t.ready(s.SpeedKmh)) {
			out.Events = append(out.Events, &clientplugin.CornerApproaching{
				At: s.At, Stint: t.stint, Lap: lapNo, Turn: c.Turn, ApexPct: c.ApexPct, MetresToApex: t.metres(c.ApexPct, pct),
			})
		}
	}
	t.lastPct = pct
	if done != nil {
		corners, reported := t.det.EndLap()
		if !reported && len(corners) > 0 {
			out.Events = append(out.Events, &clientplugin.LapLastCorner{At: s.At, Stint: t.stint, Lap: done.Number, ElapsedMs: done.LapMs, Corners: corners})
		}
		t.reported = false
		t.prev = corners
		t.cued = map[int]bool{}
		t.laps = append(t.laps, *done)
		if done.Kind == wire.KindClean && (t.best == nil || done.LapMs < t.best.LapMs) {
			t.best = done
		}
		// The server's lap carries no corners: its corner shape is a comparison
		// against a reference, and this client compares nothing. The corners go
		// to the companions, and through engineer's to the team's coach.
		out.Lap = &wire.Lap{
			Number: done.Number, LapMs: done.LapMs, Kind: done.Kind, StartedAt: done.StartedAt, Trace: done.Trace,
		}
		out.Events = append(out.Events, &clientplugin.LapCompleted{
			At: s.At, Stint: t.stint, Lap: done.Number, LapMs: done.LapMs, LapKind: done.Kind, StartedAt: done.StartedAt,
			Trace: done.Trace, SectorMs: done.SectorMs, Corners: corners,
		})
	}
	return out
}

// Finish ends the current stint, for a source that closed or an app that is
// closing. It returns the stint's id, its final summary and the events.
func (t *Tracker) Finish(at time.Time) (id string, summary wire.Summary, events []clientplugin.Event, ok bool) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if !t.active {
		return "", wire.Summary{}, nil, false
	}
	id, summary, events = t.finish(at)
	return id, summary, events, true
}

// Summary is the current stint's summary so far. final marks it as the last.
func (t *Tracker) Summary(at time.Time, final bool) wire.Summary {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.summary(at, final)
}

func (t *Tracker) summary(at time.Time, final bool) wire.Summary {
	sum := wire.Summary{
		Laps:        len(t.laps),
		Incidents:   t.incidents,
		TopSpeedKmh: int(math.Round(t.topKmh)),
		Conditions: wire.Conditions{
			Skies: t.sumWeather.Skies, Wetness: t.sumWeather.Wetness,
		},
		CarState: wire.CarState{
			FuelLevelL: t.last.FuelL,
			TyreTempC:  wire.TyreTemps{LF: t.last.Tyres.LF, RF: t.last.Tyres.RF, LR: t.last.Tyres.LR, RR: t.last.Tyres.RR},
		},
	}
	if t.nWeather > 0 {
		n := float64(t.nWeather)
		sum.Conditions.Skies = int(math.Round(float64(t.sumWeather.Skies) / n))
		sum.Conditions.Wetness = int(math.Round(float64(t.sumWeather.Wetness) / n))
		sum.Conditions.WindKmh = round1(t.sumWeather.WindKmh / n)
		sum.Conditions.Humidity = round1(t.sumWeather.Humidity / n)
		sum.Conditions.TrackTempC = round1(t.sumWeather.TrackTempC / n)
		sum.Conditions.AirTempC = round1(t.sumWeather.AirTempC / n)
	}
	if t.fuelStart > 0 && t.last.FuelL > 0 {
		sum.CarState.FuelUsedL = round1(math.Max(0, t.fuelStart-t.last.FuelL))
	}
	var clean []int
	for _, l := range t.laps {
		if l.Kind == wire.KindClean {
			clean = append(clean, l.LapMs)
		}
	}
	if len(clean) > 0 {
		total := 0
		for _, ms := range clean {
			total += ms
		}
		sum.AvgLapMs = total / len(clean)
		sum.BestLapMs = t.best.LapMs
		sum.BestTrace = t.best.Trace
		sum.ConsistencyPct = consistency(clean, sum.AvgLapMs)
	}
	if final {
		end := at
		sum.FinishedAt = &end
	}
	return sum
}

func (t *Tracker) begin(s clientplugin.Sample) (wire.Stint, string) {
	t.acc, t.det = lap.New(), corner.New()
	t.active = true
	t.first, t.last, t.lastOnTrk = s, s, s.At
	t.laps, t.best, t.prev = nil, nil, nil
	t.cued = map[int]bool{}
	t.lastPct, t.reported = int(math.Round(s.LapDistPct*1000)), false
	t.sumWeather, t.nWeather = clientplugin.Weather{}, 0
	t.fuelStart, t.incidents, t.topKmh = s.FuelL, 0, 0
	id := ids.Stint(s.At)
	t.stint = clientplugin.Stint{
		ID: id, Sim: t.sim, Track: s.Track, TrackID: s.TrackID, Car: s.Car, CarClass: s.CarClass,
		Session: sessionOf(s), TrackLengthM: s.TrackLengthM, Sectors: append([]float64(nil), s.Sectors...),
		SetupOpen: s.SetupOpen, StartedAt: s.At,
	}
	doc := wire.Stint{
		Sim: t.stint.Sim, Track: s.Track, TrackID: s.TrackID, Car: s.Car, CarClass: s.CarClass,
		SessionType: t.stint.Session, StartedAt: s.At, Sectors: t.stint.Sectors,
	}
	return doc, id
}

// SetSim names the simulator the samples come from; the stint document
// carries it. It is the source's name, set by the runner before samples flow.
func (t *Tracker) SetSim(sim string) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.sim = sim
}

func (t *Tracker) finish(at time.Time) (string, wire.Summary, []clientplugin.Event) {
	id := t.stint.ID
	sum := t.summary(at, true)
	ev := []clientplugin.Event{&clientplugin.StintFinished{At: at, Stint: t.stint, Laps: len(t.laps)}}
	t.active = false
	return id, sum, ev
}

// changed reports that this sample belongs to another stint: another track,
// car or session.
func (t *Tracker) changed(s clientplugin.Sample) bool {
	if !s.OnTrack {
		return false
	}
	return s.TrackID != t.stint.TrackID || s.Car != t.stint.Car || sessionOf(s) != t.stint.Session
}

func (t *Tracker) observe(s clientplugin.Sample) {
	t.sumWeather.Skies += s.Weather.Skies
	t.sumWeather.Wetness += s.Weather.Wetness
	t.sumWeather.WindKmh += s.Weather.WindKmh
	t.sumWeather.Humidity += s.Weather.Humidity
	t.sumWeather.TrackTempC += s.Weather.TrackTempC
	t.sumWeather.AirTempC += s.Weather.AirTempC
	t.nWeather++
	if t.fuelStart == 0 && s.FuelL > 0 {
		t.fuelStart = s.FuelL
	}
	t.incidents = max(t.incidents, s.Incidents)
	t.topKmh = math.Max(t.topKmh, s.SpeedKmh)
}

// ready says a corner's cue can wait no longer: the point the driver acts at
// is within LeadSeconds of travel at the speed of the moment. Without a track
// length there are no metres to judge by and the lap's own thousandths are
// used instead.
func (t *Tracker) ready(speedKmh float64) func(clientplugin.Corner, int) bool {
	return func(_ clientplugin.Corner, aheadPct int) bool {
		if t.stint.TrackLengthM == 0 || speedKmh <= 1 {
			return aheadPct <= DefaultLeadPct
		}
		metres := float64(aheadPct) * float64(t.stint.TrackLengthM) / 1000
		return metres <= math.Max(MinLeadMetres, speedKmh/3.6*LeadSeconds)
	}
}

func (t *Tracker) metres(apexPct, pct int) int {
	if t.stint.TrackLengthM == 0 {
		return 0
	}
	d := apexPct - pct
	if d < 0 {
		d += 1000
	}
	return d * t.stint.TrackLengthM / 1000
}

func sessionOf(s clientplugin.Sample) wire.SessionType {
	switch s.Session {
	case wire.SessionPractice, wire.SessionQualifying, wire.SessionRace, wire.SessionTesting:
		return s.Session
	}
	return wire.SessionPractice
}

// consistency is 100 minus the spread of the lap times as a percentage of the
// average, clamped to 0…100. One lap is perfectly consistent.
func consistency(ms []int, avg int) int {
	if len(ms) < 2 || avg == 0 {
		return 100
	}
	var sq float64
	for _, v := range ms {
		d := float64(v - avg)
		sq += d * d
	}
	sd := math.Sqrt(sq / float64(len(ms)))
	return int(math.Round(math.Max(0, math.Min(100, 100-sd/float64(avg)*100))))
}

func round1(v float64) float64 { return math.Round(v*10) / 10 }
