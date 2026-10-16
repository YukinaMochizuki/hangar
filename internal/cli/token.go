package cli

import (
	"errors"
	"fmt"
	"io"
	"os"
	"strings"
	"text/tabwriter"
	"time"

	"connectrpc.com/connect"
	"github.com/spf13/cobra"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/durationpb"

	iamv1alpha1 "github.com/YukinaMochizuki/hangar/gen/go/hangar/iam/v1alpha1"
	"github.com/YukinaMochizuki/hangar/gen/go/hangar/iam/v1alpha1/iamv1alpha1connect"
	"github.com/YukinaMochizuki/hangar/gen/go/hangar/registry"
	"github.com/YukinaMochizuki/hangar/internal/auth/token"
	"github.com/YukinaMochizuki/hangar/internal/client"
)

// tokenCmd is "token": agents' service tokens.
func (a *app) tokenCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "token",
		Short: "Manage service tokens, the credentials agents use",
		Long: `Manage service tokens. A token is a project's, and authenticates an agent as
serviceToken:<project>/<name>, which RoleBindings name it by; it holds only
what they grant. Its secret is shown once, by create and rotate, and never
again: Hangar keeps only its hash. Only owners may create, rotate or delete
tokens.

An agent sends the secret in HANGAR_TOKEN, which replaces the context's
credentials, or from a file a context names:

  hangar config set-context agent --endpoint URL --auth token --token-file FILE`,
	}
	cmd.AddCommand(a.tokenCreateCmd(), a.tokenRotateCmd(), a.tokenDeleteCmd(), a.tokenGetCmd())
	return cmd
}

// issuer is ServiceTokenIssuerService for the context in effect, and the
// project tokens are in.
func (a *app) issuer() (iamv1alpha1connect.ServiceTokenIssuerServiceClient, string, error) {
	c, err := a.current()
	if err != nil {
		return nil, "", err
	}
	project, err := a.projectFor(registry.ServiceToken, c)
	if err != nil {
		return nil, "", err
	}
	creds, err := a.credentials(c)
	if err != nil {
		return nil, "", err
	}
	hc, base := a.httpClient(c)
	return iamv1alpha1connect.NewServiceTokenIssuerServiceClient(hc, base, connect.WithInterceptors(creds)), project, nil
}

// secretOut is where a new secret goes: a file made for it, or stdout.
type secretOut struct {
	path string
	f    *os.File
}

// openSecretOut makes the file the secret goes to, if there is one, before
// anything is issued, so a secret is never issued with nowhere to go. It
// refuses a file that exists.
func openSecretOut(path string) (*secretOut, error) {
	if path == "" {
		return &secretOut{}, nil
	}
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if errors.Is(err, os.ErrExist) {
		return nil, usageErr("%s exists; the secret goes only to a new file", path)
	}
	if err != nil {
		return nil, err
	}
	return &secretOut{path: path, f: f}, nil
}

// abandon removes the file when nothing was issued.
func (o *secretOut) abandon() {
	if o.f != nil {
		_ = o.f.Close()
		_ = os.Remove(o.path)
	}
}

// write puts the secret where it goes, and what was done on the other
// stream: with a file, stdout says it; without, stdout holds the secret
// alone, for a script to take, and stderr says the rest.
func (o *secretOut) write(cmd *cobra.Command, secret token.Secret, what string) error {
	if o.f == nil {
		if _, err := fmt.Fprintln(cmd.OutOrStdout(), secret.Reveal()); err != nil {
			return err
		}
		_, _ = fmt.Fprintf(cmd.ErrOrStderr(), "%s; the secret above is shown only now\n", what)
		return nil
	}
	_, err := fmt.Fprintln(o.f, secret.Reveal())
	if cerr := o.f.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		return fmt.Errorf("writing the secret to %s: %w; rotate the token to issue another", o.path, err)
	}
	_, _ = fmt.Fprintf(cmd.OutOrStdout(), "%s; the secret is in %s\n", what, o.path)
	return nil
}

func durationFlag(d time.Duration) *durationpb.Duration {
	if d == 0 {
		return nil
	}
	return durationpb.New(d)
}

func describeIssued(t *iamv1alpha1.ServiceToken) string {
	return fmt.Sprintf("service token %s/%s (%s…), expires %s", t.GetMetadata().GetProject(), t.GetMetadata().GetName(),
		t.GetStatus().GetSecretPrefix(), t.GetStatus().GetExpireTime().AsTime().UTC().Format(time.RFC3339))
}

func (a *app) tokenCreateCmd() *cobra.Command {
	var role, description, secretFile string
	var duration time.Duration
	cmd := &cobra.Command{
		Use:   "create NAME [--role ROLE] [--duration D] [--secret-file FILE]",
		Short: "Create a service token, and print its secret once",
		Long: `Create a service token in the project and print its secret, once, alone on
stdout, or write it to --secret-file, a new file of mode 0600, and print
nothing of it. --role binds the token that role in the project, as you
could by hand; bind it elsewhere with a RoleBinding whose subject is
serviceToken <project>/<name>. The secret lasts --duration: a year unless
set, at least 1h, at most three years.`,
		Example: "  hangar token create deployer --role project.editor --secret-file deployer.token\n" +
			"  HANGAR_TOKEN=$(hangar token create ci --role project.viewer) ./ci.sh",
		Args: args(1, 1),
		RunE: func(cmd *cobra.Command, args []string) error {
			issuer, project, err := a.issuer()
			if err != nil {
				return err
			}
			out, err := openSecretOut(secretFile)
			if err != nil {
				return err
			}
			res, err := issuer.CreateServiceToken(cmd.Context(), connect.NewRequest(&iamv1alpha1.CreateServiceTokenRequest{
				Project: project, Name: args[0], Description: description, Role: role, Duration: durationFlag(duration),
			}))
			if err != nil {
				out.abandon()
				return err
			}
			what := "created " + describeIssued(res.Msg.GetServiceToken())
			if role != "" {
				what += ", bound " + role + " in project " + project
			}
			return out.write(cmd, token.Secret(res.Msg.GetSecret()), what)
		},
	}
	cmd.Flags().StringVar(&role, "role", "", "role to bind the token in the project, such as project.editor")
	cmd.Flags().DurationVar(&duration, "duration", 0, "how long the secret lasts, such as 720h; a year unless set")
	cmd.Flags().StringVar(&description, "description", "", "what the token is for")
	cmd.Flags().StringVar(&secretFile, "secret-file", "", "new file, mode 0600, to write the secret to instead of stdout")
	return cmd
}

func (a *app) tokenRotateCmd() *cobra.Command {
	var secretFile string
	var duration time.Duration
	cmd := &cobra.Command{
		Use:   "rotate NAME [--duration D] [--secret-file FILE]",
		Short: "Issue a service token a new secret; the old one stops working at once",
		Long: `Issue the token a new secret, printed once as create prints it. The secret it
had stops working the moment the new one is issued. The new one lasts
--duration: a year unless set, at least 1h, at most three years.`,
		Args: args(1, 1),
		RunE: func(cmd *cobra.Command, args []string) error {
			issuer, project, err := a.issuer()
			if err != nil {
				return err
			}
			out, err := openSecretOut(secretFile)
			if err != nil {
				return err
			}
			res, err := issuer.RotateServiceToken(cmd.Context(), connect.NewRequest(&iamv1alpha1.RotateServiceTokenRequest{
				Project: project, Name: args[0], Duration: durationFlag(duration),
			}))
			if err != nil {
				out.abandon()
				return err
			}
			return out.write(cmd, token.Secret(res.Msg.GetSecret()), "rotated "+describeIssued(res.Msg.GetServiceToken()))
		},
	}
	cmd.Flags().DurationVar(&duration, "duration", 0, "how long the new secret lasts, such as 720h; a year unless set")
	cmd.Flags().StringVar(&secretFile, "secret-file", "", "new file, mode 0600, to write the secret to instead of stdout")
	return cmd
}

func (a *app) tokenDeleteCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "delete NAME",
		Short: "Delete a service token; its secret stops working at once",
		Long: `Delete the token. Its secret stops working at once, and the RoleBindings
naming it are deleted with it. The name is free again once they are gone.`,
		Args: args(1, 1),
		RunE: func(cmd *cobra.Command, args []string) error {
			cl, c, err := a.client()
			if err != nil {
				return err
			}
			project, err := a.projectFor(registry.ServiceToken, c)
			if err != nil {
				return err
			}
			if _, err := cl.Delete(cmd.Context(), registry.ServiceToken, client.Key{Project: project, Name: args[0]}); err != nil {
				return err
			}
			_, _ = fmt.Fprintf(cmd.OutOrStdout(), "service token %s/%s deleted; its secret no longer works\n", project, args[0])
			return nil
		},
	}
}

func (a *app) tokenGetCmd() *cobra.Command {
	var output string
	cmd := &cobra.Command{
		Use:   "get [NAME]",
		Short: "Show service tokens: never their secrets, which Hangar does not keep",
		Args:  args(0, 1),
		RunE: func(cmd *cobra.Command, args []string) error {
			if output == "" {
				output = "table"
			}
			if err := checkOutput(output); err != nil {
				return err
			}
			cl, c, err := a.client()
			if err != nil {
				return err
			}
			project, err := a.projectFor(registry.ServiceToken, c)
			if err != nil {
				return err
			}
			var objs []proto.Message
			if len(args) == 1 {
				o, err := cl.Get(cmd.Context(), registry.ServiceToken, client.Key{Project: project, Name: args[0]})
				if err != nil {
					return err
				}
				objs = []proto.Message{o}
			} else if objs, _, err = cl.ListAll(cmd.Context(), registry.ServiceToken, client.ListOptions{Project: project}); err != nil {
				return err
			}
			if output != "table" {
				return a.printObjects(cmd.OutOrStdout(), output, registry.ServiceToken, objs, len(args) == 1, false)
			}
			return a.printTokens(cmd.OutOrStdout(), objs)
		},
	}
	cmd.Flags().StringVarP(&output, "output", "o", "", "table, json, yaml or name")
	return cmd
}

// printTokens is the token table: which secret each has, until when, and
// when it was last used.
func (a *app) printTokens(w io.Writer, objs []proto.Message) error {
	now := a.opts.Now()
	tw := tabwriter.NewWriter(w, 0, 4, 2, ' ', 0)
	_, _ = fmt.Fprintln(tw, "NAME\tSECRET\tEXPIRES\tLAST USED\tAGE\tDESCRIPTION")
	for _, o := range objs {
		t, ok := o.(*iamv1alpha1.ServiceToken)
		if !ok {
			continue
		}
		st := t.GetStatus()
		secret, expires, used := "none", "-", "never"
		if st.GetSecretPrefix() != "" {
			secret = st.GetSecretPrefix() + "…"
			exp := st.GetExpireTime().AsTime()
			expires = exp.UTC().Format(time.DateOnly)
			if !now.Before(exp) {
				expires = "expired " + expires
			}
		}
		if st.GetLastUseTime() != nil {
			used = st.GetLastUseTime().AsTime().UTC().Format("2006-01-02 15:04")
		}
		desc := strings.ReplaceAll(t.GetSpec().GetDescription(), "\t", " ")
		_, _ = fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\t%s\n", t.GetMetadata().GetName(), secret, expires, used, age(now, t.GetMetadata()), desc)
	}
	return tw.Flush()
}
