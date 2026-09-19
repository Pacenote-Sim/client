package stint_test

import (
	"context"
	"testing"
	"time"

	"github.com/pacenote-sim/clientplugin"
	"github.com/pacenote-sim/clientplugin/clientplugintest"
	demo "github.com/pacenote-sim/clientplugin/examples/source-demo"

	"github.com/pacenote-sim/client/internal/stint"
)

// What a sample costs, and what a lap costs.
//
// This is the client's hot path and the only one with a rate it does not
// choose: a simulator publishes sixty samples a second and every one of them
// goes through the tracker, which puts it through the lap accumulator and the
// corner detector before anything else in the app sees it. A driver in a race
// is not somebody who can be asked to wait, and a client that falls behind the
// game measures the lap it was not watching.
//
// The numbers to keep an eye on are the allocations. A sample that allocates
// is sixty allocations a second for as long as the car is on track, and the
// events a sample produces — most samples produce one — are what they are.

// benchSamples is a lap of the demo circuit at 20 Hz, which is what a sample
// looks like whatever produced it.
func benchSamples(b *testing.B, laps int) []clientplugin.Sample {
	b.Helper()
	src := demo.New()
	src.Enabled = true
	src.LapSeconds = 60
	clock := time.Date(2026, 9, 19, 10, 0, 0, 0, time.UTC)
	src.Sleep = func(context.Context, time.Duration) error {
		clock = clock.Add(src.Period)
		return nil
	}
	demo.SetNow(src, func() time.Time { return clock })
	samples, err := clientplugintest.Drain(context.Background(), src, laps*1200+1)
	if err != nil {
		b.Fatal(err)
	}
	return samples
}

// BenchmarkASample is the steady state: a car on track, mid-lap, one reading.
func BenchmarkASample(b *testing.B) {
	samples := benchSamples(b, 1)
	tr := stint.New()
	tr.SetSim("bench")
	// Open the stint and get past the first lap, so what is measured is the
	// ordinary sample and not the one that begins something.
	for i := range 700 {
		tr.Add(samples[i])
	}
	s := samples[700]
	b.ReportAllocs()
	for b.Loop() {
		s.At = s.At.Add(50 * time.Millisecond)
		_ = tr.Add(s)
	}
}

// BenchmarkALap is every sample of a lap, including the one that closes it:
// the trace, the sector times, the summary and the corner report all land on
// that sample, and it is the most expensive one the client ever handles.
func BenchmarkALap(b *testing.B) {
	samples := benchSamples(b, 2)
	b.ReportAllocs()
	for b.Loop() {
		tr := stint.New()
		tr.SetSim("bench")
		for i := range samples {
			_ = tr.Add(samples[i])
		}
	}
}
