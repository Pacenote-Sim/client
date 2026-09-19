//go:build wails

// The desktop build: the runner behind a window. The window is a view; every
// decision is the runner's, and the page asks the desktop service.
package app

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"time"

	"github.com/wailsapp/wails/v3/pkg/application"

	"github.com/pacenote-sim/client/internal/desktop"
	"github.com/pacenote-sim/client/internal/frontend"
	"github.com/pacenote-sim/client/internal/host"
	"github.com/pacenote-sim/client/internal/runner"
)

// ShutdownWait is how long closing the window waits for the run to finish
// sending. A stint's last summary is a few kilobytes and goes in a moment; the
// wait is for a server having a bad day, and it ends.
const ShutdownWait = 10 * time.Second

// Main runs the client behind its window and returns the process's exit code.
// args are the command line without the program's own name; stdout is not used
// by this build and is taken so that a build calls the same function whichever
// way it was compiled.
func Main(args []string, _ io.Writer, stderr *os.File) int {
	set, code := parseFlags(args, stderr)
	if code != 0 {
		return code
	}
	file, log, err := openLog(set, stderr)
	if err != nil {
		fmt.Fprintln(stderr, err)
		return 1
	}
	defer func() { _ = file.Close() }()

	var app *application.App
	emit := func(name string, data any) {
		if app != nil {
			app.Event.Emit(name, data)
		}
	}
	var svc *desktop.App
	board := host.NewBoard(func(name string) {
		if svc != nil {
			svc.OnBoard(name)
		}
	})
	svc = desktop.New(set.version, board, emit)
	svc.SetOpener(func(address string) error {
		if app == nil {
			return errors.New("the window is not open")
		}
		return app.Browser.OpenURL(address)
	})

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	// Closing the window ends the run, and the run has things to finish: the
	// stint the driver was in is ended and its summary written, whatever is
	// queued gets its last try, and a companion still uploading a lap is
	// waited for. So the window's shutdown waits for the runner to stop
	// rather than taking the process down under it. It is bounded, because a
	// server that cannot be reached must not keep an app open: what is left
	// is on disk and goes out the next time the driver opens it.
	stopped := make(chan struct{})
	shutdown := func() {
		cancel()
		select {
		case <-stopped:
		case <-time.After(ShutdownWait):
			log.Warn("closing with work unfinished; it is on disk and goes out next time")
		}
	}
	app = application.New(application.Options{
		Name:        "Pacenote",
		Description: "The Pacenote telemetry client",
		Services:    []application.Service{application.NewService(svc)},
		Assets:      application.AssetOptions{Handler: application.AssetFileServerFS(frontend.FS)},
		Logger:      log,
		LogLevel:    slog.LevelWarn,
		OnShutdown:  shutdown,
	})
	app.Window.NewWithOptions(application.WebviewWindowOptions{
		Name: "main", Title: "Pacenote", Width: 440, Height: 760, MinWidth: 360, MinHeight: 520, URL: "/",
		BackgroundColour: application.RGBA{Red: 10, Green: 10, Blue: 10, Alpha: 255},
	})

	// The runner, and again after a lost pairing, until the window closes.
	go func() {
		defer close(stopped)
		for ctx.Err() == nil {
			r, err := runner.New(runner.Config{
				Version: set.version, DataDir: set.dataDir, Address: set.address, Sources: set.sources(), Record: set.record,
				StopAfterLaps: set.laps, Log: log, Board: board, OnPairing: svc.OnPairing, OnStatus: svc.OnStatus,
			})
			if err != nil {
				svc.OnError(err)
				return
			}
			svc.SetActer(r)
			err = r.Run(ctx)
			switch {
			case err == nil, errors.Is(err, context.Canceled):
				if set.laps > 0 {
					svc.OnError(nil)
					return
				}
			case errors.Is(err, runner.ErrUnpaired):
				log.Info("pairing again")
			default:
				svc.OnError(err)
				return
			}
		}
	}()

	if err := app.Run(); err != nil {
		fmt.Fprintln(stderr, err)
		return 1
	}
	return 0
}
