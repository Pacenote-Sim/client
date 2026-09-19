// Package corner finds the corners of a lap as it is driven and measures each
// one: where braking began, how hard, the apex and its speed, how long the
// throttle waited. It compares nothing against anything: that is a server
// plugin's job, on the whole team's laps.
//
// It works on the trace points the lap package produces, in wire scaling, so a
// corner's positions are already in thousandths of the lap and its speeds in
// whole km/h, which is what every consumer wants.
package corner

import (
	"github.com/pacenote-sim/protocol/wire"

	"github.com/pacenote-sim/clientplugin"
)

// The thresholds. They are in the trace's own units: percent for pedals,
// km/h for speed, hundredths of a g for acceleration, thousandths of a lap
// for distance.
const (
	// brakeOn is the pedal pressure that opens a corner.
	brakeOn = 5
	// liftThrottle and liftDrop open a corner taken on a lift alone: the
	// throttle below this while the speed has fallen by this much from its
	// recent high.
	liftThrottle = 30
	liftDrop     = 12
	// throttleBack is the pedal position that counts as back on the power;
	// closeAfter is how many samples of it close the corner.
	throttleBack = 90
	closeAfter   = 3
	// turnInG is the lateral acceleration that marks turn-in, and apex
	// candidates are read from the lateral peak when the source gives one.
	turnInG = 40
	// minDrop is the speed a corner has to cost to be a corner; minApex is the
	// apex speed below which the car is not cornering but stopped or spinning.
	minDrop = 15
	minApex = 5
	// Patterns: brake still on at the apex, throttle waiting after it.
	lateBrakeAt = 20
	earlyApexBy = 5
	slowExitLag = 30
	earlyLag    = 20
)

// Detector finds corners in one lap at a time, sample by sample. Feed it a
// lap's points in order, call EndLap at the line, repeat. It is not safe for
// concurrent use.
type Detector struct {
	expect int // corners the previous lap had, 0 when unknown
	turn   int
	done   []clientplugin.Corner
	last   bool // lap.last-corner already reported this lap

	recentMax int
	zone      *zone
	prev      wire.TracePoint
	hasPrev   bool
}

type zone struct {
	brakeAt   int
	entryKmh  int
	peakBrake int
	turnIn    int
	hasTurnIn bool
	minKmh    int
	minAt     int
	apexG     int
	apexAt    int
	apexKmh   int
	apexGear  int
	apexBrake int
	hasApexG  bool
	pastApex  bool
	exitAt    int
	exitKmh   int
	hasExit   bool
	backOn    int
	lifted    bool
}

// Report is what one sample produced.
type Report struct {
	// Passed is a corner that closed on this sample, or nil.
	Passed *clientplugin.Corner
	// LastCorner reports that Passed was the lap's final corner, by the count
	// of the previous lap. False on a stint's first lap, where the count is
	// unknown; the caller then reports the lap at the line instead.
	LastCorner bool
}

// New is a detector with no lap behind it.
func New() *Detector { return &Detector{} }

// Expect sets the corner count a lap is expected to have, when it is known
// from elsewhere than the previous lap.
func (d *Detector) Expect(n int) { d.expect = n }

// Add feeds one point of the current lap.
func (d *Detector) Add(p wire.TracePoint) Report {
	defer func() { d.prev, d.hasPrev = p, true }()
	if p.SpeedKmh > d.recentMax {
		d.recentMax = p.SpeedKmh
	}
	if d.zone == nil {
		if d.opens(p) {
			d.zone = &zone{
				brakeAt: p.DistPct, entryKmh: d.recentMax, peakBrake: p.Brake,
				minKmh: p.SpeedKmh, minAt: p.DistPct, apexAt: p.DistPct, apexKmh: p.SpeedKmh,
				apexGear: p.Gear, apexBrake: p.Brake, lifted: p.Brake < brakeOn,
			}
		}
		return Report{}
	}
	z := d.zone
	z.peakBrake = max(z.peakBrake, p.Brake)
	if p.SpeedKmh < z.minKmh {
		z.minKmh, z.minAt = p.SpeedKmh, p.DistPct
	}
	if g := abs(p.LatG); g >= turnInG && !z.hasTurnIn {
		z.hasTurnIn, z.turnIn = true, p.Brake
	}
	if g := abs(p.LatG); g > z.apexG && !z.pastApex {
		z.apexG, z.hasApexG = g, true
		z.apexAt, z.apexKmh, z.apexGear, z.apexBrake = p.DistPct, p.SpeedKmh, p.Gear, p.Brake
		z.hasExit = false // the throttle is picked up after the apex, not before
	}
	if !z.hasApexG && p.SpeedKmh <= z.minKmh {
		// No lateral channel: the apex is the slowest point.
		z.apexAt, z.apexKmh, z.apexGear, z.apexBrake = p.DistPct, p.SpeedKmh, p.Gear, p.Brake
		z.hasExit = false
	}
	if p.Throttle >= throttleBack {
		z.backOn++
		if !z.hasExit && p.SpeedKmh >= z.minKmh {
			z.hasExit, z.exitAt, z.exitKmh = true, p.DistPct, p.SpeedKmh
		}
	} else {
		z.backOn = 0
	}
	if p.SpeedKmh > z.minKmh+minDrop/2 {
		z.pastApex = true
	}
	if z.backOn >= closeAfter && p.Brake < brakeOn {
		return d.close(p)
	}
	return Report{}
}

// opens reports that this point begins a corner: the brake comes on, or the
// throttle lifts while the speed has fallen from its recent high.
func (d *Detector) opens(p wire.TracePoint) bool {
	if p.Brake >= brakeOn {
		return true
	}
	return p.Throttle <= liftThrottle && d.recentMax-p.SpeedKmh >= liftDrop
}

func (d *Detector) close(p wire.TracePoint) Report {
	z := d.zone
	d.zone = nil
	d.recentMax = p.SpeedKmh
	if z.entryKmh-z.minKmh < minDrop || z.minKmh < minApex {
		return Report{}
	}
	if !z.hasTurnIn {
		z.turnIn = z.peakBrake
	}
	if !z.hasExit {
		z.exitAt, z.exitKmh = p.DistPct, p.SpeedKmh
	}
	d.turn++
	c := clientplugin.Corner{
		Turn:           d.turn,
		ApexPct:        z.apexAt,
		ApexKmh:        z.apexKmh,
		MinKmh:         z.minKmh,
		ExitKmh:        z.exitKmh,
		BrakeAtPct:     z.brakeAt,
		PeakBrakePct:   z.peakBrake,
		TurnInBrakePct: z.turnIn,
		BrakeAtApex:    z.apexBrake,
		ThrottleLag:    lag(z.apexAt, z.exitAt),
		GearAtApex:     z.apexGear,
	}
	if z.lifted && z.peakBrake < brakeOn {
		c.BrakeAtPct, c.PeakBrakePct, c.TurnInBrakePct, c.BrakeAtApex = 0, 0, 0, 0
	}
	c.Pattern = pattern(c)
	d.done = append(d.done, c)
	rep := Report{Passed: &c}
	if d.expect > 0 && d.turn == d.expect && !d.last {
		rep.LastCorner, d.last = true, true
	}
	return rep
}

// pattern names the shape of the mistake the measurements show, or nothing.
func pattern(c clientplugin.Corner) wire.CornerPattern {
	switch {
	case c.BrakeAtApex >= lateBrakeAt:
		return wire.PatternLateBraking
	case c.MinKmh <= c.ApexKmh-earlyApexBy && c.ThrottleLag >= earlyLag:
		return wire.PatternEarlyApex
	case c.ThrottleLag >= slowExitLag:
		return wire.PatternSlowExit
	}
	return ""
}

// EndLap closes the lap at the line. It returns every corner of the lap and
// whether the last corner was already reported through Add; when it was not,
// the caller reports the lap now. The lap's count becomes what the next lap
// expects. A corner still open at the line is dropped: the line is on a
// straight, and a corner that spans it is a corner the detector cannot place.
func (d *Detector) EndLap() (corners []clientplugin.Corner, lastReported bool) {
	corners = append([]clientplugin.Corner(nil), d.done...)
	lastReported = d.last
	if len(corners) > 0 {
		d.expect = len(corners)
	}
	d.turn, d.done, d.last, d.zone = 0, d.done[:0], false, nil
	d.recentMax = 0
	if d.hasPrev {
		d.recentMax = d.prev.SpeedKmh
	}
	return corners, lastReported
}

// Corners is what has closed so far this lap.
func (d *Detector) Corners() []clientplugin.Corner {
	return append([]clientplugin.Corner(nil), d.done...)
}

// lag is how far after the apex the throttle came back, in thousandths of the
// lap. A driver already on the power at the apex is not late, so an exit at or
// before the apex is no lag at all rather than most of a lap: subtracting one
// from the other and letting it wrap is how a corner comes to be reported with
// three and a half kilometres between its apex and its exit.
func lag(apexAt, exitAt int) int {
	d := forward(apexAt, exitAt)
	if d > MaxLagPct {
		return 0
	}
	return d
}

// MaxLagPct bounds a throttle lag: a fifth of the lap after the apex is not a
// corner's exit, it is a measurement that went wrong.
const MaxLagPct = 200

// forward is the distance from a to b round the lap, in thousandths.
func forward(a, b int) int {
	if b >= a {
		return b - a
	}
	return 1000 - a + b
}

func abs(v int) int {
	if v < 0 {
		return -v
	}
	return v
}

// Due lists the corners of the previous lap whose cue is due at this position
// and has not been raised yet on this lap.
//
// A cue is due when two things hold: the car is past the corner before this
// one, because a line about the next corner is nothing to a driver still in
// this one; and ready says there is no more room to wait. raised is the turns
// already cued this lap and Due adds the ones it returns, so a cue is raised
// once however the car moves afterwards. That is why this is a condition and
// not a point on the track: a point computed from the speed of the moment
// moves as the car accelerates, and a point that moves can slip behind the car
// between two samples and never be crossed at all.
func Due(prev []clientplugin.Corner, pct int, raised map[int]bool, ready func(c clientplugin.Corner, aheadPct int) bool) []clientplugin.Corner {
	var out []clientplugin.Corner
	for _, c := range prev {
		if raised[c.Turn] {
			continue
		}
		ahead := Ahead(ActPoint(c), pct)
		if gap, ok := GapBefore(prev, c); ok && ahead > gap {
			continue // the corner before this one is still in front of the car
		}
		if !ready(c, ahead) {
			continue
		}
		raised[c.Turn] = true
		out = append(out, c)
	}
	return out
}

// ActPoint is where the driver has to act on a corner: the braking point
// measured on the lap before, or the apex of a corner taken without brakes.
func ActPoint(c clientplugin.Corner) int {
	if c.BrakeAtPct > 0 {
		return c.BrakeAtPct
	}
	return c.ApexPct
}

// Ahead is how far a point is in front of a position, in thousandths, going
// forward round the lap.
func Ahead(point, pct int) int {
	d := point - pct
	for d < 0 {
		d += 1000
	}
	return d
}

// GapBefore is how far the nearest corner behind this one sits from the point
// its driver acts at. False on a lap with one corner, which has nothing behind
// it to wait for.
func GapBefore(prev []clientplugin.Corner, c clientplugin.Corner) (int, bool) {
	if len(prev) < 2 {
		return 0, false
	}
	point, best, found := ActPoint(c), 0, false
	for _, p := range prev {
		if p.Turn == c.Turn {
			continue
		}
		d := point - p.ApexPct
		for d <= 0 {
			d += 1000
		}
		if !found || d < best {
			best, found = d, true
		}
	}
	return best, found
}
