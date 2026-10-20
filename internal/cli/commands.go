package cli

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"connectrpc.com/connect"
	"github.com/spf13/cobra"
	"google.golang.org/protobuf/proto"

	metav1 "github.com/YukinaMochizuki/hangar/gen/go/hangar/meta/v1"
	"github.com/YukinaMochizuki/hangar/gen/go/hangar/registry"
	"github.com/YukinaMochizuki/hangar/internal/apimachinery/object"
	"github.com/YukinaMochizuki/hangar/internal/client"
)

func (a *app) configCmd() *cobra.Command {
	cmd := &cobra.Command{Use: "config", Short: "Manage contexts: which endpoint, project and credentials to use"}

	var c Context
	var use bool
	set := &cobra.Command{
		Use:   "set-context NAME --endpoint URL",
		Short: "Create or replace a context",
		Args:  args(1, 1),
		RunE: func(cmd *cobra.Command, args []string) error {
			if c.Endpoint == "" {
				return usageErr("--endpoint is required")
			}
			if c.TokenFile != "" {
				abs, err := filepath.Abs(c.TokenFile)
				if err != nil {
					return err
				}
				c.TokenFile = abs
			}
			if err := c.check(); err != nil {
				return err
			}
			cfg, err := loadConfig(a.configPath())
			if err != nil {
				return err
			}
			ctx := c
			cfg.Contexts[args[0]] = &ctx
			if use || cfg.CurrentContext == "" {
				cfg.CurrentContext = args[0]
			}
			if err := a.prepareConfigDir(); err != nil {
				return err
			}
			return cfg.save(a.configPath())
		},
	}
	set.Flags().StringVar(&c.Endpoint, "endpoint", "", "API URL, such as https://hangar.example.com, or unix:///run/hangar/admin.sock for root on the host")
	set.Flags().StringVar(&c.Project, "project", "", "default project")
	set.Flags().StringVar(&c.Auth, "auth", "", "oidc (the default: sign in with hangar login), token (a service token from --token-file), dev, or local (the default for a unix:// endpoint)")
	set.Flags().StringVar(&c.User, "user", "", "email to send in dev auth")
	set.Flags().StringVar(&c.TokenFile, "token-file", "", "file holding the service token, mode 0600, for token auth")
	set.Flags().BoolVar(&use, "use", false, "make it the current context")

	useCmd := &cobra.Command{
		Use:   "use-context NAME",
		Short: "Make a context current",
		Args:  args(1, 1),
		RunE: func(cmd *cobra.Command, args []string) error {
			cfg, err := loadConfig(a.configPath())
			if err != nil {
				return err
			}
			if _, ok := cfg.Contexts[args[0]]; !ok {
				return usageErr("no context named %q", args[0])
			}
			cfg.CurrentContext = args[0]
			if err := a.prepareConfigDir(); err != nil {
				return err
			}
			return cfg.save(a.configPath())
		},
	}

	var output string
	view := &cobra.Command{
		Use:   "view",
		Short: "Show the contexts",
		Args:  args(0, 0),
		RunE: func(cmd *cobra.Command, _ []string) error {
			cfg, err := loadConfig(a.configPath())
			if err != nil {
				return err
			}
			if output == "json" {
				js, err := json.MarshalIndent(cfg, "", "  ")
				if err != nil {
					return err
				}
				_, err = fmt.Fprintln(cmd.OutOrStdout(), string(js))
				return err
			}
			for _, name := range cfg.names() {
				mark := " "
				if name == cfg.CurrentContext {
					mark = "*"
				}
				c := cfg.Contexts[name]
				_, _ = fmt.Fprintf(cmd.OutOrStdout(), "%s %s\t%s\tproject=%s\n", mark, name, c.Endpoint, c.Project)
			}
			return nil
		},
	}
	view.Flags().StringVarP(&output, "output", "o", "", "json for machine-readable output")

	cmd.AddCommand(set, useCmd, view)
	return cmd
}

func (a *app) applyCmd() *cobra.Command {
	var files []string
	var output string
	var createOnly bool
	cmd := &cobra.Command{
		Use:   "apply -f FILE|DIR|-",
		Short: "Create or replace the objects in YAML or JSON files",
		Long: `Create or replace the objects in YAML or JSON files.

A number written where the API has a string is sent as the text written, so
"cpu: 2" and an env "value: 8080" need no quotes, and 1.10 stays 1.10. A
number anchored outside a string field and reached through a YAML alias or
merge key (<<) is not converted, so quote it where it is anchored.

With --create-only, an object whose name exists is refused and left as it
is, and apply stops there; the objects before it stay created.`,
		Args: args(0, 0),
		RunE: func(cmd *cobra.Command, _ []string) error {
			if len(files) == 0 {
				return usageErr("-f is required")
			}
			if output != "" && output != "name" && output != "json" {
				return usageErr("apply prints name or json, not %q", output)
			}
			manifests, err := readManifests(files, cmd.InOrStdin())
			if err != nil {
				return err
			}
			cl, c, err := a.client()
			if err != nil {
				return err
			}
			var applied []json.RawMessage
			for _, m := range manifests {
				meta := object.Meta(m.obj)
				if m.r.ProjectScoped() && meta.GetProject() == "" {
					if meta.Project, err = a.projectFor(m.r, c); err != nil {
						return fmt.Errorf("%s: %w", m.source, err)
					}
				}
				apply := cl.Apply
				if createOnly {
					apply = cl.Create
				}
				res, err := apply(cmd.Context(), m.r, m.obj)
				if err != nil {
					return fmt.Errorf("%s: %w", m.source, err)
				}
				for _, w := range res.Warnings {
					_, _ = fmt.Fprintf(cmd.ErrOrStderr(), "warning: %s: %s\n", objectName(m.r, res.Object), w)
				}
				switch output {
				case "json":
					js, err := marshalJSON(res.Object)
					if err != nil {
						return err
					}
					applied = append(applied, js)
				case "name":
					_, _ = fmt.Fprintln(cmd.OutOrStdout(), objectName(m.r, res.Object))
				default:
					verb := "applied"
					if res.Created {
						verb = "created"
					}
					_, _ = fmt.Fprintf(cmd.OutOrStdout(), "%s %s\n", objectName(m.r, res.Object), verb)
				}
			}
			if output == "json" {
				js, err := json.MarshalIndent(map[string]any{"items": applied}, "", "  ")
				if err != nil {
					return err
				}
				_, err = fmt.Fprintln(cmd.OutOrStdout(), string(js))
				return err
			}
			return nil
		},
	}
	cmd.Flags().StringArrayVarP(&files, "filename", "f", nil, "file, directory or - for stdin; repeatable")
	cmd.Flags().StringVarP(&output, "output", "o", "", "name or json")
	cmd.Flags().BoolVar(&createOnly, "create-only", false, "refuse objects that exist instead of replacing them")
	return cmd
}

func (a *app) getCmd() *cobra.Command {
	var output, selector string
	var all bool
	cmd := &cobra.Command{
		Use:   "get KIND [NAME...]",
		Short: "Show objects of a kind",
		Args: func(cmd *cobra.Command, args []string) error {
			if len(args) == 0 {
				return usageErr("usage: %s\n%s", cmd.UseLine(), knownKinds())
			}
			return nil
		},
		RunE: func(cmd *cobra.Command, args []string) error {
			if output == "" {
				output = "table"
			}
			if err := checkOutput(output); err != nil {
				return err
			}
			r, err := findKind(args[0])
			if err != nil {
				return err
			}
			cl, c, err := a.client()
			if err != nil {
				return err
			}
			var project string
			acrossProjects := all && r.ProjectScoped()
			if !acrossProjects {
				if project, err = a.projectFor(r, c); err != nil {
					return err
				}
			}
			var objs []proto.Message
			if names := args[1:]; len(names) > 0 {
				for _, n := range names {
					o, err := cl.Get(cmd.Context(), r, client.Key{Project: project, Name: n})
					if err != nil {
						return err
					}
					objs = append(objs, o)
				}
				return a.printObjects(cmd.OutOrStdout(), output, r, objs, len(names) == 1, false)
			}
			objs, _, err = cl.ListAll(cmd.Context(), r, client.ListOptions{Project: project, LabelSelector: selector})
			if err != nil {
				return err
			}
			return a.printObjects(cmd.OutOrStdout(), output, r, objs, false, all)
		},
	}
	cmd.Flags().StringVarP(&output, "output", "o", "", "table, json, yaml or name")
	cmd.Flags().StringVarP(&selector, "selector", "l", "", "label selector, such as app=web,tier!=db")
	cmd.Flags().BoolVarP(&all, "all-projects", "A", false, "list across every project you can read")
	return cmd
}

func (a *app) deleteCmd() *cobra.Command {
	var files []string
	var output string
	cmd := &cobra.Command{
		Use:   "delete KIND NAME... | delete -f FILE",
		Short: "Delete objects",
		Args:  args(0, -1),
		RunE: func(cmd *cobra.Command, args []string) error {
			cl, c, err := a.client()
			if err != nil {
				return err
			}
			type target struct {
				m   manifest
				key client.Key
			}
			var targets []target
			switch {
			case len(files) > 0 && len(args) == 0:
				manifests, err := readManifests(files, cmd.InOrStdin())
				if err != nil {
					return err
				}
				for _, m := range manifests {
					meta := object.Meta(m.obj)
					key := client.Key{Project: meta.GetProject(), Name: meta.GetName()}
					if m.r.ProjectScoped() && key.Project == "" {
						if key.Project, err = a.projectFor(m.r, c); err != nil {
							return err
						}
					}
					targets = append(targets, target{m, key})
				}
			case len(files) == 0 && len(args) >= 2:
				r, err := findKind(args[0])
				if err != nil {
					return err
				}
				project, err := a.projectFor(r, c)
				if err != nil {
					return err
				}
				for _, n := range args[1:] {
					targets = append(targets, target{manifest{r: r}, client.Key{Project: project, Name: n}})
				}
			default:
				return usageErr("usage: %s", cmd.UseLine())
			}
			fromFiles := len(files) > 0
			if fromFiles {
				// Files list owners before what they own; deleting in reverse
				// removes contents first, as a cascade from the owner would.
				slices.Reverse(targets)
			}
			for _, t := range targets {
				o, err := cl.Delete(cmd.Context(), t.m.r, t.key)
				// An object in a file may already be gone, taken by the
				// cascade from an owner deleted before it; that is the
				// outcome asked for.
				if fromFiles && connect.CodeOf(err) == connect.CodeNotFound {
					name := strings.ToLower(t.m.r.Kind) + "/" + t.key.Name
					if output == "name" {
						_, _ = fmt.Fprintln(cmd.OutOrStdout(), name)
					} else {
						_, _ = fmt.Fprintf(cmd.OutOrStdout(), "%s already deleted\n", name)
					}
					continue
				}
				if err != nil {
					return err
				}
				if output == "name" {
					_, _ = fmt.Fprintln(cmd.OutOrStdout(), objectName(t.m.r, o))
				} else {
					_, _ = fmt.Fprintf(cmd.OutOrStdout(), "%s deleted\n", objectName(t.m.r, o))
				}
			}
			return nil
		},
	}
	cmd.Flags().StringArrayVarP(&files, "filename", "f", nil, "file, directory or - for stdin; repeatable")
	cmd.Flags().StringVarP(&output, "output", "o", "", "name")
	return cmd
}

func (a *app) waitCmd() *cobra.Command {
	var forCond string
	var timeout time.Duration
	cmd := &cobra.Command{
		Use:   "wait KIND NAME --for=condition=TYPE|delete",
		Short: "Wait until an object meets a condition or is deleted",
		Long: "condition=TYPE waits until the condition is True for the object's current\n" +
			"generation, so a change just applied has taken effect.",
		Args: args(2, 2),
		RunE: func(cmd *cobra.Command, args []string) error {
			var condType string
			switch {
			case forCond == "delete":
			case len(forCond) > len("condition=") && forCond[:len("condition=")] == "condition=":
				condType = forCond[len("condition="):]
			default:
				return usageErr("--for must be condition=TYPE or delete")
			}
			r, err := findKind(args[0])
			if err != nil {
				return err
			}
			cl, c, err := a.client()
			if err != nil {
				return err
			}
			project, err := a.projectFor(r, c)
			if err != nil {
				return err
			}
			return await(cmd, cl, r, client.Key{Project: project, Name: args[1]}, condType, timeout)
		},
	}
	cmd.Flags().StringVar(&forCond, "for", "", "condition=TYPE or delete")
	cmd.Flags().DurationVar(&timeout, "timeout", 5*time.Minute, "give up after this long")
	return cmd
}

// await waits until the object's condition condType is True for its current
// generation, or with an empty condType until the object is gone.
func await(cmd *cobra.Command, cl *client.Client, r registry.Resource, key client.Key, condType string, timeout time.Duration) error {
	ctx, cancel := context.WithTimeout(cmd.Context(), timeout)
	defer cancel()
	done := func(o proto.Message) bool {
		if condType == "" {
			return o == nil
		}
		return o != nil && conditionMet(o, condType)
	}

	o, err := cl.Get(ctx, r, key)
	switch {
	case connect.CodeOf(err) == connect.CodeNotFound && condType == "":
		return nil
	case err != nil:
		return timeoutOr(ctx, err)
	case done(o):
		return nil
	}
	errDone := errors.New("done")
	err = cl.Watch(ctx, r, client.ListOptions{Project: key.Project}, object.Meta(o).GetResourceVersion(), func(ev client.Event) error {
		if ev.Object == nil || object.Meta(ev.Object).GetName() != key.Name {
			return nil
		}
		if ev.Type == metav1.EventType_EVENT_TYPE_DELETED {
			if condType == "" {
				return errDone
			}
			return &exitError{code: ExitNotFound, err: fmt.Errorf("%s was deleted", objectName(r, ev.Object))}
		}
		if done(ev.Object) {
			return errDone
		}
		return nil
	})
	if errors.Is(err, errDone) {
		return nil
	}
	return timeoutOr(ctx, err)
}

// conditionMet reports whether the condition is True and was computed for the
// object's current generation.
func conditionMet(o proto.Message, typ string) bool {
	c := condition(o, typ)
	if c == nil || c.GetStatus() != metav1.ConditionStatus_CONDITION_STATUS_TRUE {
		return false
	}
	observed := c.GetObservedGeneration()
	if observed == 0 {
		observed = statusObservedGeneration(o)
	}
	return observed >= object.Meta(o).GetGeneration()
}

func statusObservedGeneration(o proto.Message) int64 {
	st := object.Status(o).ProtoReflect()
	fd := st.Descriptor().Fields().ByName("observed_generation")
	if fd == nil {
		return 0
	}
	return st.Get(fd).Int()
}

func timeoutOr(ctx context.Context, err error) error {
	if ctx.Err() == context.DeadlineExceeded {
		return &exitError{code: ExitTimeout, err: errors.New("timed out")}
	}
	if err == nil {
		return errors.New("watch ended before the condition was met")
	}
	return err
}
