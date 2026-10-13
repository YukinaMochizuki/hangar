// Command hangar-cloudflare converges Cloudflare's side of the public API: the
// tunnel, its DNS record, and the Access application, policies and smoke
// token. See docs/runbook/cloudflare.md.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/YukinaMochizuki/hangar/internal/cloudflare"
	"github.com/YukinaMochizuki/hangar/internal/cloudflare/converge"
)

// Exit codes.
const (
	exitOK    = 0
	exitError = 1
	exitUsage = 2
	// exitDrift is verify finding Cloudflare or the outputs differ from the
	// config.
	exitDrift = 3
)

const usage = `usage: hangar-cloudflare plan|apply|verify --config FILE --out DIR

  plan     print what apply would change; changes nothing
  apply    converge Cloudflare and write the outputs
  verify   exit 3 if anything differs from the config

The API token is read from CLOUDFLARE_API_TOKEN.
`

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	code := run(ctx, os.Args[1:], os.Getenv, os.Stdout, os.Stderr)
	stop()
	os.Exit(code)
}

func run(ctx context.Context, args []string, getenv func(string) string, stdout, stderr io.Writer) int {
	if len(args) == 0 {
		_, _ = fmt.Fprint(stderr, usage)
		return exitUsage
	}
	mode := args[0]
	if mode != "plan" && mode != "apply" && mode != "verify" {
		_, _ = fmt.Fprint(stderr, usage)
		return exitUsage
	}
	fs := flag.NewFlagSet("hangar-cloudflare "+mode, flag.ContinueOnError)
	fs.SetOutput(stderr)
	configPath := fs.String("config", "", "config file (kept outside the repo)")
	outDir := fs.String("out", "", "directory for the state file and the outputs")
	timeout := fs.Duration("timeout", 5*time.Minute, "bound on the whole run")
	apiURL := fs.String("api-url", cloudflare.DefaultBaseURL, "Cloudflare API root")
	if err := fs.Parse(args[1:]); err != nil {
		return exitUsage
	}
	if *configPath == "" || *outDir == "" || fs.NArg() > 0 {
		_, _ = fmt.Fprint(stderr, usage)
		return exitUsage
	}
	token := getenv("CLOUDFLARE_API_TOKEN")
	if token == "" {
		_, _ = fmt.Fprintln(stderr, "hangar-cloudflare: CLOUDFLARE_API_TOKEN is not set")
		return exitUsage
	}
	cfg, err := converge.LoadConfig(*configPath)
	if err != nil {
		_, _ = fmt.Fprintln(stderr, "hangar-cloudflare:", err)
		return exitError
	}
	apply := mode == "apply"
	opts := []cloudflare.Option{cloudflare.WithBaseURL(*apiURL)}
	if !apply {
		opts = append(opts, cloudflare.ReadOnly())
	}
	client, err := cloudflare.New(cloudflare.Secret(token), opts...)
	if err != nil {
		_, _ = fmt.Fprintln(stderr, "hangar-cloudflare:", err)
		return exitError
	}
	ctx, cancel := context.WithTimeout(ctx, *timeout)
	defer cancel()
	e := &converge.Engine{Client: client, Config: cfg, Dir: *outDir, Apply: apply, Log: stderr}
	res, err := e.Run(ctx)
	if res != nil {
		printResult(stdout, mode, res, err == nil)
	}
	if err != nil {
		if errors.Is(err, converge.ErrNotOwned) {
			_, _ = fmt.Fprintln(stderr, "hangar-cloudflare:", err)
		} else {
			_, _ = fmt.Fprintln(stderr, "hangar-cloudflare: "+mode+":", err)
		}
		return exitError
	}
	if mode == "verify" && len(res.Changes) > 0 {
		return exitDrift
	}
	return exitOK
}

func printResult(w io.Writer, mode string, res *converge.Result, complete bool) {
	for _, c := range res.Changes {
		sym := map[string]string{"create": "+", "update": "~", "write": ">"}[c.Action]
		_, _ = fmt.Fprintf(w, "%s %s %s\n", sym, c.Action, c.Object)
		for _, f := range c.Fields {
			_, _ = fmt.Fprintf(w, "    %s: %s\n      -> %s\n", f.Name, f.Have, f.Want)
		}
	}
	for _, n := range res.Notes {
		_, _ = fmt.Fprintf(w, "note: %s\n", n)
	}
	n := len(res.Changes)
	switch {
	case !complete:
		_, _ = fmt.Fprintf(w, "Stopped by an error after %d change(s); the last one may not have been made.\n", n)
	case n == 0:
		_, _ = fmt.Fprintln(w, "No changes.")
	case mode == "apply":
		_, _ = fmt.Fprintf(w, "Applied %d change(s).\n", n)
	case mode == "verify":
		_, _ = fmt.Fprintf(w, "Drift: %d change(s) needed.\n", n)
	default:
		_, _ = fmt.Fprintf(w, "Plan: %d change(s).\n", n)
	}
}
