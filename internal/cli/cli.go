// Package cli is the hangar command. It is built for agents as much as for
// people: it never prompts, every command can print JSON, and the exit code
// says what went wrong (the Exit constants).
package cli

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"time"

	"connectrpc.com/connect"
	"github.com/spf13/cobra"

	"github.com/YukinaMochizuki/hangar/gen/go/hangar/registry"
	"github.com/YukinaMochizuki/hangar/internal/client"
)

// Exit codes.
const (
	ExitOK       = 0
	ExitError    = 1
	ExitUsage    = 2
	ExitNotFound = 3
	ExitConflict = 4
	ExitTimeout  = 5
)

// IO is what a run reads and writes.
type IO struct {
	In       io.Reader
	Out, Err io.Writer
	// Env looks up environment variables.
	Env func(string) string
}

// Options replace the outside world in tests; zero values use the real one.
type Options struct {
	HTTPClient     connect.HTTPClient
	RunCloudflared func(ctx context.Context, args ...string) (string, error)
	Now            func() time.Time
}

// Run runs the command line and returns the exit code.
func Run(ctx context.Context, args []string, stdio IO, opts Options) int {
	a := &app{io: stdio, env: stdio.Env, opts: opts}
	if a.env == nil {
		a.env = func(string) string { return "" }
	}
	if a.opts.HTTPClient == nil {
		a.opts.HTTPClient = http.DefaultClient
	}
	if a.opts.RunCloudflared == nil {
		a.opts.RunCloudflared = runCloudflared
	}
	if a.opts.Now == nil {
		a.opts.Now = time.Now
	}
	root := a.root()
	root.SetArgs(args)
	root.SetIn(stdio.In)
	root.SetOut(stdio.Out)
	root.SetErr(stdio.Err)
	err := root.ExecuteContext(ctx)
	if err == nil {
		return ExitOK
	}
	_, _ = fmt.Fprintln(stdio.Err, "error:", err)
	return exitCode(err)
}

type exitError struct {
	code int
	err  error
}

func (e *exitError) Error() string { return e.err.Error() }
func (e *exitError) Unwrap() error { return e.err }

func usageErr(format string, args ...any) error {
	return &exitError{code: ExitUsage, err: fmt.Errorf(format, args...)}
}

func exitCode(err error) int {
	var ee *exitError
	if errors.As(err, &ee) {
		return ee.code
	}
	switch connect.CodeOf(err) {
	case connect.CodeNotFound:
		return ExitNotFound
	case connect.CodeAlreadyExists, connect.CodeAborted:
		return ExitConflict
	case connect.CodeDeadlineExceeded:
		return ExitTimeout
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return ExitTimeout
	}
	return ExitError
}

type app struct {
	io   IO
	env  func(string) string
	opts Options

	contextName string
	project     string
}

func (a *app) root() *cobra.Command {
	root := &cobra.Command{
		Use:           "hangar",
		Short:         "Manage Hangar resources",
		SilenceUsage:  true,
		SilenceErrors: true,
	}
	root.PersistentFlags().StringVar(&a.contextName, "context", "", "context to use instead of the current one")
	root.PersistentFlags().StringVarP(&a.project, "project", "p", "", "project for project-scoped kinds, instead of the context's")
	root.SetFlagErrorFunc(func(_ *cobra.Command, err error) error { return &exitError{code: ExitUsage, err: err} })
	root.AddCommand(a.configCmd(), a.loginCmd(), a.applyCmd(), a.getCmd(), a.deleteCmd(), a.waitCmd())
	return root
}

func (a *app) runCloudflared(ctx context.Context, args ...string) (string, error) {
	return a.opts.RunCloudflared(ctx, args...)
}

func (a *app) configPath() string { return configPath(a.env) }

// current returns the context in effect.
func (a *app) current() (*Context, error) {
	cfg, err := loadConfig(a.configPath())
	if err != nil {
		return nil, err
	}
	name := a.contextName
	if name == "" {
		name = cfg.CurrentContext
	}
	if name == "" {
		return nil, usageErr("no context; create one with `hangar config set-context`")
	}
	c, ok := cfg.Contexts[name]
	if !ok {
		return nil, usageErr("no context named %q", name)
	}
	return c, nil
}

// client returns an API client for the context in effect.
func (a *app) client() (*client.Client, *Context, error) {
	c, err := a.current()
	if err != nil {
		return nil, nil, err
	}
	creds, err := a.credentials(c)
	if err != nil {
		return nil, nil, err
	}
	return client.New(a.opts.HTTPClient, c.Endpoint, connect.WithInterceptors(creds)), c, nil
}

// projectFor returns the project to use for r, if r is project-scoped.
func (a *app) projectFor(r registry.Resource, c *Context) (string, error) {
	if !r.ProjectScoped() {
		if a.project != "" {
			return "", usageErr("%s are not in projects; drop --project", r.Plural)
		}
		return "", nil
	}
	p := a.project
	if p == "" {
		p = c.Project
	}
	if p == "" {
		return "", usageErr("%s are project-scoped; pass --project or set one on the context", r.Plural)
	}
	return p, nil
}

func findKind(name string) (registry.Resource, error) {
	r, ok := registry.Find(name)
	if !ok {
		return registry.Resource{}, usageErr("unknown kind %q", name)
	}
	return r, nil
}

// args checks the number of positional arguments, as a usage error.
func args(min, max int) cobra.PositionalArgs {
	return func(cmd *cobra.Command, args []string) error {
		if len(args) < min || (max >= 0 && len(args) > max) {
			return usageErr("usage: %s", cmd.UseLine())
		}
		return nil
	}
}
