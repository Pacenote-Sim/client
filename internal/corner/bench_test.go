package corner_test

import (
	"math"
	"testing"

	"github.com/pacenote-sim/protocol/wire"

	"github.com/pacenote-sim/client/internal/corner"
)

// What watching for corners costs. It runs on every sample, beside the lap
// accumulator, for as long as the car is on track: a corner is found as it is
// driven and not by reading the lap afterwards, because the line for the next
// lap has to be asked for before the lap ends.

// benchLap is a lap with eight corners, braking and all, at 60 Hz.
func benchLap(points int) []wire.TracePoint {
	out := make([]wire.TracePoint, 0, points)
	for i := range points {
		pct := float64(i) / float64(points)
		// Eight corners: the speed dips and the pedals swap over at each.
		phase := math.Sin(pct * 8 * 2 * math.Pi)
		speed := 150 + 80*phase
		p := wire.TracePoint{
			OffsetMs: i * 16, SpeedKmh: int(speed), DistPct: int(pct * 1000),
			Gear: 3 + int(speed)/60, RPM: 4000 + int(speed)*20, LatG: int(100 * math.Cos(pct*8*2*math.Pi)),
		}
		if phase < -0.3 {
			p.Brake = int(-phase * 100)
		} else {
			p.Throttle = int(math.Min(100, 40+phase*100))
		}
		out = append(out, p)
	}
	return out
}

// BenchmarkAPoint is the steady state: one reading through the detector.
func BenchmarkAPoint(b *testing.B) {
	lap := benchLap(6000)
	d := corner.New()
	for i := range 100 {
		d.Add(lap[i])
	}
	p := lap[100]
	b.ReportAllocs()
	for b.Loop() {
		_ = d.Add(p)
	}
}

// BenchmarkALapOfCorners is a whole lap through the detector, which is what a
// driver's lap actually costs: the corners it finds and the reports it makes.
func BenchmarkALapOfCorners(b *testing.B) {
	lap := benchLap(6000)
	b.ReportAllocs()
	for b.Loop() {
		d := corner.New()
		for i := range lap {
			_ = d.Add(lap[i])
		}
		d.EndLap()
	}
}
