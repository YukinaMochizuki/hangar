package cli

import (
	"fmt"
	"strings"
	"time"

	"connectrpc.com/connect"
	"github.com/spf13/cobra"
	"google.golang.org/protobuf/proto"

	computev1alpha1 "github.com/YukinaMochizuki/hangar/gen/go/hangar/compute/v1alpha1"
	"github.com/YukinaMochizuki/hangar/gen/go/hangar/registry"
	"github.com/YukinaMochizuki/hangar/internal/client"
)

// runStateAttempts bounds how often stop and start read the container again
// after another writer changed it between their read and their apply.
const runStateAttempts = 5

// runStateVerb is what stop or start asks of a container.
type runStateVerb struct {
	name  string
	state computev1alpha1.ContainerRunState
	// done is the condition that is True once the node has done it.
	done string
	// past and already are what the command prints after asking, and when
	// there was nothing to ask.
	past, already string
}

var (
	stopVerb = runStateVerb{
		name: "stop", state: computev1alpha1.ContainerRunState_CONTAINER_RUN_STATE_STOPPED,
		done: "Stopped", past: "stopped", already: "already stopped",
	}
	startVerb = runStateVerb{
		name: "start", state: computev1alpha1.ContainerRunState_CONTAINER_RUN_STATE_RUNNING,
		done: "Ready", past: "started", already: "already running",
	}
)

// has reports whether the container already asks for the verb's state. An
// unset run state means RUNNING, and one this CLI does not know is taken as
// not running.
func (v runStateVerb) has(c *computev1alpha1.Container) bool {
	s := c.GetSpec().GetRunState()
	if s == computev1alpha1.ContainerRunState_CONTAINER_RUN_STATE_UNSPECIFIED {
		s = computev1alpha1.ContainerRunState_CONTAINER_RUN_STATE_RUNNING
	}
	return s == v.state
}

func (a *app) stopCmd() *cobra.Command {
	return a.runStateCmd(stopVerb, "Stop a container",
		`Stop a container: its process gets SIGTERM, then SIGKILL after a grace, and
its VM is powered off. The restart policy never brings a stopped container
back; its root disk and address are kept for hangar start.

This sets spec.runState to STOPPED and leaves the rest of the spec as it is.
A container already asked to stop is left alone, and the command succeeds.
Applying a manifest that leaves runState out starts the container again.

--wait returns once the condition Stopped is True, as
hangar wait KIND NAME --for=condition=Stopped does.`,
		"  hangar stop container api\n  hangar stop ct api --wait --timeout=2m")
}

func (a *app) startCmd() *cobra.Command {
	return a.runStateCmd(startVerb, "Start a stopped container",
		`Start a stopped container: it boots again on the root disk it was stopped
with.

This sets spec.runState to RUNNING and leaves the rest of the spec as it is.
A container already asked to run is left alone, and the command succeeds.

--wait returns once the condition Ready is True, as
hangar wait KIND NAME --for=condition=Ready does.`,
		"  hangar start container api\n  hangar start ct api --wait -o json")
}

func (a *app) runStateCmd(v runStateVerb, short, long, example string) *cobra.Command {
	var output string
	var wait bool
	var timeout time.Duration
	cmd := &cobra.Command{
		Use:     v.name + " KIND NAME",
		Short:   short,
		Long:    long,
		Example: example,
		Args:    args(2, 2),
		RunE: func(cmd *cobra.Command, args []string) error {
			if output != "" && output != "name" && output != "json" {
				return usageErr("%s prints name or json, not %q", v.name, output)
			}
			r, err := findKind(args[0])
			if err != nil {
				return err
			}
			if r.Kind != "Container" {
				return usageErr("%s works on containers, not %s", v.name, r.Plural)
			}
			cl, c, err := a.client()
			if err != nil {
				return err
			}
			project, err := a.projectFor(r, c)
			if err != nil {
				return err
			}
			key := client.Key{Project: project, Name: args[1]}
			o, changed, err := setRunState(cmd, cl, r, key, v)
			if err != nil {
				return err
			}
			switch output {
			case "json":
				if err := a.printObjects(cmd.OutOrStdout(), "json", r, []proto.Message{o}, true, false); err != nil {
					return err
				}
			case "name":
				_, _ = fmt.Fprintln(cmd.OutOrStdout(), objectName(r, o))
			default:
				what := v.past
				if !changed {
					what = v.already
				}
				_, _ = fmt.Fprintf(cmd.OutOrStdout(), "%s %s\n", objectName(r, o), what)
			}
			if !wait {
				return nil
			}
			return await(cmd, cl, r, key, v.done, timeout)
		},
	}
	cmd.Flags().StringVarP(&output, "output", "o", "", "name or json")
	cmd.Flags().BoolVar(&wait, "wait", false, "wait until the container has "+v.past)
	cmd.Flags().DurationVar(&timeout, "timeout", 5*time.Minute, "with --wait, give up after this long")
	return cmd
}

// setRunState reads the container and applies it back with only the run state
// changed, at the resource version it read, so a write in between is never
// overwritten: the apply fails as a conflict and is tried again on a fresh
// read. It returns the container as stored and whether it changed it.
func setRunState(cmd *cobra.Command, cl *client.Client, r registry.Resource, key client.Key, v runStateVerb) (proto.Message, bool, error) {
	var err error
	for range runStateAttempts {
		var o proto.Message
		if o, err = cl.Get(cmd.Context(), r, key); err != nil {
			return nil, false, err
		}
		ct, ok := o.(*computev1alpha1.Container)
		if !ok {
			return nil, false, fmt.Errorf("the API returned a %T for a container", o)
		}
		if v.has(ct) {
			return ct, false, nil
		}
		if ct.Spec == nil {
			ct.Spec = &computev1alpha1.ContainerSpec{}
		}
		ct.Spec.RunState = v.state
		var res client.ApplyResult
		res, err = cl.Apply(cmd.Context(), r, ct)
		if connect.CodeOf(err) == connect.CodeAborted {
			continue
		}
		if err != nil {
			return nil, false, err
		}
		for _, w := range res.Warnings {
			_, _ = fmt.Fprintf(cmd.ErrOrStderr(), "warning: %s: %s\n", objectName(r, res.Object), w)
		}
		return res.Object, true, nil
	}
	return nil, false, fmt.Errorf("%s/%s changed under each of %d tries: %w", strings.ToLower(r.Kind), key.Name, runStateAttempts, err)
}
