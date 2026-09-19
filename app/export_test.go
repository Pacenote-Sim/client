//go:build !wails

package app

import (
	"io"

	"github.com/pacenote-sim/client/internal/runner"
	"github.com/pacenote-sim/client/internal/stamp"
	"github.com/pacenote-sim/clientplugin"
)

// The pieces of the program, for the tests.

// Settings is what the flags decided.
type Settings = settings

// ParseFlags is parseFlags: the settings, and a non-zero exit code.
func ParseFlags(args []string, stderr io.Writer) (Settings, int) { return parseFlags(args, stderr) }

// Sources is what the flags asked to drive.
func Sources(s Settings) []clientplugin.Source { return s.sources() }

// StatusLine is the one line a run prints when something changes.
func StatusLine(s runner.Status) string { return statusLine(s) }

// Version is the version a build reports.
func Version() string { return buildVersion() }

// The settings a test reads back.

// Address is the server this run talks to.
func (s Settings) Address() string { return s.address }

// DataDir is where the token, the queue and the recordings live.
func (s Settings) DataDir() string { return s.dataDir }

// Laps is where the run stops, or zero.
func (s Settings) Laps() int { return s.laps }

// Record reports that every session is written to disk.
func (s Settings) Record() bool { return s.record }

// AddressFrom is addressFrom: the server a build talks to, from its stamp or
// its flag.
func AddressFrom(read func() (stamp.Stamp, error), flagValue string) (string, error) {
	return addressFrom(read, flagValue)
}

// DemoSource is the demo circuit the --demo flag drives.
func DemoSource(speed float64) clientplugin.Source { return demoSource(speed) }
