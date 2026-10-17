package cli

import (
	"fmt"
	"io"
	"text/tabwriter"

	"connectrpc.com/connect"
	"github.com/spf13/cobra"
	"google.golang.org/protobuf/proto"

	iamv1alpha1 "github.com/YukinaMochizuki/hangar/gen/go/hangar/iam/v1alpha1"
	"github.com/YukinaMochizuki/hangar/gen/go/hangar/iam/v1alpha1/iamv1alpha1connect"
	tenancyv1alpha1 "github.com/YukinaMochizuki/hangar/gen/go/hangar/tenancy/v1alpha1"
	"github.com/YukinaMochizuki/hangar/gen/go/hangar/tenancy/v1alpha1/tenancyv1alpha1connect"
)

// memberCmd is "member": who holds which role in a tenant or project.
func (a *app) memberCmd() *cobra.Command {
	var tenant string
	cmd := &cobra.Command{
		Use:   "member",
		Short: "Manage who holds which role in a tenant or project",
		Long: `Manage the members of a tenant (--tenant) or a project (--project, or the
context's). A member is a person's email bound a role there, a RoleBinding:
tenant.owner or tenant.viewer in a tenant, project.owner, project.editor or
project.viewer in a project. Owners manage members; no one grants a role
they could not grant by applying the binding.

Someone who is not a platform member yet can be added: their role takes
effect once an administrator invites them and they sign up.`,
		Example: `  hangar member list --tenant acme
  hangar member add carol@example.com --role project.editor
  hangar member set-role carol@example.com --role project.viewer
  hangar member remove carol@example.com`,
	}
	cmd.PersistentFlags().StringVar(&tenant, "tenant", "", "a tenant, instead of a project")
	cmd.AddCommand(a.memberListCmd(&tenant), a.memberAddCmd(&tenant), a.memberSetRoleCmd(&tenant), a.memberRemoveCmd(&tenant))
	return cmd
}

// members is MemberService for the context in effect, and the scope the
// command names: --tenant, or else the project in effect.
func (a *app) members(tenant string) (iamv1alpha1connect.MemberServiceClient, *iamv1alpha1.BindingScope, error) {
	c, err := a.current()
	if err != nil {
		return nil, nil, err
	}
	var scope *iamv1alpha1.BindingScope
	switch project := a.project; {
	case tenant != "" && project != "":
		return nil, nil, usageErr("pass --tenant or --project, not both")
	case tenant != "":
		scope = &iamv1alpha1.BindingScope{Target: &iamv1alpha1.BindingScope_Tenant{Tenant: tenant}}
	default:
		if project == "" {
			project = c.Project
		}
		if project == "" {
			return nil, nil, usageErr("pass --tenant or --project, or set a project on the context")
		}
		scope = &iamv1alpha1.BindingScope{Target: &iamv1alpha1.BindingScope_Project{Project: project}}
	}
	creds, err := a.credentials(c)
	if err != nil {
		return nil, nil, err
	}
	hc, base := a.httpClient(c)
	return iamv1alpha1connect.NewMemberServiceClient(hc, base, connect.WithInterceptors(creds)), scope, nil
}

func scopeString(s *iamv1alpha1.BindingScope) string {
	switch t := s.GetTarget().(type) {
	case *iamv1alpha1.BindingScope_Tenant:
		return "tenant/" + t.Tenant
	case *iamv1alpha1.BindingScope_Project:
		return "project/" + t.Project
	case *iamv1alpha1.BindingScope_Platform:
		return "platform"
	}
	return ""
}

// printMemberJSON writes m as one JSON document.
func printMemberJSON(w io.Writer, m proto.Message) error {
	js, err := marshalJSON(m)
	if err != nil {
		return err
	}
	_, err = fmt.Fprintf(w, "%s\n", js)
	return err
}

func (a *app) memberListCmd(tenant *string) *cobra.Command {
	var output string
	cmd := &cobra.Command{
		Use:   "list",
		Short: "List the members of a tenant or project",
		Long: "Lists the people bound a role at exactly the tenant or project: a project's\n" +
			"list leaves out its tenant's members, who also reach it.",
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			if output != "table" && output != "json" {
				return usageErr("unknown output %q; want table or json", output)
			}
			ms, scope, err := a.members(*tenant)
			if err != nil {
				return err
			}
			res, err := ms.ListMembers(cmd.Context(), connect.NewRequest(&iamv1alpha1.ListMembersRequest{Scope: scope}))
			if err != nil {
				return err
			}
			if output == "json" {
				return printMemberJSON(cmd.OutOrStdout(), res.Msg)
			}
			tw := tabwriter.NewWriter(cmd.OutOrStdout(), 0, 4, 3, ' ', 0)
			_, _ = fmt.Fprintln(tw, "EMAIL\tUSERNAME\tROLE\tSCOPE\tPLATFORM MEMBER")
			for _, m := range res.Msg.GetMembers() {
				_, _ = fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\n", m.GetEmail(), orDash(m.GetUsername()), m.GetRole(), scopeString(m.GetScope()), yesNo(m.GetPlatformMember()))
			}
			return tw.Flush()
		},
	}
	cmd.Flags().StringVarP(&output, "output", "o", "table", "table or json")
	return cmd
}

func orDash(s string) string {
	if s == "" {
		return "-"
	}
	return s
}

func yesNo(b bool) string {
	if b {
		return "yes"
	}
	return "no"
}

// reportMember tells what a write made of a member, with its warnings, such
// as that the person is not a platform member yet.
func reportMember(cmd *cobra.Command, verb string, m *iamv1alpha1.Member, warnings []string) {
	_, _ = fmt.Fprintf(cmd.OutOrStdout(), "%s %s as %s in %s\n", verb, m.GetEmail(), m.GetRole(), scopeString(m.GetScope()))
	for _, w := range warnings {
		_, _ = fmt.Fprintf(cmd.ErrOrStderr(), "warning: %s\n", w)
	}
}

func (a *app) memberAddCmd(tenant *string) *cobra.Command {
	var role string
	cmd := &cobra.Command{
		Use:   "add EMAIL --role ROLE",
		Short: "Bind a person a role in a tenant or project",
		Args:  args(1, 1),
		RunE: func(cmd *cobra.Command, args []string) error {
			if role == "" {
				return usageErr("--role is required")
			}
			ms, scope, err := a.members(*tenant)
			if err != nil {
				return err
			}
			res, err := ms.AddMember(cmd.Context(), connect.NewRequest(&iamv1alpha1.AddMemberRequest{Scope: scope, Email: args[0], Role: role}))
			if err != nil {
				return err
			}
			reportMember(cmd, "added", res.Msg.GetMember(), res.Msg.GetWarnings())
			return nil
		},
	}
	cmd.Flags().StringVar(&role, "role", "", "the role, such as project.editor or tenant.viewer")
	return cmd
}

func (a *app) memberSetRoleCmd(tenant *string) *cobra.Command {
	var role string
	cmd := &cobra.Command{
		Use:   "set-role EMAIL --role ROLE",
		Short: "Change the role a member holds in a tenant or project",
		Args:  args(1, 1),
		RunE: func(cmd *cobra.Command, args []string) error {
			if role == "" {
				return usageErr("--role is required")
			}
			ms, scope, err := a.members(*tenant)
			if err != nil {
				return err
			}
			res, err := ms.SetMemberRole(cmd.Context(), connect.NewRequest(&iamv1alpha1.SetMemberRoleRequest{Scope: scope, Email: args[0], Role: role}))
			if err != nil {
				return err
			}
			reportMember(cmd, "set", res.Msg.GetMember(), res.Msg.GetWarnings())
			return nil
		},
	}
	cmd.Flags().StringVar(&role, "role", "", "the new role")
	return cmd
}

func (a *app) memberRemoveCmd(tenant *string) *cobra.Command {
	return &cobra.Command{
		Use:   "remove EMAIL",
		Short: "Remove a member's roles from a tenant or project",
		Args:  args(1, 1),
		RunE: func(cmd *cobra.Command, args []string) error {
			ms, scope, err := a.members(*tenant)
			if err != nil {
				return err
			}
			res, err := ms.RemoveMember(cmd.Context(), connect.NewRequest(&iamv1alpha1.RemoveMemberRequest{Scope: scope, Email: args[0]}))
			if err != nil {
				return err
			}
			for _, m := range res.Msg.GetRemoved() {
				_, _ = fmt.Fprintf(cmd.OutOrStdout(), "removed %s as %s from %s\n", m.GetEmail(), m.GetRole(), scopeString(m.GetScope()))
			}
			return nil
		},
	}
}

// projectCmd is "project": the projects the caller reaches, and which one
// the context uses.
func (a *app) projectCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "project",
		Short: "List the projects you reach, and switch the context's project",
	}
	cmd.AddCommand(a.projectListCmd(), a.projectUseCmd())
	return cmd
}

// projects is ProjectService for the context in effect.
func (a *app) projects(c *Context) (tenancyv1alpha1connect.ProjectServiceClient, error) {
	creds, err := a.credentials(c)
	if err != nil {
		return nil, err
	}
	hc, base := a.httpClient(c)
	return tenancyv1alpha1connect.NewProjectServiceClient(hc, base, connect.WithInterceptors(creds)), nil
}

func (a *app) projectListCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "list",
		Short: "List the projects you hold a role in, marking the context's",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			c, err := a.current()
			if err != nil {
				return err
			}
			ps, err := a.projects(c)
			if err != nil {
				return err
			}
			tw := tabwriter.NewWriter(cmd.OutOrStdout(), 0, 4, 3, ' ', 0)
			_, _ = fmt.Fprintln(tw, "CURRENT\tNAME\tTENANT\tDISPLAY NAME")
			req := &tenancyv1alpha1.ListProjectsRequest{}
			for {
				res, err := ps.ListProjects(cmd.Context(), connect.NewRequest(req))
				if err != nil {
					return err
				}
				for _, p := range res.Msg.GetItems() {
					current := ""
					if p.GetMetadata().GetName() == c.Project {
						current = "*"
					}
					_, _ = fmt.Fprintf(tw, "%s\t%s\t%s\t%s\n", current, p.GetMetadata().GetName(), p.GetSpec().GetTenant(), p.GetSpec().GetDisplayName())
				}
				if res.Msg.GetNextPageToken() == "" {
					break
				}
				req.PageToken = res.Msg.GetNextPageToken()
			}
			return tw.Flush()
		},
	}
}

func (a *app) projectUseCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "use NAME",
		Short: "Make a project the context's default",
		Long:  "Checks that you reach the project, then makes it the context's project.",
		Args:  args(1, 1),
		RunE: func(cmd *cobra.Command, args []string) error {
			cfg, err := loadConfig(a.configPath())
			if err != nil {
				return err
			}
			name := a.contextName
			if name == "" {
				name = cfg.CurrentContext
			}
			if name == "" {
				return usageErr("no context; create one with `hangar config set-context`")
			}
			c, ok := cfg.Contexts[name]
			if !ok {
				return usageErr("no context named %q", name)
			}
			ps, err := a.projects(c)
			if err != nil {
				return err
			}
			if _, err := ps.GetProject(cmd.Context(), connect.NewRequest(&tenancyv1alpha1.GetProjectRequest{Name: args[0]})); err != nil {
				return err
			}
			c.Project = args[0]
			if err := a.prepareConfigDir(); err != nil {
				return err
			}
			if err := cfg.save(a.configPath()); err != nil {
				return err
			}
			_, _ = fmt.Fprintf(cmd.OutOrStdout(), "context %q now uses project %q\n", name, args[0])
			return nil
		},
	}
}
