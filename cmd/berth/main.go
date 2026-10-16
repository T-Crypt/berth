// berth leases one GPU to one inference engine at a time.
//
// The command tree, the output contract and the exit codes live in
// internal/cli; this is the entrypoint and the only place the release sets the
// version.
package main

import (
	"os"
	"runtime/debug"
	"strings"

	"github.com/T-Crypt/berth/internal/cli"
)

// version is set by the release build (-X main.version=); otherwise it
// comes from the module version Go records (go install ...@v0.1.0), and
// a plain build from a checkout reads "dev".
var version = ""

func init() {
	if version != "" {
		return
	}
	version = "dev"
	if bi, ok := debug.ReadBuildInfo(); ok && bi.Main.Version != "" && bi.Main.Version != "(devel)" {
		version = strings.TrimPrefix(bi.Main.Version, "v")
	}
}

func main() {
	os.Exit(cli.Run(cli.Env{Args: os.Args[1:], Out: os.Stdout, Err: os.Stderr, Version: version}))
}
