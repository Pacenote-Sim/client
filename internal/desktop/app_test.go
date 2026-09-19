package desktop_test

import (
	"context"
	"errors"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/pacenote-sim/protocol/wire"

	"github.com/pacenote-sim/client/internal/desktop"
	"github.com/pacenote-sim/client/internal/host"
	"github.com/pacenote-sim/client/internal/runner"
)

type emitted struct {
	name string
	data any
}

func TestThePageSeesTheRunAsItChanges(t *testing.T) {
	t.Parallel()
	r := require.New(t)
	var events []emitted
	app := desktop.New("1.2.3", nil, func(name string, data any) { events = append(events, emitted{name, data}) })

	v := app.Status()
	r.Equal("1.2.3", v.Version)
	r.Equal("1.2.3", app.Version())
	r.False(v.Status.Paired)
	r.Nil(v.Pairing)

	app.OnPairing(wire.PairStart{UserCode: "H4T-9KQ", VerificationURI: "https://team.example/pair"})
	v = app.Status()
	r.Equal(&desktop.Pairing{Code: "H4T-9KQ", URL: "https://team.example/pair"}, v.Pairing)
	r.Equal(desktop.EventPairing, events[len(events)-1].name)

	app.OnStatus(runner.Status{Paired: true, Driver: "Mihai", Source: "iracing", Lap: 3, Companions: []string{"engineer", "voice"}})
	v = app.Status()
	r.Nil(v.Pairing, "paired: the code is gone")
	r.Equal("Mihai", v.Status.Driver)
	r.Equal(desktop.EventStatus, events[len(events)-1].name)
	r.Equal([]desktop.CompanionRow{{Name: "engineer"}, {Name: "voice"}}, v.Plugins, "running companions with nothing written yet")

	// The board is the companions' side; the service is told and tells the page.
	// The board needs the service's hook and the service needs the board, so
	// the board is made first with a hook that finds the service later.
	var withBoard *desktop.App
	b := host.NewBoard(func(name string) {
		if withBoard != nil {
			withBoard.OnBoard(name)
		}
	})
	withBoard = desktop.New("1.2.3", b, func(name string, data any) { events = append(events, emitted{name, data}) })
	h := host.New("engineer", nil, nil, nil, b, nil, nil)
	h.Status("3 lines ready")
	h.Page("<p>Turn 4</p>")
	r.Equal(desktop.EventBoard, events[len(events)-1].name)
	r.Equal(desktop.CompanionRow{Name: "engineer", Status: "3 lines ready", Page: "<p>Turn 4</p>"}, events[len(events)-1].data)
	rows := withBoard.Companions()
	r.Equal([]desktop.CompanionRow{{Name: "engineer", Status: "3 lines ready", Page: "<p>Turn 4</p>"}}, rows)
	r.Len(withBoard.Status().Plugins, 1)

	app.OnError(errors.New("the server is gone"))
	r.Equal("the server is gone", app.Status().Error)
	r.Equal(desktop.EventError, events[len(events)-1].name)
	app.OnError(nil)
	r.Empty(app.Status().Error)
}

type acts struct{ got []string }

func (a *acts) Act(_ context.Context, plugin, action string, values map[string]string) error {
	a.got = append(a.got, plugin+"/"+action+"/"+values["level"])
	if action == "explode" {
		return errors.New("no")
	}
	return nil
}

func TestPageActionsReachTheRun(t *testing.T) {
	t.Parallel()
	r := require.New(t)
	app := desktop.New("1.0.0", nil, nil)
	r.ErrorContains(app.Act("voice", "volume", nil), "nothing is running")
	a := &acts{}
	app.SetActer(a)
	r.NoError(app.Act("voice", "volume", map[string]string{"level": "70"}))
	r.Equal([]string{"voice/volume/70"}, a.got)
	r.EqualError(app.Act("voice", "explode", nil), "no")
}

// A link on a companion's page goes nowhere inside the window, so the page
// hands it here and it opens in the machine's browser. Only this client's own
// server, and only the web: a page is HTML a plugin wrote.
func TestOpeningAnAddressFromAPage(t *testing.T) {
	t.Parallel()
	r := require.New(t)

	app := desktop.New("1.0.0", host.NewBoard(nil), nil)
	var opened []string
	app.SetOpener(func(address string) error {
		opened = append(opened, address)
		return nil
	})

	// Nothing opens before the client knows its server.
	r.Error(app.Open("https://pacenote.example/plugin/visual-telemetry/charts/a/1.png"))

	app.OnStatus(runner.Status{Paired: true, Server: "https://pacenote.example"})
	r.NoError(app.Open("https://pacenote.example/plugin/visual-telemetry/charts/a/1.png"))
	r.NoError(app.Open("https://PACENOTE.EXAMPLE/plugin/engineer/"))
	r.Len(opened, 2)

	// Somewhere else, or something that is not the web, is refused.
	for _, address := range []string{
		"https://somewhere.else/x", "file:///etc/passwd", "javascript:alert(1)",
		"/plugin/visual-telemetry/charts", "", "http://",
	} {
		r.Error(app.Open(address), address)
	}
	r.Len(opened, 2, "a refused address was opened anyway")

	// A build with no browser says so rather than pretending.
	plain := desktop.New("1.0.0", host.NewBoard(nil), nil)
	plain.OnStatus(runner.Status{Paired: true, Server: "https://pacenote.example"})
	err := plain.Open("https://pacenote.example/x")
	r.ErrorContains(err, "cannot open a browser")

	// A browser that fails says why.
	app.SetOpener(func(string) error { return errors.New("no browser here") })
	err = app.Open("https://pacenote.example/x")
	r.ErrorContains(err, "no browser here")
}
