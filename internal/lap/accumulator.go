// Package lap turns a stream of samples into laps: where each lap begins and
// ends, its time to the millisecond, its trace at the source's full rate, its
// sector times and its kind. It knows nothing about simulators, servers or
// corners; it is fed samples and hands back laps.
//
// A lap begins and ends at the line. The line is crossed between two samples,
// so the crossing time is interpolated between them from the distance each
// sample sits from it, which makes a lap time good to a few milliseconds at
// any sample rate rather than to one sample period.
package lap

import (
	"time"

	"github.com/pacenote-sim/protocol/wire"

	"github.com/pacenote-sim/clientplugin"
)

// MaxTracePoints bounds a lap's trace in memory. A lap longer than this at the
// source's rate is thinned by half and kept thinning, so a fifteen-minute
// lap of a stuck car costs the same as a two-minute one.
const MaxTracePoints = 16384

// Completed is a lap that has crossed the line.
type Completed struct {
	// Number counts the laps this accumulator has completed, from 1. It is the
	// lap number every event uses; the simulator's own counter is on the samples.
	Number int
	// LapMs is line to line. StartedAt is when the lap began.
	LapMs     int
	StartedAt time.Time
	// Kind is the protocol's: clean, in, out or invalid.
	Kind wire.Kind
	// Trace is every sample of the lap in wire scaling, offsets from the lap's start.
	Trace []wire.TracePoint
	// SectorMs is each sector's time in the order of the boundaries given, or
	// nil when there were no boundaries or a boundary was never crossed.
	SectorMs []int
}

// Accumulator builds laps from samples. It is not safe for concurrent use;
// one goroutine feeds it.
type Accumulator struct {
	number  int
	started bool
	startAt time.Time
	prev    clientplugin.Sample
	hasPrev bool

	trace   []wire.TracePoint
	stride  int
	skipped int

	onPitAtStart bool
	onPitAtEnd   bool
	offTrack     bool
	sawPit       bool

	sectors []float64
	crossed []time.Time
	simLap  int
}

// New is an empty accumulator. Everything before the first line crossing is
// the out lap and is not a lap.
func New() *Accumulator {
	return &Accumulator{stride: 1}
}

// Add feeds one sample. It returns the lap the sample belongs to (0 before the
// first crossing), the time since that lap began in milliseconds, and the lap
// just completed when this sample is the first of the next one.
func (a *Accumulator) Add(s clientplugin.Sample) (lap, offsetMs int, done *Completed) {
	if !a.hasPrev {
		a.prev, a.hasPrev = s, true
		a.startAt = s.At
		a.simLap = s.Lap
		a.sectors = append([]float64(nil), s.Sectors...)
		a.note(s)
		return 0, 0, nil
	}
	defer func() { a.prev = s }()

	switch {
	case crossedLine(a.prev, s):
		at := crossingAt(a.prev, s, 1)
		if a.started {
			done = a.finish(at)
		}
		a.begin(s, at)
		a.note(s)
		a.append(s, at)
		return a.number, int(s.At.Sub(at).Milliseconds()), done
	case s.Lap != a.simLap && a.started:
		// The simulator's counter moved without the car crossing the line: a
		// reset to the pits, a tow. What was driven is not a lap.
		a.abandon(s)
		return 0, 0, nil
	}

	a.crossSectors(a.prev, s)
	a.note(s)
	if a.started {
		a.append(s, a.startAt)
		return a.number, int(s.At.Sub(a.startAt).Milliseconds()), nil
	}
	return 0, int(s.At.Sub(a.startAt).Milliseconds()), nil
}

// Lap is the current lap number, 0 before the first crossing.
func (a *Accumulator) Lap() int { return a.number }

// Started is when the current lap began, or when the first sample arrived.
func (a *Accumulator) Started() time.Time { return a.startAt }

// Current is the trace of the lap so far, in wire scaling. It is the caller's copy.
func (a *Accumulator) Current() []wire.TracePoint {
	return append([]wire.TracePoint(nil), a.trace...)
}

func (a *Accumulator) begin(s clientplugin.Sample, at time.Time) {
	a.number++
	a.started = true
	a.startAt = at
	a.simLap = s.Lap
	a.trace = a.trace[:0]
	a.stride, a.skipped = 1, 0
	a.onPitAtStart, a.onPitAtEnd, a.offTrack, a.sawPit = s.OnPitRoad, false, false, false
	a.sectors = append(a.sectors[:0], s.Sectors...)
	a.crossed = make([]time.Time, len(a.sectors))
	if len(a.crossed) > 0 {
		a.crossed[0] = at
	}
}

func (a *Accumulator) abandon(s clientplugin.Sample) {
	a.number-- // the lap that was in progress was never a lap; its number is reused
	a.started = false
	a.startAt = s.At
	a.simLap = s.Lap
	a.trace = a.trace[:0]
}

func (a *Accumulator) note(s clientplugin.Sample) {
	if s.OnPitRoad {
		a.sawPit = true
	}
	a.onPitAtEnd = s.OnPitRoad
	if !s.OnTrack {
		a.offTrack = true
	}
}

func (a *Accumulator) append(s clientplugin.Sample, startAt time.Time) {
	a.skipped++
	if a.skipped < a.stride {
		return
	}
	a.skipped = 0
	if len(a.trace) >= MaxTracePoints {
		half := a.trace[:0]
		for i := 0; i < len(a.trace); i += 2 {
			half = append(half, a.trace[i])
		}
		a.trace = half
		a.stride *= 2
	}
	a.trace = append(a.trace, s.TracePoint(int(s.At.Sub(startAt).Milliseconds())))
}

func (a *Accumulator) crossSectors(prev, s clientplugin.Sample) {
	if !a.started || len(a.crossed) != len(a.sectors) {
		return // the out lap has no sectors to time
	}
	for i := 1; i < len(a.sectors); i++ {
		b := a.sectors[i]
		if prev.LapDistPct < b && s.LapDistPct >= b && a.crossed[i].IsZero() {
			a.crossed[i] = crossingAt(prev, s, b)
		}
	}
}

func (a *Accumulator) finish(at time.Time) *Completed {
	c := &Completed{
		Number:    a.number,
		LapMs:     int(at.Sub(a.startAt).Milliseconds()),
		StartedAt: a.startAt,
		Kind:      a.kind(),
		Trace:     append([]wire.TracePoint(nil), a.trace...),
	}
	if n := len(a.sectors); n > 0 {
		c.SectorMs = make([]int, n)
		for i := range n {
			if a.crossed[i].IsZero() {
				c.SectorMs = nil
				break
			}
			end := at
			if i+1 < n {
				end = a.crossed[i+1]
				if end.IsZero() {
					c.SectorMs = nil
					break
				}
			}
			c.SectorMs[i] = int(end.Sub(a.crossed[i]).Milliseconds())
		}
	}
	return c
}

func (a *Accumulator) kind() wire.Kind {
	switch {
	case a.onPitAtStart:
		return wire.KindOut
	case a.onPitAtEnd:
		return wire.KindIn
	case a.offTrack || a.sawPit:
		return wire.KindInvalid
	}
	return wire.KindClean
}

// crossedLine reports that the car passed the line between two samples: its
// lap distance wrapped from the end of the lap to the start.
func crossedLine(prev, s clientplugin.Sample) bool {
	return prev.LapDistPct > 0.5 && s.LapDistPct < 0.5 && prev.LapDistPct-s.LapDistPct > 0.5
}

// crossingAt interpolates when the car passed the point at fraction b of the
// lap, between two samples. b = 1 is the line, where the fraction wraps.
func crossingAt(prev, s clientplugin.Sample, b float64) time.Time {
	before := b - prev.LapDistPct
	after := s.LapDistPct - b
	if b >= 1 {
		after = s.LapDistPct
	}
	total := before + after
	if total <= 0 {
		return s.At
	}
	return prev.At.Add(time.Duration(float64(s.At.Sub(prev.At)) * before / total))
}

// Resample thins a trace to at most n points, evenly by distance round the lap,
// keeping the first and the last. It picks the nearest sample to each target
// and invents no values; a chart drawn from it has a point every so many
// metres, which is where a chart wants them.
//
// A trace already short enough comes back as it was given, not copied: the
// caller has it already and nobody writes to a trace once it is measured.
func Resample(pts []wire.TracePoint, n int) []wire.TracePoint {
	if n <= 0 {
		return nil
	}
	if len(pts) <= n {
		return pts
	}
	if n == 1 {
		return []wire.TracePoint{pts[0]}
	}
	// One pass. j walks forward to the sample nearest each target and never
	// goes back, because both the targets and the lap are in order; taken is
	// the index j stopped at last, so a target that lands on a sample already
	// taken is skipped rather than repeating it.
	out := make([]wire.TracePoint, 0, n)
	j, taken := 0, -1
	for k := range n {
		target := k * 1000 / (n - 1)
		for j+1 < len(pts) && abs(pts[j+1].DistPct-target) <= abs(pts[j].DistPct-target) {
			j++
		}
		if j <= taken {
			continue
		}
		out = append(out, pts[j])
		taken = j
	}
	if taken != len(pts)-1 {
		out = append(out, pts[len(pts)-1])
	}
	return out
}

func abs(v int) int {
	if v < 0 {
		return -v
	}
	return v
}
