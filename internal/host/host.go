// Package host is what the app lends a companion: the door to its own server
// plugin, the speaker, a page and a status line in the window, a log with its
// name on it, and who the driver is. One Host per companion, and each knows
// only its own plugin's name.
package host

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"sort"
	"sync"

	"github.com/pacenote-sim/clientplugin"

	"github.com/pacenote-sim/client/internal/api"
)

// Muted is a Player that knows whether anything it is given would be heard:
// the voice companion with its sound turned off is running and audible to
// nobody.
type Muted interface {
	Audible() bool
}

// Player plays audio through the driver's speakers. The app has one; the
// voice companion, when built in, is the one that knows how to speak words
// that came without audio.
type Player interface {
	Play(ctx context.Context, audio []byte, contentType string) error
}

// Speaker speaks words. The app asks the voice companion first and the
// operating system's own voice otherwise; a Speaker that can do neither logs.
type Speaker interface {
	Say(ctx context.Context, text string) error
}

// Settings keeps each companion's own values. The runner implements it over
// the configuration file.
type Settings interface {
	Setting(plugin, key string) string
	SetSetting(plugin, key, value string) error
}

// Board is what the window shows about every companion: its page and its
// status line. Companions write, the window reads, and Changed is told.
type Board struct {
	mu      sync.Mutex
	pages   map[string]string
	status  map[string]string
	changed func(name string)
}

// NewBoard is an empty board. changed, if given, is called with the companion's
// name whenever its page or status changes.
func NewBoard(changed func(name string)) *Board {
	return &Board{pages: map[string]string{}, status: map[string]string{}, changed: changed}
}

// Page is a companion's current page.
func (b *Board) Page(name string) string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.pages[name]
}

// Status is a companion's current status line.
func (b *Board) Status(name string) string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.status[name]
}

// Names is every companion that has written something, sorted.
func (b *Board) Names() []string {
	b.mu.Lock()
	defer b.mu.Unlock()
	seen := map[string]bool{}
	for n := range b.pages {
		seen[n] = true
	}
	for n := range b.status {
		seen[n] = true
	}
	out := make([]string, 0, len(seen))
	for n := range seen {
		out = append(out, n)
	}
	sort.Strings(out)
	return out
}

func (b *Board) setPage(name, html string) {
	b.mu.Lock()
	b.pages[name] = html
	b.mu.Unlock()
	if b.changed != nil {
		b.changed(name)
	}
}

func (b *Board) setStatus(name, text string) {
	b.mu.Lock()
	b.status[name] = text
	b.mu.Unlock()
	if b.changed != nil {
		b.changed(name)
	}
}

// Host is one companion's host.
type Host struct {
	name     string
	client   *api.Client
	player   Player
	speaker  Speaker
	board    *Board
	settings Settings
	log      *slog.Logger

	mu     sync.RWMutex
	slug   string
	driver string
}

// New is the host for the companion of the named server plugin. A nil
// settings store keeps nothing: every setting reads as empty.
func New(name string, client *api.Client, player Player, speaker Speaker, board *Board, settings Settings, log *slog.Logger) *Host {
	if log == nil {
		log = slog.New(slog.DiscardHandler)
	}
	log = log.With(slog.String("plugin", name))
	if player == nil {
		player = Silent{Log: log}
	}
	if speaker == nil {
		speaker = Silent{Log: log}
	}
	if settings == nil {
		settings = forgetful{}
	}
	return &Host{name: name, client: client, player: player, speaker: speaker, board: board, settings: settings, log: log}
}

// SetDriver tells the host who is signed in, from GET /me.
func (h *Host) SetDriver(slug, name string) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.slug, h.driver = slug, name
}

// Do implements [clientplugin.Host]: the request goes to this companion's own
// plugin, with the token, and nowhere else.
func (h *Host) Do(ctx context.Context, method, path string, body io.Reader, header http.Header) (*http.Response, error) {
	res, err := h.client.Plugin(ctx, h.name, method, path, body, header)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", h.name, err)
	}
	return res, nil
}

// Play implements [clientplugin.Host].
func (h *Host) Play(ctx context.Context, audio []byte, contentType string) error {
	if err := h.player.Play(ctx, audio, contentType); err != nil {
		return fmt.Errorf("%s: playing: %w", h.name, err)
	}
	return nil
}

// Say implements [clientplugin.Host].
func (h *Host) Say(ctx context.Context, text string) error {
	if err := h.speaker.Say(ctx, text); err != nil {
		return fmt.Errorf("%s: speaking: %w", h.name, err)
	}
	return nil
}

// Page implements [clientplugin.Host].
func (h *Host) Page(html string) { h.board.setPage(h.name, html) }

// Status implements [clientplugin.Host].
func (h *Host) Status(text string) { h.board.setStatus(h.name, text) }

// Log implements [clientplugin.Host].
func (h *Host) Log() *slog.Logger { return h.log }

// Audible implements [clientplugin.Host]: whether a companion handing over
// audio would be heard at all.
func (h *Host) Audible() bool {
	m, ok := h.player.(Muted)
	if !ok {
		return h.player != nil
	}
	return m.Audible()
}

// Server implements [clientplugin.Host].
func (h *Host) Server() string {
	if h.client == nil {
		return ""
	}
	return h.client.Address()
}

// Driver implements [clientplugin.Host].
func (h *Host) Driver() (slug, name string) {
	h.mu.RLock()
	defer h.mu.RUnlock()
	return h.slug, h.driver
}

// Setting implements [clientplugin.Host].
func (h *Host) Setting(key string) string { return h.settings.Setting(h.name, key) }

// SetSetting implements [clientplugin.Host].
func (h *Host) SetSetting(key, value string) error {
	if err := h.settings.SetSetting(h.name, key, value); err != nil {
		return fmt.Errorf("%s: keeping %s: %w", h.name, key, err)
	}
	return nil
}

var _ clientplugin.Host = (*Host)(nil)

// forgetful is the settings store of a host with none: nothing is kept.
type forgetful struct{}

func (forgetful) Setting(string, string) string           { return "" }
func (forgetful) SetSetting(string, string, string) error { return nil }

// Silent is a Player and a Speaker for a build with no audio: what would have
// been heard goes to the log, so that a headless run shows the lines.
type Silent struct {
	Log *slog.Logger
}

// Audible implements Muted: a build with no audio hears nothing, so a
// companion whose server half would buy some is told not to.
func (Silent) Audible() bool { return false }

// Play implements Player.
func (s Silent) Play(ctx context.Context, audio []byte, contentType string) error {
	s.Log.LogAttrs(ctx, slog.LevelInfo, "audio would play", slog.Int("bytes", len(audio)), slog.String("type", contentType))
	return nil
}

// Say implements Speaker.
func (s Silent) Say(ctx context.Context, text string) error {
	s.Log.LogAttrs(ctx, slog.LevelInfo, "would say", slog.String("text", text))
	return nil
}
