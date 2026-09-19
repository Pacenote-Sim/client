package lap_test

import (
	"testing"

	"github.com/pacenote-sim/protocol/wire"

	"github.com/pacenote-sim/client/internal/lap"
)

// What thinning a lap costs. Every lap the client sends goes through this
// twice — once for the lap and once for a summary's best — and a lap read at
// sixty samples a second is five or six thousand points against a server that
// takes three hundred.

func benchTrace(n int) []wire.TracePoint {
	out := make([]wire.TracePoint, 0, n)
	for i := range n {
		out = append(out, wire.TracePoint{
			OffsetMs: i * 16, SpeedKmh: 100 + i%120, Throttle: 100, Brake: 0, Gear: 4,
			RPM: 7000, DistPct: i * 1000 / n, La: 5043700 + i, Lo: 597140 + i,
		})
	}
	return out
}

// BenchmarkResampleToTheWire is a lap as a simulator gave it, thinned to what
// a server asked for.
func BenchmarkResampleToTheWire(b *testing.B) {
	trace := benchTrace(6000)
	b.ReportAllocs()
	for b.Loop() {
		out := lap.Resample(trace, 300)
		if len(out) != 300 {
			b.Fatalf("resampled to %d", len(out))
		}
	}
}

// BenchmarkResampleAtTheCap is the other end: a lap at the client's own limit,
// asked for more points than it has, which must cost nothing but the check.
func BenchmarkResampleAtTheCap(b *testing.B) {
	trace := benchTrace(300)
	b.ReportAllocs()
	for b.Loop() {
		_ = lap.Resample(trace, 4096)
	}
}
