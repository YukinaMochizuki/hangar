package cli

import (
	"fmt"
	"io"
	"os"

	"github.com/spf13/cobra"

	metav1 "github.com/YukinaMochizuki/hangar/gen/go/hangar/meta/v1"
	"github.com/YukinaMochizuki/hangar/gen/go/hangar/registry"
	secretsv1alpha1 "github.com/YukinaMochizuki/hangar/gen/go/hangar/secrets/v1alpha1"
	"github.com/YukinaMochizuki/hangar/internal/secrets"
)

// createSecretCmd is "create secret", under create.
func (a *app) createSecretCmd() *cobra.Command {
	secret := &cobra.Command{Use: "secret", Short: "Create or replace a secret; its values are read from a file, never from the command line"}
	secret.AddCommand(a.createRegistrySecretCmd())
	return secret
}

func (a *app) createRegistrySecretCmd() *cobra.Command {
	var from string
	cmd := &cobra.Command{
		Use:   "registry NAME --from-docker-config FILE|-",
		Short: "Create or replace a registry-auth secret from a Docker config",
		Long: `Create or replace a registry-auth secret, holding registry logins in the
Docker config format, as docker login writes it (~/.docker/config.json, or
"-" for stdin). Its values are write-only: nothing shows them again.
A config whose logins live in a credential helper ("credsStore") holds none;
write one with them instead.`,
		Args: args(1, 1),
		RunE: func(cmd *cobra.Command, args []string) error {
			if from == "" {
				return usageErr("--from-docker-config is required")
			}
			data, err := readFileOrStdin(from, cmd.InOrStdin())
			if err != nil {
				return err
			}
			sec := &secretsv1alpha1.Secret{
				Metadata: &metav1.ObjectMeta{Name: args[0]},
				Spec: &secretsv1alpha1.SecretSpec{
					Type: secretsv1alpha1.SecretType_SECRET_TYPE_REGISTRY_AUTH,
					Data: map[string][]byte{secrets.DockerConfigKey: data},
				},
			}
			// Checked here too, so a bad file never leaves the machine.
			if err := secrets.Validate(sec.Spec.Type, sec.Spec.Data); err != nil {
				return usageErr("%s: %v", from, err)
			}
			cl, c, err := a.client()
			if err != nil {
				return err
			}
			if sec.Metadata.Project, err = a.projectFor(registry.Secret, c); err != nil {
				return err
			}
			res, err := cl.Apply(cmd.Context(), registry.Secret, sec)
			if err != nil {
				return err
			}
			verb := "applied"
			if res.Created {
				verb = "created"
			}
			_, _ = fmt.Fprintf(cmd.OutOrStdout(), "%s %s, for %d registries\n", objectName(registry.Secret, res.Object), verb, len(secrets.Registries(sec.Spec.Data)))
			return nil
		},
	}
	cmd.Flags().StringVar(&from, "from-docker-config", "", "Docker config file, or - for stdin")
	return cmd
}

// readFileOrStdin reads a file, or stdin for "-", up to a bound past what
// a secret may hold.
func readFileOrStdin(path string, stdin io.Reader) ([]byte, error) {
	r := stdin
	if path != "-" {
		f, err := os.Open(path)
		if err != nil {
			return nil, err
		}
		defer func() { _ = f.Close() }()
		r = f
	}
	b, err := io.ReadAll(io.LimitReader(r, secrets.MaxDataSize+1))
	if err != nil {
		return nil, err
	}
	if len(b) > secrets.MaxDataSize {
		return nil, usageErr("%s is larger than a secret may hold (%d bytes)", path, secrets.MaxDataSize)
	}
	return b, nil
}
