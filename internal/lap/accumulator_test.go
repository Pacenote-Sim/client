package lap_test

import (
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/pacenote-sim/protocol/wire"

	"github.com/pacenote-sim/clientplugin"

	"github.com/pacenote-sim/client/internal/lap"
)

var t0 = time.Date(2026, 9, 18, 10, 0, 0, 0, time.UTC)

// drive produces samples at rate Hz for the given number of laps of lapSeconds
// each, starting at fraction start of the lap. shape may change a sample.
func drive(start float64, laps, lapSeconds, rate int, shape func(i int, s *clientplugin.Sample)) []clientplugin.Sample {
	n := laps * lapSeconds * rate
	out := make([]clientplugin.Sample, 0, n)
	for i := range n {
		elapsed := float64(i) / float64(rate)
		pos := start + elapsed/float64(lapSeconds)
		s := clientplugin.Sample{
			At:         t0.Add(time.Duration(elapsed * float64(time.Second))),
			OnTrack:    true,
			Lap:        1 + int(pos),
			LapDistPct: pos - float64(int(pos)),
			SpeedKmh:   200,
			Throttle:   1,
			Sectors:    []float64{0, 0.4, 0.7},
		}
		if shape != nil {
			shape(i, &s)
		}
		out = append(out, s)
	}
	return out
}

func feed(a *lap.Accumulator, samples []clientplugin.Sample) []*lap.Completed {
	var done []*lap.Completed
	for i := range samples {
		if _, _, d := a.Add(samples[i]); d != nil {
			done = append(done, d)
		}
	}
	return done
}

func TestLapsAreTimedFromTheLineToTheLine(t *testing.T) {
	t.Parallel()
	r := require.New(t)

	// Start a third of the way round; the first crossing begins lap 1.
	a := lap.New()
	samples := drive(0.33, 3, 60, 20, nil)
	done := feed(a, samples)
	r.Len(done, 2, "three laps of driving from mid-lap complete two whole laps")
	for i, d := range done {
		r.Equal(i+1, d.Number)
		r.InDelta(60000, d.LapMs, 1, "interpolated at the crossing, not rounded to a sample")
		r.Equal(wire.KindClean, d.Kind)
		r.Len(d.Trace, 1200, "60 s at 20 Hz")
		r.Equal([]int{24000, 18000, 18000}, roundSectors(d.SectorMs, 5))
		r.Equal(0, d.Trace[0].OffsetMs/100, "the first point is within a sample of the line")
		r.Positive(d.Trace[len(d.Trace)-1].DistPct)
	}
	r.Equal(3, a.Lap())
	r.False(a.Started().IsZero())
	r.NotEmpty(a.Current())

	// Before the first crossing, samples belong to lap 0 and offsets count
	// from the first sample.
	b := lap.New()
	l, off, d := b.Add(samples[0])
	r.Equal(0, l)
	r.Equal(0, off)
	r.Nil(d)
	l, off, d = b.Add(samples[1])
	r.Equal(0, l)
	r.Equal(50, off)
	r.Nil(d)
}

func roundSectors(ms []int, tol int) []int {
	out := make([]int, len(ms))
	for i, v := range ms {
		out[i] = (v + tol) / 1000 * 1000
	}
	return out
}

func TestALapKnowsWhatKindItWas(t *testing.T) {
	t.Parallel()
	r := require.New(t)

	// Started on pit road: out. Ended on pit road: in. Off track in the middle: invalid.
	pitAtStart := feed(lap.New(), drive(0.9, 3, 60, 10, func(i int, s *clientplugin.Sample) {
		if i > 55 && i < 70 {
			s.OnPitRoad = true
		}
	}))
	r.Len(pitAtStart, 2)
	r.Equal(wire.KindOut, pitAtStart[0].Kind)
	r.Equal(wire.KindClean, pitAtStart[1].Kind)

	pitAtEnd := feed(lap.New(), drive(0.9, 3, 60, 10, func(i int, s *clientplugin.Sample) {
		if i > 640 && i <= 660 {
			s.OnPitRoad = true
		}
	}))
	r.Len(pitAtEnd, 2)
	r.Equal(wire.KindIn, pitAtEnd[0].Kind, "on pit road as the line is crossed")

	offTrack := feed(lap.New(), drive(0.9, 3, 60, 10, func(i int, s *clientplugin.Sample) {
		if i == 300 {
			s.OnTrack = false
		}
	}))
	r.Len(offTrack, 2)
	r.Equal(wire.KindInvalid, offTrack[0].Kind)

	pitInMiddle := feed(lap.New(), drive(0.9, 3, 60, 10, func(i int, s *clientplugin.Sample) {
		if i == 300 {
			s.OnPitRoad = true
		}
	}))
	r.Equal(wire.KindInvalid, pitInMiddle[0].Kind, "pit road mid-lap without ending there")
}

func TestAResetToThePitsAbandonsTheLap(t *testing.T) {
	t.Parallel()
	r := require.New(t)

	// Four laps of driving; during the third the simulator's counter jumps
	// without the car crossing the line: a reset to the pits.
	a := lap.New()
	samples := drive(0.9, 4, 60, 10, func(i int, s *clientplugin.Sample) {
		if i >= 1500 {
			s.Lap += 5
		}
	})
	done := feed(a, samples)
	r.Len(done, 2, "the lap in progress at the reset was not counted; the one after it is not finished")
	r.Equal(1, done[0].Number)
	r.Equal(2, done[1].Number)
	r.Equal(3, a.Lap(), "counting resumed at the next crossing")
}

func TestSectorsNeedEveryBoundaryCrossed(t *testing.T) {
	t.Parallel()
	r := require.New(t)

	// A source with no sector boundaries: no sector times.
	none := feed(lap.New(), drive(0.9, 2, 60, 10, func(_ int, s *clientplugin.Sample) { s.Sectors = nil }))
	r.Len(none, 1)
	r.Nil(none[0].SectorMs)

	// A boundary the samples skip over is still crossed: two samples straddle it.
	coarse := feed(lap.New(), drive(0.9, 2, 60, 1, nil))
	r.Len(coarse, 1)
	r.Len(coarse[0].SectorMs, 3)
}

func TestALongLapIsThinnedNotGrown(t *testing.T) {
	t.Parallel()
	r := require.New(t)

	// 20 minutes at 60 Hz is 72 000 samples in one lap.
	a := lap.New()
	done := feed(a, drive(0.99, 2, 1200, 60, nil))
	r.Len(done, 1)
	r.LessOrEqual(len(done[0].Trace), lap.MaxTracePoints)
	r.Greater(len(done[0].Trace), lap.MaxTracePoints/2)
	r.InDelta(1200000, done[0].LapMs, 20)
	// Offsets still ascend and cover the lap.
	for i := 1; i < len(done[0].Trace); i++ {
		r.Greater(done[0].Trace[i].OffsetMs, done[0].Trace[i-1].OffsetMs)
	}
}

func TestResampleKeepsTheShapeByDistance(t *testing.T) {
	t.Parallel()
	r := require.New(t)

	pts := make([]wire.TracePoint, 1000)
	for i := range pts {
		pts[i] = wire.TracePoint{OffsetMs: i * 60, DistPct: i, SpeedKmh: 100 + i%50}
	}
	out := lap.Resample(pts, 101)
	r.Len(out, 101)
	r.Equal(pts[0], out[0])
	r.Equal(pts[999], out[100])
	for i := 1; i < len(out); i++ {
		r.InDelta(10, out[i].DistPct-out[i-1].DistPct, 1, "a point every ten thousandths")
	}

	r.Nil(lap.Resample(pts, 0))
	r.Equal(pts[:1], lap.Resample(pts, 1))
	r.Equal(pts[:5], lap.Resample(pts[:5], 10), "a short trace comes back whole")
	r.Len(lap.Resample(pts, 300), 300)

	// Points bunched at one distance (a stopped car) do not repeat.
	bunched := make([]wire.TracePoint, 0, 200)
	for i := range 100 {
		bunched = append(bunched, wire.TracePoint{OffsetMs: i, DistPct: 500})
	}
	for i := range 100 {
		bunched = append(bunched, wire.TracePoint{OffsetMs: 100 + i, DistPct: 500 + i*5})
	}
	thin := lap.Resample(bunched, 50)
	for i := 1; i < len(thin); i++ {
		r.Greater(thin[i].OffsetMs, thin[i-1].OffsetMs, "no point twice")
	}
}
