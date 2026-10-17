package cli

import (
	"cmp"
	"fmt"

	"connectrpc.com/connect"
	"github.com/spf13/cobra"

	iamv1alpha1 "github.com/YukinaMochizuki/hangar/gen/go/hangar/iam/v1alpha1"
	"github.com/YukinaMochizuki/hangar/gen/go/hangar/iam/v1alpha1/iamv1alpha1connect"
	"github.com/YukinaMochizuki/hangar/gen/go/hangar/registry"
	"github.com/YukinaMochizuki/hangar/gen/go/hangar/tenancy/v1alpha1/tenancyv1alpha1connect"
)

// accountClients are the clients of the services about the caller's own
// account, for the current context.
func (a *app) accountClients() (iamv1alpha1connect.AccountServiceClient, tenancyv1alpha1connect.NameServiceClient, error) {
	c, err := a.current()
	if err != nil {
		return nil, nil, err
	}
	creds, err := a.credentials(c)
	if err != nil {
		return nil, nil, err
	}
	hc, base := a.httpClient(c)
	return iamv1alpha1connect.NewAccountServiceClient(hc, base, connect.WithInterceptors(creds)),
		tenancyv1alpha1connect.NewNameServiceClient(hc, base, connect.WithInterceptors(creds)), nil
}

func (a *app) signupCmd() *cobra.Command {
	var username, displayName string
	cmd := &cobra.Command{
		Use:   "signup --username NAME [--display-name TEXT]",
		Short: "Sign up, once an administrator has invited you",
		Long: "Creates your User, which makes you a platform member once an administrator\n" +
			"has invited you by binding you a platform role. The username is yours for\n" +
			"good: global, never reused, and it cannot change. The display name is\n" +
			"what people read, and can change. Your email comes from your sign-in.\n" +
			"\n" +
			"Signing up also opens a tenant you own and a project in it, both named\n" +
			"after your username, such as alice-k3m9x, with a default network, and\n" +
			"makes that project the context's, so you can start right away.",
		Example: "  hangar signup --username alice --display-name \"Alice Liddell\"",
		Args:    cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			if username == "" {
				return usageErr("--username is required")
			}
			accounts, names, err := a.accountClients()
			if err != nil {
				return err
			}
			free, err := nameFree(cmd.Context(), names, registry.User, username)
			if err != nil {
				return err
			}
			if !free {
				return fmt.Errorf("username %q is taken, was used before or is reserved, and usernames are never reused; choose another", username)
			}
			res, err := accounts.SignUp(cmd.Context(), connect.NewRequest(&iamv1alpha1.SignUpRequest{Username: username, DisplayName: displayName}))
			if err != nil {
				return err
			}
			m := res.Msg
			w := cmd.OutOrStdout()
			_, _ = fmt.Fprintf(w, "signed up as %s\n", m.GetUser().GetMetadata().GetName())
			if m.GetProject() == "" {
				return nil
			}
			_, _ = fmt.Fprintf(w, "opened tenant %s with project %s\n", m.GetTenant(), m.GetProject())
			if err := a.useProject(m.GetProject()); err != nil {
				return fmt.Errorf("signed up, but setting project %s on the context failed: %w", m.GetProject(), err)
			}
			_, _ = fmt.Fprintf(w, "the context now uses project %s\n", m.GetProject())
			return nil
		},
	}
	cmd.Flags().StringVar(&username, "username", "", "the username, a DNS label such as alice")
	cmd.Flags().StringVar(&displayName, "display-name", "", "the name people read")
	return cmd
}

// useProject makes project the default of the context in effect.
func (a *app) useProject(project string) error {
	cfg, err := loadConfig(a.configPath())
	if err != nil {
		return err
	}
	name := cmp.Or(a.contextName, cfg.CurrentContext)
	c, ok := cfg.Contexts[name]
	if !ok {
		return usageErr("no context named %q", name)
	}
	c.Project = project
	if err := a.prepareConfigDir(); err != nil {
		return err
	}
	return cfg.save(a.configPath())
}

func (a *app) whoamiCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "whoami",
		Short: "Show who you are signed in as, and whether you have signed up",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			accounts, _, err := a.accountClients()
			if err != nil {
				return err
			}
			res, err := accounts.WhoAmI(cmd.Context(), connect.NewRequest(&iamv1alpha1.WhoAmIRequest{}))
			if err != nil {
				return err
			}
			m := res.Msg
			w := cmd.OutOrStdout()
			_, _ = fmt.Fprintf(w, "%s:%s\n", m.GetType(), m.GetId())
			if m.GetType() != "user" {
				return nil
			}
			switch u := m.GetUser(); {
			case u != nil && !m.GetInvited():
				_, _ = fmt.Fprintf(w, "username: %s\ndisplay name: %s\nnot a platform member: an administrator has to add you again\n",
					u.GetMetadata().GetName(), u.GetSpec().GetDisplayName())
			case u != nil:
				_, _ = fmt.Fprintf(w, "username: %s\ndisplay name: %s\n", u.GetMetadata().GetName(), u.GetSpec().GetDisplayName())
			case m.GetInvited():
				_, _ = fmt.Fprintln(w, "invited, not signed up yet: run hangar signup --username NAME")
			default:
				_, _ = fmt.Fprintln(w, "not a platform member yet: an administrator has to add you")
			}
			return nil
		},
	}
}
