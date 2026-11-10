// Command ka is the KanArtist CLI, MCP server (ka mcp) and git merge
// driver (ka merge-driver). See docs/prd.md.
package main

import (
	"context"
	"os"
	"os/signal"
	"path/filepath"
	"runtime/debug"
	"strings"
	"syscall"

	"github.com/KanArtist/ka/internal/cli"
	"github.com/KanArtist/ka/internal/engine"
	"github.com/KanArtist/ka/pkg/kaspec"
)

// version is set by release builds (-ldflags "-X main.version=1.2.3").
var version = "0.1.0-dev"

func main() {
	cli.Version = version
	// go install github.com/KanArtist/ka/cmd/ka@v1.2.3 records the module version.
	if bi, ok := debug.ReadBuildInfo(); ok && strings.HasSuffix(version, "-dev") && strings.HasPrefix(bi.Main.Version, "v") && !strings.HasPrefix(bi.Main.Version, "v0.0.0-") {
		cli.Version = strings.TrimPrefix(bi.Main.Version, "v")
	}
	kaspec.KaVersion = cli.Version
	if exe, err := os.Executable(); err == nil {
		if real, err := filepath.EvalSymlinks(exe); err == nil {
			exe = real
		}
		// Forward slashes keep the command valid in git's sh on Windows too.
		engine.MergeDriverCommand = `"` + strings.ReplaceAll(exe, `\`, "/") + `" merge-driver %O %A %B %P`
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	os.Exit(cli.Run(ctx, &cli.IO{In: os.Stdin, Out: os.Stdout, Err: os.Stderr}, os.Args[1:]))
}
