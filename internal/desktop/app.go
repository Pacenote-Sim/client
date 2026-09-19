// Package desktop is the window's side of the app: what the page asks for and
// what it is told. It is a Wails service — every exported method on App is
// callable from the page by name — and it knows nothing about Wails itself,
// so it is tested like any other package. The window is a view of the
// runner; this is the glass between them.
package desktop

import (
	"context"
	"errors"
	"fmt"
	"net/url"
	"sort"
	"strings"
	"sync"

	"github.com/pacenote-sim/protocol/wire"

	"github.com/pacenote-sim/client/internal/host"
	"github.com/pacenote-sim/client/internal/runner"
)

// The events the page listens to.
const (
	EventStatus  = "pacenote:status"
	EventPairing = "pacenote:pairing"
	EventBoard   = "pacenote:board"
	EventError   = "pacenote:error"
)

// View is everything the page shows about the run.
type View struct {
	Version string         `json:"version"`
	Status  runner.Status  `json:"status"`
	Pairing *Pairing       `json:"pairing,omitempty"`
	Error   string         `json:"error,omitempty"`
	Plugins []CompanionRow `json:"plugins"`
}

// Pairing is the code to show while the client is not paired.
type Pairing struct {
	Code string `json:"code"`
	URL  string `json:"url"`
}

// CompanionRow is one companion as the page lists it.
type CompanionRow struct {
	Name   string `json:"name"`
	Status string `json:"status"`
	Page   string `json:"page"`
}

// Acter delivers a page action to the companion that owns the page: the
// runner. It is set when a run starts, since a run is what has companions.
type Acter interface {
	Act(ctx context.Context, plugin, action string, values map[string]string) error
}

// App is the service. Its exported methods are the page's API.
type App struct {
	version string
	board   *host.Board
	emit    func(name string, data any)

	// opener opens an address in the machine's own browser. A link inside the
	// window's web view goes nowhere on its own — the view has no tabs and no
	// address bar — so a page's links are handed here instead.
	opener func(url string) error

	mu      sync.Mutex
	status  runner.Status
	pairing *Pairing
	err     string
	acter   Acter
}

// New is the service. emit sends an event to the page; nil emits nothing,
// which is how it is tested.
func New(version string, board *host.Board, emit func(name string, data any)) *App {
	if emit == nil {
		emit = func(string, any) {}
	}
	return &App{version: version, board: board, emit: emit}
}

// SetOpener says how to open an address outside the window. Without one,
// Open refuses and says so, which is what a test and a headless build see.
func (a *App) SetOpener(open func(url string) error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.opener = open
}

// Open opens an address from a page in the machine's browser: a chart, the
// pairing page, whatever a companion linked to. Only the server this client is
// paired to, and only over http or https: a page is HTML a plugin wrote, and a
// plugin cannot use the window to open anything it likes.
func (a *App) Open(address string) error {
	a.mu.Lock()
	open, server := a.opener, a.status.Server
	a.mu.Unlock()
	if err := allowedAddress(address, server); err != nil {
		return err
	}
	if open == nil {
		return errors.New("this build cannot open a browser")
	}
	if err := open(address); err != nil {
		return fmt.Errorf("the address could not be opened: %w", err)
	}
	return nil
}

// allowedAddress is the rule: http or https, and the server this client is
// paired to. A client that is not paired to anything opens nothing.
func allowedAddress(address, server string) error {
	u, err := url.Parse(address)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		return errors.New("that is not a web address")
	}
	s, err := url.Parse(server)
	if err != nil || s.Host == "" {
		return errors.New("this client is not paired to a server")
	}
	if !strings.EqualFold(u.Host, s.Host) {
		return fmt.Errorf("%s is not this client's server", u.Host)
	}
	return nil
}

// SetActer sets where page actions go: the current run.
func (a *App) SetActer(acter Acter) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.acter = acter
}

// OnStatus is the runner's hook: keep the status, tell the page.
func (a *App) OnStatus(s runner.Status) {
	a.mu.Lock()
	a.status = s
	if s.Paired {
		a.pairing = nil
	}
	a.mu.Unlock()
	a.emit(EventStatus, a.Status())
}

// OnPairing is the runner's hook: the code to show.
func (a *App) OnPairing(p wire.PairStart) {
	a.mu.Lock()
	a.pairing = &Pairing{Code: p.UserCode, URL: p.VerificationURI}
	a.mu.Unlock()
	a.emit(EventPairing, a.Status())
}

// OnBoard is the board's hook: a companion wrote something.
func (a *App) OnBoard(name string) {
	a.emit(EventBoard, a.companion(name))
}

// OnError is the runner's hook for an error that ended the run.
func (a *App) OnError(err error) {
	a.mu.Lock()
	a.err = ""
	if err != nil {
		a.err = err.Error()
	}
	a.mu.Unlock()
	a.emit(EventError, a.Status())
}

// Status is the page's first and every question: the whole view.
func (a *App) Status() View {
	a.mu.Lock()
	defer a.mu.Unlock()
	return View{Version: a.version, Status: a.status, Pairing: a.pairing, Error: a.err, Plugins: a.companionsLocked()}
}

// Companions lists every companion with a page or a status, sorted by name.
func (a *App) Companions() []CompanionRow {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.companionsLocked()
}

func (a *App) companionsLocked() []CompanionRow {
	seen := map[string]bool{}
	var out []CompanionRow
	if a.board != nil {
		for _, n := range a.board.Names() {
			seen[n] = true
			out = append(out, CompanionRow{Name: n, Status: a.board.Status(n), Page: a.board.Page(n)})
		}
	}
	for _, n := range a.status.Companions {
		if !seen[n] {
			out = append(out, CompanionRow{Name: n})
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}

func (a *App) companion(name string) CompanionRow {
	if a.board == nil {
		return CompanionRow{Name: name}
	}
	return CompanionRow{Name: name, Status: a.board.Status(name), Page: a.board.Page(name)}
}

// Act is a control on a companion's page being used: a form submitted or an
// input changed, with its data-action name and the values. The companion
// answers by redrawing its page.
func (a *App) Act(plugin, action string, values map[string]string) error {
	a.mu.Lock()
	acter := a.acter
	a.mu.Unlock()
	if acter == nil {
		return errors.New("nothing is running to take that")
	}
	if err := acter.Act(context.Background(), plugin, action, values); err != nil {
		return err
	}
	return nil
}

// Version is this client's version.
func (a *App) Version() string { return a.version }
