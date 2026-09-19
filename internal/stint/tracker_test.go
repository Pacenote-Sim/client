package stint_test

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/pacenote-sim/protocol/wire"

	"github.com/pacenote-sim/clientplugin"
	"github.com/pacenote-sim/clientplugin/clientplugintest"
	demo "github.com/pacenote-sim/clientplugin/examples/source-demo"

	"github.com/pacenote-sim/client/internal/stint"
)

var t0 = time.Date(2026, 9, 18, 10, 0, 0, 0, time.UTC)

// demoLaps is the demo circuit for the given number of 60 s laps at 20 Hz.
func demoLaps(t *testing.T, laps int) []clientplugin.Sample {
	t.Helper()
	src := demo.New()
	src.LapSeconds = 60
	clock := t0
	src.Sleep = func(context.Context, time.Duration) error {
		clock = clock.Add(src.Period)
		return nil
	}
	demo.SetNow(src, func() time.Time { return clock })
	samples, err := clientplugintest.Drain(context.Background(), src, laps*1200+1)
	require.NoError(t, err)
	return samples
}

type collected struct {
	kinds   map[clientplugin.EventKind]int
	events  []clientplugin.Event
	began   []string
	laps    []*wire.Lap
	ended   []string
	summary []*wire.Summary
}

func feed(tr *stint.Tracker, samples []clientplugin.Sample) collected {
	c := collected{kinds: map[clientplugin.EventKind]int{}}
	for i := range samples {
		out := tr.Add(samples[i])
		for _, e := range out.Events {
			c.kinds[e.Kind()]++
			c.events = append(c.events, e)
		}
		if out.Began != nil {
			c.began = append(c.began, out.BeganID)
		}
		if out.Lap != nil {
			c.laps = append(c.laps, out.Lap)
		}
		if out.Ended != "" {
			c.ended = append(c.ended, out.Ended)
			c.summary = append(c.summary, out.EndedSummary)
		}
	}
	return c
}

func TestAStintFromTheDemoCircuit(t *testing.T) {
	t.Parallel()
	r := require.New(t)

	tr := stint.New()
	tr.SetSim("demo")
	r.False(tr.Active())
	c := feed(tr, demoLaps(t, 4))

	r.Len(c.began, 1, "one stint")
	r.True(tr.Active())
	r.Equal(c.began[0], tr.Stint().ID)
	r.Equal("demo", tr.Stint().Sim)
	r.Equal("Demo Circuit", tr.Stint().Track)
	r.Equal(wire.SessionPractice, tr.Stint().Session)
	r.Equal(3000, tr.Stint().TrackLengthM)
	r.Equal(1, c.kinds[clientplugin.KindStintStarted])
	r.Equal(4*1200+1, c.kinds[clientplugin.KindSampled])

	// The first minute is the out lap: a lap counts from the first crossing of
	// the line, so four laps of samples complete three laps.
	r.Len(c.laps, 3)
	r.Equal(3, tr.Laps())
	for i, l := range c.laps {
		r.Equal(i+1, l.Number)
		r.InDelta(60000, l.LapMs, 60)
		r.Equal(wire.KindClean, l.Kind)
		r.Nil(l.Corners, "the server's lap carries no corners: the client compares nothing")
		r.NotEmpty(l.Trace)
	}
	r.Equal(3, c.kinds[clientplugin.KindLapCompleted])
	r.Equal(9, c.kinds[clientplugin.KindCornerPassed])
	r.Equal(3, c.kinds[clientplugin.KindLapLastCorner], "one per lap: at the line for the first, at the last corner after")
	r.Equal(7, c.kinds[clientplugin.KindCornerApproaching],
		"from the second lap, a cue before each of the three corners, and Turn 1 again on the lap the stint ended in")

	// The last-corner report of lap 1 comes at the line, of lap 2 before it.
	var lastCorner []*clientplugin.LapLastCorner
	for _, e := range c.events {
		if lc, ok := e.(*clientplugin.LapLastCorner); ok {
			lastCorner = append(lastCorner, lc)
		}
	}
	r.InDelta(60000, lastCorner[0].ElapsedMs, 60)
	r.Less(lastCorner[1].ElapsedMs, 55000, "lap 2's report leaves before the line")
	r.Len(lastCorner[1].Corners, 3)

	// The cue comes eight seconds of travel before the braking point: at the
	// demo's speeds, some 550 m before braking begins and 700 m before the apex.
	for _, e := range c.events {
		if a, ok := e.(*clientplugin.CornerApproaching); ok {
			r.Greater(a.MetresToApex, 450)
			r.Less(a.MetresToApex, 950)
			r.Contains([]int{1, 2, 3}, a.Turn)
		}
	}

	sum := tr.Summary(t0.Add(3*time.Minute), false)
	r.Equal(3, sum.Laps)
	r.InDelta(60000, sum.BestLapMs, 60)
	r.InDelta(60000, sum.AvgLapMs, 60)
	r.GreaterOrEqual(sum.ConsistencyPct, 99)
	r.InDelta(250, sum.TopSpeedKmh, 2)
	r.NotEmpty(sum.BestTrace)
	r.Nil(sum.FinishedAt)
	r.Positive(sum.CarState.FuelUsedL, "the demo burns fuel")

	id, final, events, ok := tr.Finish(t0.Add(3 * time.Minute))
	r.True(ok)
	r.Equal(c.began[0], id)
	r.NotNil(final.FinishedAt)
	r.Len(events, 1)
	r.Equal(clientplugin.KindStintFinished, events[0].Kind())
	r.False(tr.Active())
	_, _, _, ok = tr.Finish(t0)
	r.False(ok, "finishing nothing is nothing")
}

func TestAStintEndsWhenTheSessionChangesOrTheDriverLeaves(t *testing.T) {
	t.Parallel()
	r := require.New(t)

	tr := stint.New()
	tr.SetSim("demo")
	samples := demoLaps(t, 3)
	// After the first counted lap, the driver joins a race on another car.
	for i := 2500; i < len(samples); i++ {
		samples[i].Session = wire.SessionRace
		samples[i].Car = "Other GT3"
	}
	c := feed(tr, samples)
	r.Len(c.began, 2, "a new stint for the race")
	r.Len(c.ended, 1)
	r.Equal(c.began[0], c.ended[0])
	r.NotNil(c.summary[0].FinishedAt)
	r.Equal(1, c.summary[0].Laps, "the first stint finished one lap")
	r.Equal(2, c.kinds[clientplugin.KindStintStarted])
	r.Equal(1, c.kinds[clientplugin.KindStintFinished])
	r.Equal(wire.SessionRace, tr.Stint().Session)
	r.Equal("Other GT3", tr.Stint().Car)

	// Out of the car for longer than a pit stop: the stint is over, and the
	// next time on track is a new one.
	tr = stint.New()
	tr.SetSim("demo")
	samples = demoLaps(t, 1)
	gone := samples[len(samples)-1]
	gone.OnTrack = false
	gone.At = gone.At.Add(stint.GarageTimeout + time.Second)
	back := samples[0]
	back.At = gone.At.Add(time.Second)
	c = feed(tr, append(samples, gone, back))
	r.Len(c.ended, 1)
	r.Len(c.began, 2)

	// Before the driver is on track, nothing happens at all.
	tr = stint.New()
	garage := clientplugin.Sample{At: t0, OnTrack: false}
	out := tr.Add(garage)
	r.Empty(out.Events)
	r.Nil(out.Began)
	r.False(tr.Active())

	// An unknown session type is filed as practice; no track length means a
	// default lead and no metres.
	tr = stint.New()
	odd := demoLaps(t, 3)
	for i := range odd {
		odd[i].Session = "warmup"
		odd[i].TrackLengthM = 0
	}
	c = feed(tr, odd)
	r.Equal(wire.SessionPractice, tr.Stint().Session)
	for _, e := range c.events {
		if a, ok := e.(*clientplugin.CornerApproaching); ok {
			r.Zero(a.MetresToApex)
		}
	}
	r.Equal(3, c.kinds[clientplugin.KindCornerApproaching])
}

func TestTheSummaryCountsCleanLapsOnly(t *testing.T) {
	t.Parallel()
	r := require.New(t)

	tr := stint.New()
	samples := demoLaps(t, 4)
	// The first counted lap goes through the pits.
	for i := 1500; i < 1600; i++ {
		samples[i].OnPitRoad = true
	}
	for i := range samples {
		samples[i].Incidents = i / 1000
		samples[i].Weather = clientplugin.Weather{Skies: 1, WindKmh: 10, Humidity: 50, TrackTempC: 30, AirTempC: 20}
		samples[i].Tyres = clientplugin.Tyres{LF: 80, RF: 81, LR: 79, RR: 82}
	}
	c := feed(tr, samples)
	r.Len(c.laps, 3)
	r.Equal(wire.KindInvalid, c.laps[0].Kind)
	sum := tr.Summary(t0, false)
	r.Equal(3, sum.Laps)
	r.Equal(4, sum.Incidents)
	r.InDelta(60000, sum.BestLapMs, 60)
	r.Equal(wire.Conditions{Skies: 1, Wetness: 0, WindKmh: 10, Humidity: 50, TrackTempC: 30, AirTempC: 20}, sum.Conditions)
	r.Equal(wire.TyreTemps{LF: 80, RF: 81, LR: 79, RR: 82}, sum.CarState.TyreTempC)
	r.Equal(100, sum.ConsistencyPct, "two clean laps of the same time")

	// No clean lap: no best, no average.
	tr = stint.New()
	samples = demoLaps(t, 2)
	for i := range samples {
		samples[i].OnPitRoad = i > 10
	}
	feed(tr, samples)
	sum = tr.Summary(t0, false)
	r.Equal(1, sum.Laps)
	r.Zero(sum.BestLapMs)
	r.Nil(sum.BestTrace)
}
