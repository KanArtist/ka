// Command ka is the KanArtist CLI, MCP server (ka mcp) and git merge
// driver (ka merge-driver). See docs/prd.md.
package main

import (
	"context"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"

	"github.com/JumpKickOSS/ka/internal/cli"
	"github.com/JumpKickOSS/ka/internal/engine"
)

var version = "0.1.0-m1"

func main() {
	cli.Version = version
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
