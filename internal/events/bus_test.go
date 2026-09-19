package events_test

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/pacenote-sim/clientplugin"
	"github.com/pacenote-sim/clientplugin/clientplugintest"

	"github.com/pacenote-sim/client/internal/events"
)

type recorder struct {
	name     string
	wants    []clientplugin.EventKind
	startErr error
	stopErr  error
	slow     time.Duration
	failOn   clientplugin.EventKind

	mu      sync.Mutex
	got     []clientplugin.EventKind
	started int
	stopped int
}

func (r *recorder) Name() string                    { return r.name }
func (r *recorder) Wants() []clientplugin.EventKind { return r.wants }
func (r *recorder) Start(context.Context, clientplugin.Host) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.started++
	return r.startErr
}

func (r *recorder) Notify(ctx context.Context, e clientplugin.Event) error {
	if r.slow > 0 {
		select {
		case <-time.After(r.slow):
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	r.got = append(r.got, e.Kind())
	if e.Kind() == r.failOn {
		return errors.New("could not")
	}
	return nil
}

func (r *recorder) Stop() error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.stopped++
	return r.stopErr
}

func (r *recorder) kinds() []clientplugin.EventKind {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]clientplugin.EventKind(nil), r.got...)
}

func eventually(t *testing.T, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatal("condition not met in time")
}

func TestEventsReachOnlyThoseWhoAskedInOrder(t *testing.T) {
	t.Parallel()
	r := require.New(t)
	ctx := context.Background()
	h := clientplugintest.NewHost(t, nil)
	bus := events.New(nil)

	laps := &recorder{name: "laps", wants: []clientplugin.EventKind{clientplugin.KindLapCompleted, clientplugin.KindStintFinished}}
	all := &recorder{name: "all", wants: clientplugin.Kinds(), failOn: clientplugin.KindStintStarted}
	r.NoError(bus.Start(ctx, laps, h))
	r.NoError(bus.Start(ctx, all, h))
	r.NoError(bus.Start(ctx, all, h), "starting twice is once")
	r.Equal(1, all.started)
	r.Equal([]string{"all", "laps"}, bus.Running())

	at := time.Now()
	bus.Publish(&clientplugin.StintStarted{At: at})
	for i := range 3 {
		bus.Publish(&clientplugin.LapCompleted{At: at, Lap: i + 1})
	}
	bus.Publish(&clientplugin.Sampled{Sample: clientplugin.Sample{At: at}})
	bus.Publish(&clientplugin.StintFinished{At: at})

	eventually(t, func() bool { return len(laps.kinds()) == 4 && len(all.kinds()) == 6 })
	r.Equal([]clientplugin.EventKind{
		clientplugin.KindLapCompleted, clientplugin.KindLapCompleted, clientplugin.KindLapCompleted, clientplugin.KindStintFinished,
	}, laps.kinds(), "only what was asked for, in order")
	r.Equal(clientplugin.KindStintStarted, all.kinds()[0], "a Notify error changes nothing: the next event still came")

	// What is queued when Stop is called is still delivered before Stop.
	bus.Publish(&clientplugin.LapCompleted{At: at, Lap: 4})
	r.NoError(bus.Stop("laps"))
	r.Equal(1, laps.stopped)
	r.Len(laps.kinds(), 5, "the lap published just before the stop reached the companion")
	r.NoError(bus.Stop("laps"), "stopping what is stopped is nothing")
	r.Equal([]string{"all"}, bus.Running())
	bus.Publish(&clientplugin.LapCompleted{At: at, Lap: 9})
	time.Sleep(20 * time.Millisecond)
	r.Len(laps.kinds(), 5, "a stopped companion hears nothing")
	r.NoError(bus.StopAll())
	r.Empty(bus.Running())
}

func TestAFailingStartAndASlowCompanion(t *testing.T) {
	t.Parallel()
	r := require.New(t)
	ctx := context.Background()
	h := clientplugintest.NewHost(t, nil)
	bus := events.New(nil)

	broken := &recorder{name: "broken", startErr: errors.New("no key")}
	r.ErrorContains(bus.Start(ctx, broken, h), "broken could not start: no key")
	r.Empty(bus.Running())

	// A companion that takes its time delays only itself, and a flood of
	// samples is thinned rather than blocking the publisher.
	slow := &recorder{name: "slow", wants: []clientplugin.EventKind{clientplugin.KindSampled, clientplugin.KindLapCompleted}, slow: 2 * time.Millisecond}
	quick := &recorder{name: "quick", wants: []clientplugin.EventKind{clientplugin.KindSampled}}
	r.NoError(bus.Start(ctx, slow, h))
	r.NoError(bus.Start(ctx, quick, h))
	start := time.Now()
	for range 2000 {
		bus.Publish(&clientplugin.Sampled{Sample: clientplugin.Sample{At: start}})
	}
	bus.Publish(&clientplugin.LapCompleted{At: start, Lap: 1})
	r.Less(time.Since(start), time.Second, "publishing never waits for a companion")
	eventually(t, func() bool { return len(quick.kinds()) >= 256 })
	r.Less(len(slow.kinds()), 2001, "the slow companion lost samples, not the publisher its time")

	stopErr := &recorder{name: "stuck", wants: nil, stopErr: errors.New("still running")}
	r.NoError(bus.Start(ctx, stopErr, h))
	err := bus.StopAll()
	r.ErrorContains(err, "stuck did not stop cleanly")
	r.Empty(bus.Running())
}
