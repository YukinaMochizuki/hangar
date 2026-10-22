package cli

import (
	"context"
	"errors"
	"fmt"
	"time"

	"connectrpc.com/connect"
	"github.com/spf13/cobra"
	"google.golang.org/protobuf/proto"

	metav1 "github.com/YukinaMochizuki/hangar/gen/go/hangar/meta/v1"
	networkv1alpha1 "github.com/YukinaMochizuki/hangar/gen/go/hangar/network/v1alpha1"
	"github.com/YukinaMochizuki/hangar/gen/go/hangar/network/v1alpha1/networkv1alpha1connect"
	"github.com/YukinaMochizuki/hangar/gen/go/hangar/registry"
	"github.com/YukinaMochizuki/hangar/internal/apimachinery/conditions"
)

// checkEvery is how often check --wait asks again.
const checkEvery = 10 * time.Second

// errNotVerified is a check that found the domain not verified.
var errNotVerified = errors.New("not verified")

// reasonHeldByAnotherTenant is the Verified reason of a domain whose name
// another tenant verified first, as the apiserver writes it.
const reasonHeldByAnotherTenant = "HeldByAnotherTenant"

// checkCmd is "check": a custom domain's challenge record looked for now.
func (a *app) checkCmd() *cobra.Command {
	var output string
	var wait bool
	var timeout time.Duration
	cmd := &cobra.Command{
		Use:   "check KIND NAME",
		Short: "Look for a custom domain's challenge record now",
		Long: `Look for a custom domain's challenge record now, rather than at the
apiserver's next check, which comes every minute. The domain's own
nameservers are asked, so a resolver that remembers the record missing,
for as long as the domain's negative TTL, does not hide one just added.
What is found is written into the domain's status, as the apiserver's own
checks write it. It takes what changing the domain takes: an owner of its
tenant.

It prints whether the domain is verified, and why not. It exits 0 once it
is, and 1 when it is not: an agent can retry, or pass --wait, which asks
again every 10 seconds until it is or the timeout passes (exit 5), and
stops at once with exit 1 when another tenant holds the name, which no
record of this tenant's changes.

Only custom domains (customdomain, cdom) are checked.`,
		Example: "  hangar check customdomain example-com\n  hangar check cdom example-com --wait --timeout=30m -o json",
		Args:    args(2, 2),
		RunE: func(cmd *cobra.Command, args []string) error {
			if output != "" && output != "name" && output != "json" {
				return usageErr("check prints name or json, not %q", output)
			}
			if !wait && cmd.Flags().Changed("timeout") {
				return usageErr("--timeout is how long --wait waits; pass --wait")
			}
			r, err := findKind(args[0])
			if err != nil {
				return err
			}
			if r.Kind != registry.CustomDomain.Kind {
				return usageErr("check works on custom domains, not %s", r.Plural)
			}
			if a.project != "" {
				return usageErr("%s are not in projects; drop --project", r.Plural)
			}
			checks, err := a.domainChecks()
			if err != nil {
				return err
			}
			ctx := cmd.Context()
			if wait {
				var cancel context.CancelFunc
				ctx, cancel = context.WithTimeout(ctx, timeout)
				defer cancel()
			}
			// last is what the last check found, printed when waiting ends
			// without the domain verified.
			var last *networkv1alpha1.CustomDomain
			giveUp := func() error {
				if last != nil {
					if err := a.printCheck(cmd, output, r, last); err != nil {
						return err
					}
				}
				if ctx.Err() == context.DeadlineExceeded {
					return &exitError{code: ExitTimeout, err: errors.New("timed out before the domain was verified")}
				}
				return errors.New("interrupted before the domain was verified")
			}
			for {
				res, err := checks.CheckCustomDomain(ctx, connect.NewRequest(&networkv1alpha1.CheckCustomDomainRequest{Name: args[1]}))
				switch {
				case err != nil && wait && ctx.Err() != nil:
					return giveUp()
				case err != nil && wait && connect.CodeOf(err) == connect.CodeResourceExhausted:
					// Checked a moment ago; the next round asks again.
				case err != nil:
					return err
				default:
					last = res.Msg.GetCustomDomain()
					verified := conditions.Find(last.GetStatus().GetConditions(), "Verified")
					done := verified.GetStatus() == metav1.ConditionStatus_CONDITION_STATUS_TRUE
					if done || !wait || verified.GetReason() == reasonHeldByAnotherTenant {
						if err := a.printCheck(cmd, output, r, last); err != nil {
							return err
						}
						if done {
							return nil
						}
						return &exitError{code: ExitError, err: errNotVerified}
					}
				}
				select {
				case <-ctx.Done():
					return giveUp()
				case <-time.After(checkEvery):
				}
			}
		},
	}
	cmd.Flags().StringVarP(&output, "output", "o", "", "name or json")
	cmd.Flags().BoolVar(&wait, "wait", false, "ask again every 10 seconds until the domain is verified")
	cmd.Flags().DurationVar(&timeout, "timeout", 30*time.Minute, "with --wait, give up after this long")
	return cmd
}

// printCheck prints what a check found.
func (a *app) printCheck(cmd *cobra.Command, output string, r registry.Resource, d *networkv1alpha1.CustomDomain) error {
	verified := conditions.Find(d.GetStatus().GetConditions(), "Verified")
	switch output {
	case "json":
		return a.printObjects(cmd.OutOrStdout(), "json", r, []proto.Message{d}, true, false)
	case "name":
		_, err := fmt.Fprintln(cmd.OutOrStdout(), objectName(r, d))
		return err
	}
	if verified.GetStatus() == metav1.ConditionStatus_CONDITION_STATUS_TRUE {
		_, err := fmt.Fprintf(cmd.OutOrStdout(), "%s verified: %s\n", objectName(r, d), verified.GetMessage())
		return err
	}
	_, err := fmt.Fprintf(cmd.OutOrStdout(), "%s not verified (%s): %s\n", objectName(r, d), verified.GetReason(), verified.GetMessage())
	return err
}

// domainChecks is CustomDomainCheckService for the context in effect.
func (a *app) domainChecks() (networkv1alpha1connect.CustomDomainCheckServiceClient, error) {
	c, err := a.current()
	if err != nil {
		return nil, err
	}
	creds, err := a.credentials(c)
	if err != nil {
		return nil, err
	}
	hc, base := a.httpClient(c)
	return networkv1alpha1connect.NewCustomDomainCheckServiceClient(hc, base, connect.WithInterceptors(creds)), nil
}
