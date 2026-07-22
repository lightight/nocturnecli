// Command nocturne is a terminal coding agent powered by the Nocturne API.
package main

import (
	_ "embed"
	"fmt"
	"os"

	"github.com/lightight/nocturnecli/internal/app"
)

//go:embed docs/index.html
var docsHTML string

//go:embed install.sh
var installSh string

//go:embed install.ps1
var installPs1 string

func main() {
	// Put the Windows console into UTF-8 mode before anything is printed, so the
	// TUI's glyphs don't come out as mojibake. No-op on other platforms.
	app.EnableConsoleUTF8()

	args := os.Args[1:]

	// `nocturne serve` hosts the docs site, install scripts, and the remote relay.
	if len(args) > 0 && args[0] == "serve" {
		assets := app.ServerAssets{Docs: docsHTML, InstallSh: installSh, InstallPs1: installPs1}
		if err := app.RunServer(args[1:], assets); err != nil {
			fmt.Fprintln(os.Stderr, "nocturne serve: "+err.Error())
			os.Exit(1)
		}
		return
	}

	if err := app.Run(args); err != nil {
		fmt.Fprintln(os.Stderr, "nocturne: "+err.Error())
		os.Exit(1)
	}
}
