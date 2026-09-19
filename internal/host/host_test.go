package host_test

import (
	"bytes"
	"context"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/pacenote-sim/client/internal/api"
	"github.com/pacenote-sim/client/internal/host"
)

type failing struct{}

type memory map[string]string

func (m memory) Setting(plugin, key string) string { return m[plugin+"/"+key] }
func (m memory) SetSetting(plugin, key, value string) error {
	m[plugin+"/"+key] = value
	return nil
}

type failingStore struct{}

func (failingStore) Setting(string, string) string { return "" }
func (failingStore) SetSetting(string, string, string) error {
	return errors.New("disk full")
}

func (failing) Play(context.Context, []byte, string) error { return errors.New("no device") }
func (failing) Say(context.Context, string) error          { return errors.New("no voice") }

func TestAHostLendsExactlyWhatTheContractSays(t *testing.T) {
	t.Parallel()
	r := require.New(t)
	ctx := context.Background()

	var seen []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		seen = append(seen, req.URL.Path+" "+req.Header.Get("Authorization"))
		_, _ = w.Write([]byte("hi"))
	}))
	t.Cleanup(srv.Close)
	client, err := api.New(srv.URL, "1.0.0", nil)
	r.NoError(err)
	client.SetToken("tok-1")

	var changed []string
	board := host.NewBoard(func(name string) { changed = append(changed, name) })
	var logged bytes.Buffer
	log := slog.New(slog.NewTextHandler(&logged, nil))

	h := host.New("engineer", client, nil, nil, board, nil, log)
	h.SetDriver("mihai", "Mihai")

	res, err := h.Do(ctx, http.MethodGet, "/cues?stint=1", nil, nil)
	r.NoError(err)
	body, _ := io.ReadAll(res.Body)
	r.NoError(res.Body.Close())
	r.Equal("hi", string(body))
	r.Equal([]string{"/plugin/engineer/cues Bearer tok-1"}, seen, "its own mount, with the token")
	_, err = h.Do(ctx, http.MethodGet, "../voice/speak", nil, nil) //nolint:bodyclose // refused before a request exists.
	r.ErrorIs(err, api.ErrOutsideMount)
	r.ErrorContains(err, "engineer:")

	r.NoError(h.Play(ctx, []byte("mp3"), "audio/mpeg"))
	r.NoError(h.Say(ctx, "Turn four."))
	r.Contains(logged.String(), "audio would play")
	r.Contains(logged.String(), "Turn four.")
	r.Contains(logged.String(), "plugin=engineer")

	h.Page("<p>x</p>")
	h.Status("ready")
	r.Equal("<p>x</p>", board.Page("engineer"))
	r.Equal("ready", board.Status("engineer"))
	r.Equal([]string{"engineer"}, board.Names())
	r.Equal([]string{"engineer", "engineer"}, changed)
	r.Empty(board.Page("voice"))

	slug, name := h.Driver()
	r.Equal("mihai", slug)
	r.Equal("Mihai", name)
	r.NotNil(h.Log())
	r.Empty(h.Setting("volume"), "a host with no store keeps nothing")
	r.NoError(h.SetSetting("volume", "70"))
	r.Empty(h.Setting("volume"))

	// With a store, a setting is the companion's own and survives.
	store := memory{}
	kept := host.New("voice", client, nil, nil, board, store, nil)
	r.NoError(kept.SetSetting("volume", "70"))
	r.Equal("70", kept.Setting("volume"))
	r.Equal("70", store["voice/volume"])
	other := host.New("engineer", client, nil, nil, board, store, nil)
	r.Empty(other.Setting("volume"), "another companion's setting is not this one's")
	r.ErrorContains(host.New("voice", client, nil, nil, board, failingStore{}, nil).SetSetting("k", "v"), "voice: keeping k")

	// A player or speaker that fails says which companion and what.
	broken := host.New("voice", client, failing{}, failing{}, host.NewBoard(nil), nil, nil)
	r.ErrorContains(broken.Play(ctx, nil, "audio/wav"), "voice: playing: no device")
	r.ErrorContains(broken.Say(ctx, "x"), "voice: speaking: no voice")
	broken.Status("s")
}

// A companion asks whether anything it hands over would be heard, so that its
// server half does not buy audio for a driver who is reading.
func TestAudibleFollowsThePlayer(t *testing.T) {
	t.Parallel()
	r := require.New(t)

	// A player that says nothing about muting is audible while it is there.
	plain := host.New("engineer", nil, &recordingPlayer{}, nil, host.NewBoard(nil), nil, nil)
	r.True(plain.Audible())

	// One that knows its own volume answers for itself.
	m := &mutedPlayer{}
	h := host.New("engineer", nil, m, nil, host.NewBoard(nil), nil, nil)
	r.False(h.Audible(), "the driver has it turned off")
	m.on = true
	r.True(h.Audible())

	// A build with no audio at all hears nothing.
	r.False(host.Silent{Log: slog.New(slog.DiscardHandler)}.Audible())
	quiet := host.New("engineer", nil, host.Silent{Log: slog.New(slog.DiscardHandler)}, nil, host.NewBoard(nil), nil, nil)
	r.False(quiet.Audible())
}

type recordingPlayer struct{}

func (*recordingPlayer) Play(context.Context, []byte, string) error { return nil }

type mutedPlayer struct{ on bool }

func (*mutedPlayer) Play(context.Context, []byte, string) error { return nil }
func (m *mutedPlayer) Audible() bool                            { return m.on }
