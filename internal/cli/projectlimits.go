package cli

import (
	"cmp"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"slices"
	"strconv"
	"strings"
	"text/tabwriter"

	"connectrpc.com/connect"
	"github.com/spf13/cobra"
	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/reflect/protoreflect"
	"sigs.k8s.io/yaml"

	"github.com/YukinaMochizuki/hangar/gen/go/hangar/registry"
	tenancyv1alpha1 "github.com/YukinaMochizuki/hangar/gen/go/hangar/tenancy/v1alpha1"
	"github.com/YukinaMochizuki/hangar/internal/client"
)

// projectLimitNames are the Limits fields a project may set, in the order
// they print: projects, domains and machine_calls are the plan's alone.
var projectLimitNames = []protoreflect.Name{
	"cpu", "memory", "disk", "machines", "networks", "addresses", "public_endpoints",
	"port_forwards", "service_tokens", "file_shares", "custom_images", "network_endpoints",
}

// limitFlag is a Limits field as a flag names it: public_endpoints is
// --public-endpoints.
func limitFlag(n protoreflect.Name) string { return strings.ReplaceAll(string(n), "_", "-") }

// limitField is the field of Limits or Usage m named n.
func limitField(m protoreflect.Message, n protoreflect.Name) protoreflect.FieldDescriptor {
	return m.Descriptor().Fields().ByName(n)
}

// limitText is the field n of m as the CLI prints it, "" when m leaves it
// unset (a Limits) or m is nil.
func limitText(m proto.Message, n protoreflect.Name) string {
	if m == nil || !m.ProtoReflect().IsValid() {
		return ""
	}
	r := m.ProtoReflect()
	fd := limitField(r, n)
	if fd.HasPresence() && !r.Has(fd) {
		return ""
	}
	v := r.Get(fd)
	if fd.Kind() == protoreflect.StringKind {
		return zeroIfEmpty(v.String())
	}
	return strconv.FormatInt(v.Int(), 10)
}

// zeroIfEmpty is a Usage quantity, which the apiserver may leave empty, as 0.
func zeroIfEmpty(s string) string {
	if s == "" {
		return "0"
	}
	return s
}

func (a *app) projectLimitsCmd() *cobra.Command {
	var output string
	cmd := &cobra.Command{
		Use:   "limits [NAME]",
		Short: "Show a project's limits: its own, its tenant's plan's, those in effect, and its usage",
		Long: `Show a project's limits, the context's project when NAME is not given.

OWN is what the project's spec.limits sets, set with ` + "`hangar project set-limits`" + `;
PLAN is the tenant's plan's, shown only to whoever may read the tenant; IN
EFFECT is the smaller of the two, and USAGE what the project holds. An empty
cell is no limit. The plan alone limits projects, domains and machine calls.`,
		Args: args(0, 1),
		RunE: func(cmd *cobra.Command, args []string) error {
			if output != "" && output != "json" && output != "yaml" {
				return usageErr("limits prints a table, json or yaml, not %q", output)
			}
			cl, c, err := a.client()
			if err != nil {
				return err
			}
			name, err := a.limitsProject(args, c)
			if err != nil {
				return err
			}
			obj, err := cl.Get(cmd.Context(), registry.Project, client.Key{Name: name})
			if err != nil {
				return err
			}
			p := obj.(*tenancyv1alpha1.Project)
			// Someone with a role in the project alone may not read its
			// tenant; the plan is then left out.
			var plan *tenancyv1alpha1.Limits
			t, err := cl.Get(cmd.Context(), registry.Tenant, client.Key{Name: p.GetSpec().GetTenant()})
			switch connect.CodeOf(err) {
			case connect.CodePermissionDenied, connect.CodeNotFound:
			default:
				if err != nil {
					return err
				}
				plan = t.(*tenancyv1alpha1.Tenant).GetStatus().GetLimits()
				if plan == nil {
					plan = &tenancyv1alpha1.Limits{}
				}
			}
			return printProjectLimits(cmd.OutOrStdout(), output, p, plan)
		},
	}
	cmd.Flags().StringVarP(&output, "output", "o", "", "json or yaml")
	return cmd
}

// limitsProject is the project NAME names, or the context's.
func (a *app) limitsProject(args []string, c *Context) (string, error) {
	if len(args) == 1 {
		if a.project != "" && a.project != args[0] {
			return "", usageErr("NAME %q and --project %q name different projects", args[0], a.project)
		}
		return args[0], nil
	}
	if p := cmp.Or(a.project, c.Project); p != "" {
		return p, nil
	}
	return "", usageErr("name a project, or pass --project or set one on the context")
}

func printProjectLimits(w io.Writer, output string, p *tenancyv1alpha1.Project, plan *tenancyv1alpha1.Limits) error {
	own, effect, usage := p.GetSpec().GetLimits(), p.GetStatus().GetLimits(), p.GetStatus().GetUsage()
	if output == "json" || output == "yaml" {
		doc := map[string]any{"project": p.GetMetadata().GetName(), "tenant": p.GetSpec().GetTenant()}
		for key, m := range map[string]proto.Message{"own": own, "plan": plan, "inEffect": effect, "usage": usage} {
			if key == "plan" && plan == nil {
				continue
			}
			if m == nil || !m.ProtoReflect().IsValid() {
				m = &tenancyv1alpha1.Limits{}
				if key == "usage" {
					m = &tenancyv1alpha1.Usage{}
				}
			}
			// Usage counts have no presence: a zero is written, not left out,
			// so a script reads 0 rather than null.
			js, err := protojson.MarshalOptions{EmitUnpopulated: key == "usage"}.Marshal(m)
			if err != nil {
				return err
			}
			doc[key] = json.RawMessage(js)
		}
		js, err := json.MarshalIndent(doc, "", "  ")
		if err != nil {
			return err
		}
		if output == "yaml" {
			if js, err = yaml.JSONToYAML(js); err != nil {
				return err
			}
			_, err = w.Write(js)
			return err
		}
		_, err = fmt.Fprintln(w, string(js))
		return err
	}
	tw := tabwriter.NewWriter(w, 0, 4, 3, ' ', 0)
	_, _ = fmt.Fprintln(tw, "LIMIT\tOWN\tPLAN\tIN EFFECT\tUSAGE")
	for _, n := range projectLimitNames {
		planCell := limitText(plan, n)
		if plan == nil {
			planCell = "?"
		}
		_, _ = fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\n", limitFlag(n), dash(limitText(own, n)), dash(planCell), dash(limitText(effect, n)), limitText(usage, n))
	}
	if err := tw.Flush(); err != nil {
		return err
	}
	if plan == nil {
		_, _ = fmt.Fprintf(w, "PLAN is ? where this account may not read tenant %q.\n", p.GetSpec().GetTenant())
	}
	return nil
}

// dash is an unset limit, "-".
func dash(s string) string {
	if s == "" {
		return "-"
	}
	return s
}

func (a *app) projectSetLimitsCmd() *cobra.Command {
	var unset []string
	values := map[protoreflect.Name]*string{}
	cmd := &cobra.Command{
		Use:   "set-limits [NAME] [--cpu Q] [--memory Q] [--disk Q] [--machines N] ... [--unset LIMIT,...]",
		Short: "Set or unset some of a project's own limits, keeping the rest",
		Long: `Set or unset some of a project's own limits, its spec.limits, keeping the
others as they are: the context's project when NAME is not given.

A project's own limits are a budget inside its tenant's plan, such as one to
cap an agent's project; the limits in effect are the smaller of the two. A
limit unset follows the plan. --unset takes limit names, as the flags name
them, repeated or comma-separated, or all.

Changing them needs apply on projects at the tenant, so a role held on the
project alone cannot raise its own budget, and a limit may not be above the
plan's. The plan alone limits projects, domains and machine calls.

The project is written back at the resource version it was read at; when it
changed meanwhile, it is read again and the same change made on it, so
anything else changed meanwhile is kept.`,
		Example: `  hangar project set-limits agent-sandbox --cpu 4 --memory 8Gi --machines 3
  hangar project set-limits --unset disk,machines
  hangar project set-limits agent-sandbox --unset all`,
		Args: args(0, 1),
		RunE: func(cmd *cobra.Command, args []string) error {
			set := map[protoreflect.Name]string{}
			for _, n := range projectLimitNames {
				if cmd.Flags().Changed(limitFlag(n)) {
					set[n] = *values[n]
				}
			}
			clear, err := unsetNames(unset)
			if err != nil {
				return err
			}
			if len(set) == 0 && len(clear) == 0 {
				return usageErr("set or unset at least one limit; see hangar project set-limits --help")
			}
			for _, n := range clear {
				if _, ok := set[n]; ok {
					return usageErr("--%s is both set and unset", limitFlag(n))
				}
			}
			// A value that is no number fails before anything is read.
			if err := changeLimits(&tenancyv1alpha1.Limits{}, set, nil); err != nil {
				return err
			}
			cl, c, err := a.client()
			if err != nil {
				return err
			}
			name, err := a.limitsProject(args, c)
			if err != nil {
				return err
			}
			res, err := applyLimitChange(cmd.Context(), cl, name, set, clear)
			if err != nil {
				return err
			}
			for _, warning := range res.Warnings {
				_, _ = fmt.Fprintf(cmd.ErrOrStderr(), "warning: project/%s: %s\n", name, warning)
			}
			_, err = fmt.Fprintf(cmd.OutOrStdout(), "project/%s: %s\n", name, ownLimitsLine(res.Object.(*tenancyv1alpha1.Project).GetSpec().GetLimits()))
			return err
		},
	}
	for _, n := range projectLimitNames {
		v := new(string)
		values[n] = v
		what := "a count"
		switch n {
		case "cpu":
			what = `vCPUs, such as "4" or "2500m"`
		case "memory", "disk":
			what = `a size, such as "8Gi"`
		}
		cmd.Flags().StringVar(v, limitFlag(n), "", fmt.Sprintf("the project's own %s limit: %s", strings.ReplaceAll(string(n), "_", " "), what))
	}
	cmd.Flags().StringSliceVar(&unset, "unset", nil, "limits to unset, so the plan's holds: names as the flags give them, or all")
	return cmd
}

// limitChangeTries is how many times set-limits reads the project and
// applies its change before giving up on writers that keep changing it.
const limitChangeTries = 5

// applyLimitChange reads the project, changes its own limits and applies it
// at the resource version read. The usage controller writes a project's
// status whenever what its tenant holds changes, which moves that version;
// the change is then made again on the project read anew, other writers'
// changes kept.
func applyLimitChange(ctx context.Context, cl *client.Client, name string, set map[protoreflect.Name]string, clear []protoreflect.Name) (client.ApplyResult, error) {
	var err error
	for range limitChangeTries {
		var obj proto.Message
		if obj, err = cl.Get(ctx, registry.Project, client.Key{Name: name}); err != nil {
			return client.ApplyResult{}, err
		}
		p := obj.(*tenancyv1alpha1.Project)
		if p.Spec == nil {
			p.Spec = &tenancyv1alpha1.ProjectSpec{}
		}
		if p.Spec.Limits == nil {
			p.Spec.Limits = &tenancyv1alpha1.Limits{}
		}
		if err = changeLimits(p.Spec.Limits, set, clear); err != nil {
			return client.ApplyResult{}, err
		}
		// No limits left are sent as no limits field, as a project that never
		// had any is stored, so unsetting what is not set writes nothing.
		if proto.Size(p.Spec.Limits) == 0 {
			p.Spec.Limits = nil
		}
		var res client.ApplyResult
		res, err = cl.Apply(ctx, registry.Project, p)
		if connect.CodeOf(err) != connect.CodeAborted {
			return res, err
		}
	}
	return client.ApplyResult{}, &exitError{code: ExitConflict, err: fmt.Errorf(
		"project %s changed each of the %d times it was written; nothing was applied, run the command again: %w", name, limitChangeTries, err)}
}

// unsetNames reads --unset's limit names.
func unsetNames(unset []string) ([]protoreflect.Name, error) {
	var out []protoreflect.Name
	for _, u := range unset {
		u = strings.TrimSpace(u)
		if u == "all" {
			if len(unset) > 1 {
				return nil, usageErr("--unset all unsets every limit; name no other")
			}
			return slices.Clone(projectLimitNames), nil
		}
		i := slices.IndexFunc(projectLimitNames, func(n protoreflect.Name) bool { return limitFlag(n) == u })
		if i < 0 {
			return nil, usageErr("--unset %q is no limit a project sets; want all or one of %s", u, strings.Join(limitFlagNames(), ", "))
		}
		out = append(out, projectLimitNames[i])
	}
	return out, nil
}

func limitFlagNames() []string {
	out := make([]string, len(projectLimitNames))
	for i, n := range projectLimitNames {
		out[i] = limitFlag(n)
	}
	return out
}

// changeLimits sets and clears fields of l. A count must be a whole number;
// the apiserver judges everything else, such as a limit above the plan's.
func changeLimits(l *tenancyv1alpha1.Limits, set map[protoreflect.Name]string, clear []protoreflect.Name) error {
	r := l.ProtoReflect()
	for _, n := range clear {
		r.Clear(limitField(r, n))
	}
	for n, v := range set {
		fd := limitField(r, n)
		if fd.Kind() == protoreflect.StringKind {
			if strings.TrimSpace(v) == "" {
				return usageErr("--%s is empty; to unset it, use --unset %s", limitFlag(n), limitFlag(n))
			}
			r.Set(fd, protoreflect.ValueOfString(v))
			continue
		}
		i, err := strconv.ParseInt(v, 10, 32)
		if err != nil {
			return usageErr("--%s %q is not a whole number", limitFlag(n), v)
		}
		r.Set(fd, protoreflect.ValueOfInt32(int32(i)))
	}
	return nil
}

// ownLimitsLine is a project's own limits on one line.
func ownLimitsLine(l *tenancyv1alpha1.Limits) string {
	var parts []string
	for _, n := range projectLimitNames {
		if v := limitText(l, n); v != "" {
			parts = append(parts, limitFlag(n)+" "+v)
		}
	}
	if len(parts) == 0 {
		return "no limits of its own; its tenant's plan holds"
	}
	return "limits " + strings.Join(parts, ", ") + "; the rest follow its tenant's plan"
}
