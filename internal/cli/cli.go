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
	"os"
	"os/signal"
	"sync/atomic"
	"syscall"
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
	HTTPClient connect.HTTPClient
	// OpenBrowser opens a URL in a browser, for hangar login.
	OpenBrowser func(url string) error
	Now         func() time.Time
	// Signals relays the process's signals sigs to c, as signal.Notify
	// does, until stop.
	Signals func(c chan<- os.Signal, sigs ...os.Signal) (stop func())
	// Terminal returns the terminal IO.In is, if it is one.
	Terminal func(in io.Reader) (Terminal, bool)
	// ExecPing and ExecPong time how hangar exec tells the server is gone,
	// as apiserver.Config's do the other way; zero means 30 s each.
	ExecPing, ExecPong time.Duration
	// SSH is OpenSSH's ssh, which hangar ssh runs; empty looks it up on
	// PATH. Self is how ssh runs hangar for ssh-proxy; empty is this
	// executable.
	SSH, Self string
}

// interrupts are the signals that end a command, unless it takes them.
var interrupts = []os.Signal{os.Interrupt, syscall.SIGTERM}

// Run runs the command line and returns the exit code.
func Run(ctx context.Context, args []string, stdio IO, opts Options) int {
	a := &app{io: stdio, env: stdio.Env, opts: opts}
	if a.env == nil {
		a.env = func(string) string { return "" }
	}
	if a.opts.HTTPClient == nil {
		a.opts.HTTPClient = http.DefaultClient
	}
	if a.opts.OpenBrowser == nil {
		a.opts.OpenBrowser = openBrowser
	}
	if a.opts.Now == nil {
		a.opts.Now = time.Now
	}
	if a.opts.Signals == nil {
		a.opts.Signals = func(c chan<- os.Signal, sigs ...os.Signal) func() {
			signal.Notify(c, sigs...)
			return func() { signal.Stop(c) }
		}
	}
	if a.opts.Terminal == nil {
		a.opts.Terminal = func(in io.Reader) (Terminal, bool) { return terminalOf(in) }
	}
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	sigs := make(chan os.Signal, 1)
	stop := a.opts.Signals(sigs, interrupts...)
	defer stop()
	go func() {
		for {
			select {
			case <-ctx.Done():
				return
			case sig := <-sigs:
				if take := a.takeInterrupts.Load(); take == nil || !(*take)(sig) {
					cancel()
				}
			}
		}
	}()
	root := a.root()
	root.SetArgs(args)
	root.SetIn(stdio.In)
	root.SetOut(stdio.Out)
	root.SetErr(stdio.Err)
	err := root.ExecuteContext(ctx)
	var remote exitStatus
	switch {
	case err == nil:
		return ExitOK
	case errors.As(err, &remote):
		return int(remote)
	}
	_, _ = fmt.Fprintln(stdio.Err, "error:", err)
	return exitCode(err)
}

// exitStatus ends a command that ran a process elsewhere with the process's
// exit code, saying nothing.
type exitStatus int

func (e exitStatus) Error() string { return fmt.Sprintf("exit status %d", int(e)) }

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
	// takeInterrupts, if set, is given SIGINT and SIGTERM first; what it
	// takes does not end the command.
	takeInterrupts atomic.Pointer[func(os.Signal) bool]

	contextName string
	project     string
	// warnedConfigDir is set once the config directory's mode was warned
	// about.
	warnedConfigDir bool
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
	root.AddCommand(a.configCmd(), a.loginCmd(), a.logoutCmd(), a.signupCmd(), a.whoamiCmd(), a.applyCmd(), a.createCmd(), a.getCmd(), a.deleteCmd(), a.waitCmd(), a.stopCmd(), a.startCmd(), a.logsCmd(), a.execCmd(), a.sshCmd(), a.sshProxyCmd())
	return root
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
	hc, base := a.httpClient(c)
	return client.New(hc, base, connect.WithInterceptors(creds)), c, nil
}

// httpClient returns the client and base URL that reach the context's API:
// the Options' for a URL, or one dialing the socket of a unix:// endpoint.
func (a *app) httpClient(c *Context) (connect.HTTPClient, string) {
	if path, ok := c.socketPath(); ok {
		return client.UnixSocket(path), client.UnixSocketURL
	}
	return a.opts.HTTPClient, c.Endpoint
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
