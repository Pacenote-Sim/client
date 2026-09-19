// Command pacenote is the Pacenote client with no plugins in it: the app, the
// flags and nothing to read a simulator with but the demo circuit and a
// recording. It is what this repository builds and tests.
//
// A client a driver runs has plugins, and a build is how they get in: a main
// package that imports the ones it wants and calls [app.Main]. A team's server
// generates exactly this file from the plugins the operator ticked, and the
// README shows how to write one by hand. The client module requires no plugin
// of its own, so a build takes only what it asked for.
package main

import (
	"os"

	"github.com/pacenote-sim/client/app"
)

func main() { os.Exit(app.Main(os.Args[1:], os.Stdout, os.Stderr)) }
