// Package events delivers the app's events to the companions that asked for
// them: one goroutine per companion, in order, never blocking the capture
// loop. A slow companion delays its own next event and nobody else's.
package events

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sort"
	"sync"

	"github.com/pacenote-sim/clientplugin"
)

// QueueDepth is how many events wait for one companion before the bus starts
// dropping. Samples are dropped first: the newest sample is worth more than a
// queue of old ones. Anything else dropped is logged, because it means a
// companion has stopped keeping up.
const QueueDepth = 256

// Bus is the fan-out.
type Bus struct {
	log *slog.Logger
	mu  sync.Mutex
	// subs by companion name.
	subs map[string]*subscriber
}

type subscriber struct {
	c       clientplugin.Companion
	wants   map[clientplugin.EventKind]bool
	ch      chan clientplugin.Event
	cancel  context.CancelFunc
	done    chan struct{}
	log     *slog.Logger
	stopped bool
}

// New is an empty bus.
func New(log *slog.Logger) *Bus {
	if log == nil {
		log = slog.New(slog.DiscardHandler)
	}
	return &Bus{log: log, subs: map[string]*subscriber{}}
}

// Start starts a companion with its host and begins delivering the events it
// wants. Starting a companion that is running is a no-op. A Start that fails
// leaves the companion stopped and returns why.
func (b *Bus) Start(ctx context.Context, c clientplugin.Companion, h clientplugin.Host) error {
	b.mu.Lock()
	defer b.mu.Unlock()
	if _, running := b.subs[c.Name()]; running {
		return nil
	}
	cctx, cancel := context.WithCancel(ctx)
	if err := c.Start(cctx, h); err != nil {
		cancel()
		return fmt.Errorf("%s could not start: %w", c.Name(), err)
	}
	s := &subscriber{
		c: c, wants: map[clientplugin.EventKind]bool{}, ch: make(chan clientplugin.Event, QueueDepth),
		cancel: cancel, done: make(chan struct{}), log: b.log.With(slog.String("plugin", c.Name())),
	}
	for _, k := range c.Wants() {
		s.wants[k] = true
	}
	b.subs[c.Name()] = s
	go s.run(cctx)
	return nil
}

func (s *subscriber) run(ctx context.Context) {
	defer close(s.done)
	for {
		select {
		case <-ctx.Done():
			return
		case e, ok := <-s.ch:
			if !ok {
				return
			}
			if err := s.c.Notify(ctx, e); err != nil && !errors.Is(err, context.Canceled) {
				s.log.LogAttrs(ctx, slog.LevelWarn, "a companion could not handle an event",
					slog.String("event", string(e.Kind())), slog.String("reason", err.Error()))
			}
		}
	}
}

// Stop stops one companion: no more events are accepted, the ones already
// queued are delivered, then its context ends and its Stop is called. A lap
// completed a moment before the app closes still reaches its companion.
func (b *Bus) Stop(name string) error {
	b.mu.Lock()
	s, ok := b.subs[name]
	delete(b.subs, name)
	if ok {
		s.stopped = true
		close(s.ch)
	}
	b.mu.Unlock()
	if !ok {
		return nil
	}
	<-s.done
	s.cancel()
	if err := s.c.Stop(); err != nil {
		return fmt.Errorf("%s did not stop cleanly: %w", name, err)
	}
	return nil
}

// StopAll stops every companion, in name order, and returns the first error.
func (b *Bus) StopAll() error {
	var first error
	for _, name := range b.Running() {
		if err := b.Stop(name); err != nil && first == nil {
			first = err
		}
	}
	return first
}

// Running is the names of the companions delivering, sorted.
func (b *Bus) Running() []string {
	b.mu.Lock()
	defer b.mu.Unlock()
	out := make([]string, 0, len(b.subs))
	for name := range b.subs {
		out = append(out, name)
	}
	sort.Strings(out)
	return out
}

// Publish hands an event to every companion that wants its kind. It never
// blocks: a companion whose queue is full loses this event, silently for a
// sample and with a log line for anything else.
func (b *Bus) Publish(e clientplugin.Event) {
	b.mu.Lock()
	defer b.mu.Unlock()
	for _, s := range b.subs {
		if !s.wants[e.Kind()] || s.stopped {
			continue
		}
		select {
		case s.ch <- e:
		default:
			if e.Kind() != clientplugin.KindSampled {
				s.log.LogAttrs(context.Background(), slog.LevelWarn, "a companion is not keeping up; an event was dropped",
					slog.String("event", string(e.Kind())))
			}
		}
	}
}
