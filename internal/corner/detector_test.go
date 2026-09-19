package corner_test

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/pacenote-sim/protocol/wire"

	"github.com/pacenote-sim/clientplugin"
	"github.com/pacenote-sim/clientplugin/clientplugintest"
	demo "github.com/pacenote-sim/clientplugin/examples/source-demo"

	"github.com/pacenote-sim/client/internal/corner"
)

// A trace is built from segments: n points, speed from v0 to v1, fixed pedals
// and lateral g, one thousandth of a lap per point.
type seg struct {
	n      int
	v0, v1 int
	thr    int
	brk    int
	lg     int
}

func trace(startPct int, segs ...seg) []wire.TracePoint {
	var out []wire.TracePoint
	p := startPct
	for _, s := range segs {
		for i := range s.n {
			v := s.v0
			if s.n > 1 {
				v = s.v0 + (s.v1-s.v0)*i/(s.n-1)
			}
			out = append(out, wire.TracePoint{OffsetMs: len(out) * 50, DistPct: p % 1000, SpeedKmh: v, Throttle: s.thr, Brake: s.brk, LatG: s.lg, Gear: 2 + v/60})
			p++
		}
	}
	return out
}

func run(d *corner.Detector, pts []wire.TracePoint) (passed []clientplugin.Corner, lastAt int) {
	lastAt = -1
	for i, p := range pts {
		rep := d.Add(p)
		if rep.Passed != nil {
			passed = append(passed, *rep.Passed)
		}
		if rep.LastCorner {
			lastAt = i
		}
	}
	return passed, lastAt
}

// The demo source's lap has three corners, and every measurement of each is
// where the demo puts it.
func TestTheDemoLapHasThreeCorners(t *testing.T) {
	t.Parallel()
	r := require.New(t)

	src := demo.New()
	src.LapSeconds = 60
	clock := time.Date(2026, 9, 18, 10, 0, 0, 0, time.UTC)
	src.Sleep = func(context.Context, time.Duration) error {
		clock = clock.Add(src.Period)
		return nil
	}
	demo.SetNow(src, func() time.Time { return clock })
	samples, err := clientplugintest.Drain(context.Background(), src, 1200)
	r.NoError(err)

	d := corner.New()
	pts := make([]wire.TracePoint, 0, len(samples))
	for i, s := range samples {
		pts = append(pts, s.TracePoint(i*50))
	}
	passed, lastAt := run(d, pts)
	r.Len(passed, 3)
	r.Equal(-1, lastAt, "the first lap has no expectation")
	for i, c := range passed {
		r.Equal(i+1, c.Turn)
		r.InDelta([]int{250, 550, 800}[i], c.ApexPct, 3, "the apex is where the demo puts it")
		r.InDelta(100, c.ApexKmh, 3)
		r.Equal(c.ApexKmh, c.MinKmh, "no lateral channel: the apex is the slowest point")
		r.Less(c.BrakeAtPct, c.ApexPct)
		r.Equal(100, c.PeakBrakePct)
		r.Greater(c.ExitKmh, c.ApexKmh)
		r.Positive(c.ThrottleLag)
		r.Positive(c.GearAtApex)
		r.Empty(c.Pattern)
		r.Equal(c.ApexKmh, c.Wire().ApexKmh)
	}
	corners, reported := d.EndLap()
	r.Len(corners, 3)
	r.False(reported)

	// The second lap knows to expect three, and says so on the third.
	passed, lastAt = run(d, pts)
	r.Len(passed, 3)
	r.Positive(lastAt)
	r.Equal(3, passed[2].Turn)
	corners, reported = d.EndLap()
	r.Len(corners, 3)
	r.True(reported)
}

func TestWhatIsAndIsNotACorner(t *testing.T) {
	t.Parallel()
	r := require.New(t)

	// A braked corner with a lateral channel: turn-in is read at 0.4 g, the
	// apex at the lateral peak, the exit where the throttle is back.
	braked := trace(100,
		seg{n: 20, v0: 200, v1: 200, thr: 100},
		seg{n: 10, v0: 200, v1: 120, brk: 90, lg: 10},
		seg{n: 5, v0: 120, v1: 100, brk: 40, lg: 60},
		seg{n: 5, v0: 100, v1: 95, brk: 0, thr: 20, lg: 120},
		seg{n: 10, v0: 95, v1: 140, thr: 100, lg: 30},
		seg{n: 20, v0: 140, v1: 200, thr: 100},
	)
	passed, _ := run(corner.New(), braked)
	r.Len(passed, 1)
	c := passed[0]
	r.Equal(120, c.BrakeAtPct)
	r.Equal(90, c.PeakBrakePct)
	r.Equal(40, c.TurnInBrakePct, "the pressure when the car started turning")
	r.InDelta(137, c.ApexPct, 3, "at the lateral peak")
	r.Equal(95, c.MinKmh)
	r.Equal(0, c.BrakeAtApex)
	r.Equal(140, c.ApexPct+c.ThrottleLag, "the throttle came back at 140")
	r.Positive(c.ExitKmh)

	// A lift with no brake is a corner with no brake numbers.
	lift := trace(300,
		seg{n: 20, v0: 220, v1: 220, thr: 100},
		seg{n: 10, v0: 220, v1: 190, thr: 10},
		seg{n: 10, v0: 190, v1: 220, thr: 100},
		seg{n: 10, v0: 220, v1: 220, thr: 100},
	)
	passed, _ = run(corner.New(), lift)
	r.Len(passed, 1)
	r.Zero(passed[0].PeakBrakePct)
	r.Zero(passed[0].BrakeAtPct)
	r.Equal(190, passed[0].MinKmh)

	// A dab of brake that costs nothing is not a corner; a car that stops is not cornering.
	dab := trace(500,
		seg{n: 10, v0: 200, v1: 200, thr: 100},
		seg{n: 3, v0: 200, v1: 195, brk: 20},
		seg{n: 10, v0: 195, v1: 200, thr: 100},
	)
	passed, _ = run(corner.New(), dab)
	r.Empty(passed)
	stopped := trace(600,
		seg{n: 10, v0: 100, v1: 100, thr: 100},
		seg{n: 10, v0: 100, v1: 0, brk: 100},
		seg{n: 10, v0: 0, v1: 0},
		seg{n: 10, v0: 0, v1: 100, thr: 100},
	)
	passed, _ = run(corner.New(), stopped)
	r.Empty(passed)

	// A corner still open at the line is dropped and the lap's count is what closed.
	d := corner.New()
	run(d, braked)
	run(d, trace(990, seg{n: 5, v0: 200, v1: 150, brk: 80}))
	corners, _ := d.EndLap()
	r.Len(corners, 1)
	r.Empty(d.Corners())
}

func TestPatternsNameTheMistake(t *testing.T) {
	t.Parallel()
	r := require.New(t)

	// Brake still on at the apex.
	late := trace(100,
		seg{n: 10, v0: 200, v1: 200, thr: 100},
		seg{n: 10, v0: 200, v1: 100, brk: 90, lg: 20},
		seg{n: 4, v0: 100, v1: 90, brk: 40, lg: 120},
		seg{n: 4, v0: 90, v1: 100, thr: 60, lg: 60},
		seg{n: 10, v0: 100, v1: 180, thr: 100},
	)
	passed, _ := run(corner.New(), late)
	r.Len(passed, 1)
	r.Equal(wire.PatternLateBraking, passed[0].Pattern)
	r.GreaterOrEqual(passed[0].BrakeAtApex, 20)

	// Turned in early: still slowing well past the lateral peak, throttle waiting.
	early := trace(100,
		seg{n: 10, v0: 200, v1: 200, thr: 100},
		seg{n: 10, v0: 200, v1: 110, brk: 90, lg: 20},
		seg{n: 2, v0: 105, v1: 104, brk: 0, thr: 0, lg: 120},
		seg{n: 25, v0: 100, v1: 85, brk: 0, thr: 5, lg: 80},
		seg{n: 10, v0: 90, v1: 180, thr: 100, lg: 10},
	)
	passed, _ = run(corner.New(), early)
	r.Len(passed, 1)
	r.Equal(wire.PatternEarlyApex, passed[0].Pattern)
	r.Less(passed[0].MinKmh, passed[0].ApexKmh)

	// Off the brakes at the apex but a long wait for the throttle.
	slow := trace(100,
		seg{n: 10, v0: 200, v1: 200, thr: 100},
		seg{n: 10, v0: 200, v1: 100, brk: 90, lg: 20},
		seg{n: 35, v0: 100, v1: 102, brk: 0, thr: 10, lg: 100},
		seg{n: 10, v0: 102, v1: 180, thr: 100, lg: 10},
	)
	passed, _ = run(corner.New(), slow)
	r.Len(passed, 1)
	r.Equal(wire.PatternSlowExit, passed[0].Pattern)
	r.GreaterOrEqual(passed[0].ThrottleLag, 30)
}

func TestExpectingTheLastCorner(t *testing.T) {
	t.Parallel()
	r := require.New(t)

	one := trace(100,
		seg{n: 10, v0: 200, v1: 200, thr: 100},
		seg{n: 10, v0: 200, v1: 100, brk: 90},
		seg{n: 10, v0: 100, v1: 200, thr: 100},
	)
	d := corner.New()
	d.Expect(1)
	passed, lastAt := run(d, one)
	r.Len(passed, 1)
	r.Positive(lastAt, "with an expectation, the first lap reports at its last corner")
	corners, reported := d.EndLap()
	r.Len(corners, 1)
	r.True(reported)

	// A lap that finds fewer corners than expected never reports early; the
	// caller reports at the line. And the expectation does not shrink to zero.
	d.Expect(2)
	_, lastAt = run(d, one)
	r.Equal(-1, lastAt)
	corners, reported = d.EndLap()
	r.Len(corners, 1)
	r.False(reported)
	_, lastAt = run(d, one)
	r.Positive(lastAt, "the next lap expects what the previous found")
}

func TestACornerCueIsDueOnce(t *testing.T) {
	t.Parallel()
	r := require.New(t)

	prev := []clientplugin.Corner{{Turn: 1, ApexPct: 40}, {Turn: 2, ApexPct: 500}, {Turn: 3, ApexPct: 990}}
	within := func(lead int) func(clientplugin.Corner, int) bool {
		return func(_ clientplugin.Corner, ahead int) bool { return ahead <= lead }
	}
	raised := map[int]bool{}

	r.Empty(corner.Due(prev, 300, raised, within(60)), "200 thousandths still to run")
	got := corner.Due(prev, 445, raised, within(60))
	r.Len(got, 1)
	r.Equal(2, got[0].Turn)
	r.Empty(corner.Due(prev, 450, raised, within(60)), "raised once, however the car moves after")
	got = corner.Due(prev, 935, raised, within(60))
	r.Equal(3, got[0].Turn, "due at 930 for the apex at 990")
	got = corner.Due(prev, 5, raised, within(60))
	r.Len(got, 1)
	r.Equal(1, got[0].Turn, "the apex at 40 comes due across the line")
	r.Empty(corner.Due(prev, 5, raised, within(60)))
	// However long the lead, only the next corner is ever due: the gap rule
	// keeps every other corner waiting for the one in front of it.
	got = corner.Due(prev, 5, map[int]bool{}, within(1000))
	r.Len(got, 1)
	r.Equal(1, got[0].Turn)

	// The corner before this one has to be behind the car: a line about the
	// next corner is nothing to a driver still in this one.
	pair := []clientplugin.Corner{{Turn: 1, ApexPct: 500}, {Turn: 2, ApexPct: 520}}
	got = corner.Due(pair, 480, map[int]bool{}, within(60))
	r.Len(got, 1)
	r.Equal(1, got[0].Turn, "Turn 2 waits: Turn 1 is still in front of the car")
	got = corner.Due(pair, 505, map[int]bool{}, within(60))
	r.Len(got, 1)
	r.Equal(2, got[0].Turn, "past Turn 1, Turn 2 is due")

	// The point the driver acts at is the braking point where one was
	// measured, and the apex where none was.
	braked := clientplugin.Corner{Turn: 1, ApexPct: 500, BrakeAtPct: 440}
	r.Equal(440, corner.ActPoint(braked))
	r.Equal(500, corner.ActPoint(clientplugin.Corner{Turn: 1, ApexPct: 500}))
	r.Equal(40, corner.Ahead(440, 400))
	r.Equal(940, corner.Ahead(440, 500), "forward round the lap")
	gap, ok := corner.GapBefore(prev, prev[1])
	r.True(ok)
	r.Equal(460, gap)
	gap, ok = corner.GapBefore(prev, prev[0])
	r.True(ok)
	r.Equal(50, gap, "the corner before Turn 1 is Turn 3, across the line")
	_, ok = corner.GapBefore([]clientplugin.Corner{braked}, braked)
	r.False(ok, "one corner has nothing behind it")
}

// The throttle pickup is measured after the apex. A driver already back on the
// power before the apex has no lag, and a corner is never reported with most of
// a lap between its apex and its exit — which is what subtracting one from the
// other and letting it wrap produces.
func TestTheThrottleLagIsMeasuredAfterTheApex(t *testing.T) {
	t.Parallel()
	r := require.New(t)

	// A corner taken with the power already back on through a long entry: the
	// throttle crosses the threshold before the lateral peak arrives.
	d := corner.New()
	var passed *clientplugin.Corner
	add := func(pct, kmh, throttle, brake, latG int) {
		rep := d.Add(wire.TracePoint{
			DistPct: pct, SpeedKmh: kmh, Throttle: throttle, Brake: brake, LatG: latG, Gear: 3,
		})
		if rep.Passed != nil {
			passed = rep.Passed
		}
	}
	add(300, 200, 0, 90, 0)   // braking
	add(310, 150, 0, 80, 50)  // still braking, turning in
	add(320, 120, 95, 0, 90)  // back on the power early, lateral still rising
	add(330, 118, 95, 0, 180) // the apex proper, later than the pickup
	add(340, 130, 100, 0, 120)
	add(350, 150, 100, 0, 60)
	add(360, 175, 100, 0, 20)
	r.NotNil(passed, "a corner that cost 80 km/h is a corner")
	r.Equal(330, passed.ApexPct)
	r.LessOrEqual(passed.ThrottleLag, corner.MaxLagPct, "no lag runs most of a lap")
	r.GreaterOrEqual(passed.ThrottleLag, 0)
	r.Less(passed.ThrottleLag, 40, "the power was on at the apex; the lag is short or nothing")
}
