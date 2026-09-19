package events_test

import (
	"context"
	"testing"
	"time"

	"github.com/pacenote-sim/clientplugin"
	"github.com/pacenote-sim/clientplugin/clientplugintest"

	"github.com/pacenote-sim/client/internal/events"
)

// What handing a sample to the companions costs. Every reading the app takes
// is published here, so this runs sixty times a second beside the tracker, and
// it runs on the goroutine that is reading the simulator: a companion that is
// slow must cost that goroutine nothing but a channel send.

// counter is a companion that wants everything and does nothing with it, which
// is what the bus itself costs.
type counter struct {
	name string
	got  int
}

func (c *counter) Name() string { return c.name }
func (c *counter) Wants() []clientplugin.EventKind {
	return []clientplugin.EventKind{clientplugin.KindSampled}
}
func (c *counter) Start(context.Context, clientplugin.Host) error { return nil }
func (c *counter) Stop() error                                    { return nil }
func (c *counter) Notify(context.Context, clientplugin.Event) error {
	c.got++
	return nil
}

func benchBus(b *testing.B, companions int) *events.Bus {
	b.Helper()
	bus := events.New(nil)
	h := clientplugintest.NewHost(b, nil)
	for i := range companions {
		if err := bus.Start(context.Background(), &counter{name: string(rune('a' + i))}, h); err != nil {
			b.Fatal(err)
		}
	}
	b.Cleanup(func() { _ = bus.StopAll() })
	return bus
}

// BenchmarkPublishToOne is the ordinary case: the sample goes to the one
// companion that asked for samples.
func BenchmarkPublishToOne(b *testing.B) {
	bus := benchBus(b, 1)
	e := &clientplugin.Sampled{Lap: 3, OffsetMs: 1000, Sample: clientplugin.Sample{At: time.Now(), SpeedKmh: 180}}
	b.ReportAllocs()
	for b.Loop() {
		bus.Publish(e)
	}
}

// BenchmarkPublishToFour is a client built with every official plugin.
func BenchmarkPublishToFour(b *testing.B) {
	bus := benchBus(b, 4)
	e := &clientplugin.Sampled{Lap: 3, OffsetMs: 1000, Sample: clientplugin.Sample{At: time.Now(), SpeedKmh: 180}}
	b.ReportAllocs()
	for b.Loop() {
		bus.Publish(e)
	}
}

// BenchmarkPublishToNobody is a client whose companions want other things: the
// cost of a sample nobody asked for, which is most samples on most clients.
func BenchmarkPublishToNobody(b *testing.B) {
	bus := benchBus(b, 0)
	e := &clientplugin.Sampled{Lap: 3, OffsetMs: 1000, Sample: clientplugin.Sample{At: time.Now(), SpeedKmh: 180}}
	b.ReportAllocs()
	for b.Loop() {
		bus.Publish(e)
	}
}
