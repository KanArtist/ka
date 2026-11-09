package cli

import (
	"context"
	"fmt"
	"path/filepath"

	"github.com/KanArtist/ka/internal/api"
	"github.com/KanArtist/ka/internal/uisrc"
)

// cmdUI chooses where the daemon's web UI comes from.
func cmdUI(ctx context.Context, x *IO, args []string) int {
	sub := "status"
	if len(args) > 0 && args[0] != "" && args[0][0] != '-' {
		sub, args = args[0], args[1:]
	}
	switch sub {
	case "status":
		cfg, err := uisrc.Load()
		if err != nil {
			fmt.Fprintln(x.Err, "ka:", err)
			return 1
		}
		st := uisrc.Resolve(cfg)
		if st.Dir != "" {
			fmt.Fprintf(x.Out, "web UI: %s\n", st.Dir)
		} else {
			fmt.Fprintf(x.Out, "web UI: embedded in this binary (%s)\n", st.Reason)
			if st.Missing {
				fmt.Fprintln(x.Out, "the daemon clones ka again when it starts")
			}
		}
		fmt.Fprintf(x.Out, "API: %s\n", api.APIHash())
		return 0
	case "use":
		dir := x.Dir
		if len(args) > 0 {
			dir = args[0]
			if !filepath.IsAbs(dir) {
				dir = filepath.Join(x.Dir, dir)
			}
		}
		root, ok := uisrc.FindCheckout(filepath.Clean(dir))
		if !ok {
			fmt.Fprintf(x.Err, "ka: %s is not inside a checkout of github.com/KanArtist/ka\n", dir)
			return 1
		}
		cfg, _ := uisrc.Load()
		cfg.Checkout = root
		if cfg.URL == "" {
			cfg.URL = uisrc.DefaultURL
		}
		if err := uisrc.Save(cfg); err != nil {
			fmt.Fprintln(x.Err, "ka:", err)
			return 1
		}
		fmt.Fprintf(x.Out, "the daemon serves the web UI from %s\n", filepath.Join(root, "ui"))
		if err := uisrc.Check(filepath.Join(root, "ui")); err != nil {
			fmt.Fprintf(x.Out, "for now it serves the embedded UI: %v\n", err)
		}
		fmt.Fprintln(x.Out, "a running daemon picks this up when it restarts (ka daemon stop)")
		return 0
	case "embedded":
		if err := uisrc.Clear(); err != nil {
			fmt.Fprintln(x.Err, "ka:", err)
			return 1
		}
		fmt.Fprintln(x.Out, "the daemon serves the web UI embedded in this binary")
		return 0
	}
	fmt.Fprintln(x.Err, "usage: ka ui [status] | use [DIR] | embedded")
	return 2
}
