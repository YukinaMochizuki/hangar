package cli

import (
	"context"
	"crypto/rand"
	"fmt"
	"math/big"
	"strings"
	"unicode"

	"connectrpc.com/connect"
	"github.com/spf13/cobra"
	"golang.org/x/text/unicode/norm"
	"google.golang.org/protobuf/proto"

	metav1 "github.com/YukinaMochizuki/hangar/gen/go/hangar/meta/v1"
	"github.com/YukinaMochizuki/hangar/gen/go/hangar/registry"
	tenancyv1alpha1 "github.com/YukinaMochizuki/hangar/gen/go/hangar/tenancy/v1alpha1"
	"github.com/YukinaMochizuki/hangar/gen/go/hangar/tenancy/v1alpha1/tenancyv1alpha1connect"
)

const (
	// maxSlug bounds the part of a suggested name taken from the display
	// name, so that with its suffix the name stays at most 27 characters and
	// leaves room in a hostname label for what is put before it.
	maxSlug = 20
	// suggestAttempts bounds how many suggested names are tried before giving
	// up; with six random digits each, a second is rarely needed.
	suggestAttempts = 5
)

func (a *app) createCmd() *cobra.Command {
	var displayName, tenant, output string
	cmd := &cobra.Command{
		Use:   "create KIND [NAME] --display-name TEXT",
		Short: "Create a tenant or a project, suggesting its name from the display name, or a secret",
		Long: "Creates a tenant, or a project in --tenant. Their names are global and\n" +
			"never reused, so without NAME one is made from the display name and six\n" +
			"random digits, such as my-demo-438212, and checked to be free first. The\n" +
			"display name is what people read, and can change; the name cannot.\n" +
			"A NAME that is taken, or was ever used, is refused before anything is sent.\n" +
			"\n" +
			"\"create secret\" creates or replaces a secret from a file instead.",
		Example: "  hangar create tenant --display-name \"Alice's lab\"\n" +
			"  hangar create project --tenant alices-lab-204816 --display-name \"My demo\"\n" +
			"  hangar create project my-demo --tenant alices-lab-204816 --display-name \"My demo\"",
		Args: args(1, 2),
		RunE: func(cmd *cobra.Command, args []string) error {
			if output != "" && output != "name" {
				return usageErr("create prints name, not %q", output)
			}
			r, err := findKind(args[0])
			if err != nil {
				return err
			}
			switch {
			case r.Kind != registry.Tenant.Kind && r.Kind != registry.Project.Kind:
				return usageErr("create makes tenants and projects; use apply for %s", r.Plural)
			case r.Kind == registry.Project.Kind && tenant == "":
				return usageErr("a project needs --tenant")
			case r.Kind == registry.Tenant.Kind && tenant != "":
				return usageErr("--tenant is for projects")
			case len(args) == 1 && strings.TrimSpace(displayName) == "":
				return usageErr("without NAME, --display-name is required to suggest one from")
			}
			cl, c, err := a.client()
			if err != nil {
				return err
			}
			creds, err := a.credentials(c)
			if err != nil {
				return err
			}
			hc, base := a.httpClient(c)
			names := tenancyv1alpha1connect.NewNameServiceClient(hc, base, connect.WithInterceptors(creds))
			name := ""
			if len(args) == 2 {
				name = args[1]
				free, err := nameFree(cmd.Context(), names, r, name)
				if err != nil {
					return err
				}
				if !free {
					return fmt.Errorf("%s name %q is taken or was used before, and names are never reused; leave NAME out to have one suggested",
						strings.ToLower(r.Kind), name)
				}
			} else if name, err = suggestName(cmd.Context(), names, r, displayName); err != nil {
				return err
			}
			res, err := cl.Create(cmd.Context(), r, newTenancyObject(r, name, displayName, tenant))
			if connect.CodeOf(err) == connect.CodeAlreadyExists {
				return &exitError{code: ExitConflict, err: fmt.Errorf("%s %s was taken after its name was checked", strings.ToLower(r.Kind), name)}
			}
			if err != nil {
				return err
			}
			if output == "name" {
				_, _ = fmt.Fprintln(cmd.OutOrStdout(), objectName(r, res.Object))
				return nil
			}
			_, _ = fmt.Fprintf(cmd.OutOrStdout(), "%s created\n", objectName(r, res.Object))
			return nil
		},
	}
	cmd.Flags().StringVar(&displayName, "display-name", "", "the name people read")
	cmd.Flags().StringVar(&tenant, "tenant", "", "the tenant a project belongs to")
	cmd.Flags().StringVarP(&output, "output", "o", "", "name")
	cmd.AddCommand(a.createSecretCmd())
	return cmd
}

func newTenancyObject(r registry.Resource, name, displayName, tenant string) proto.Message {
	meta := &metav1.ObjectMeta{Name: name}
	if r.Kind == registry.Tenant.Kind {
		return &tenancyv1alpha1.Tenant{ApiVersion: r.APIVersion(), Kind: r.Kind, Metadata: meta,
			Spec: &tenancyv1alpha1.TenantSpec{DisplayName: displayName}}
	}
	return &tenancyv1alpha1.Project{ApiVersion: r.APIVersion(), Kind: r.Kind, Metadata: meta,
		Spec: &tenancyv1alpha1.ProjectSpec{Tenant: tenant, DisplayName: displayName}}
}

// suggestName finds a free name made from displayName: its slug and six
// random digits. Someone can still take it before it is created, which
// create then reports.
func suggestName(ctx context.Context, names tenancyv1alpha1connect.NameServiceClient, r registry.Resource, displayName string) (string, error) {
	base := slug(displayName)
	if base == "" {
		base = strings.ToLower(r.Kind)
	}
	for range suggestAttempts {
		n, err := rand.Int(rand.Reader, big.NewInt(1_000_000))
		if err != nil {
			return "", err
		}
		name := fmt.Sprintf("%s-%06d", base, n.Int64())
		free, err := nameFree(ctx, names, r, name)
		if err != nil {
			return "", err
		}
		if free {
			return name, nil
		}
	}
	return "", fmt.Errorf("no free %s name found from %q after %d tries; pass a NAME", strings.ToLower(r.Kind), displayName, suggestAttempts)
}

func nameFree(ctx context.Context, names tenancyv1alpha1connect.NameServiceClient, r registry.Resource, name string) (bool, error) {
	res, err := names.CheckName(ctx, connect.NewRequest(&tenancyv1alpha1.CheckNameRequest{Kind: r.Kind, Name: name}))
	if err != nil {
		return false, err
	}
	return res.Msg.GetAvailable(), nil
}

// slug turns a display name into the start of an object name: lower case
// ASCII letters and digits, accents dropped (é is e), with runs of anything
// else made one '-', at most maxSlug characters.
func slug(s string) string {
	var b strings.Builder
	dash := false
	for _, c := range norm.NFKD.String(strings.ToLower(s)) {
		if unicode.Is(unicode.Mn, c) {
			continue // an accent, decomposed from its letter
		}
		if b.Len() >= maxSlug {
			break
		}
		if c >= 'a' && c <= 'z' || c >= '0' && c <= '9' {
			if dash && b.Len() > 0 {
				b.WriteByte('-')
			}
			dash = false
			b.WriteRune(c)
		} else {
			dash = true
		}
	}
	out := b.String()
	if len(out) > maxSlug {
		out = out[:maxSlug]
	}
	return strings.TrimRight(out, "-")
}
