package cli

import (
	"fmt"
	"time"

	"connectrpc.com/connect"
	"github.com/spf13/cobra"
	"google.golang.org/protobuf/types/known/timestamppb"

	billingv1alpha1 "github.com/YukinaMochizuki/hangar/gen/go/hangar/billing/v1alpha1"
	"github.com/YukinaMochizuki/hangar/gen/go/hangar/billing/v1alpha1/billingv1alpha1connect"
)

func (a *app) payCmd() *cobra.Command {
	var at string
	var undo bool
	cmd := &cobra.Command{
		Use:   "pay STATEMENT",
		Short: "Record a statement paid, or not (platform.root)",
		Long: `Records that a Statement was paid, which Hangar does not collect itself:
the tenant pays outside Hangar, and this records it, with the time it was
paid (--at, now if left out) and who recorded it. --undo takes it back.
Recording a statement already paid again, with no --at, changes nothing.

hangar get statements lists them; see docs/decisions/billing.md.`,
		Example: "  hangar admin pay acme-202611010000\n  hangar admin pay acme-202611010000 --at 2026-12-03T10:00:00+08:00\n  hangar admin pay acme-202611010000 --undo",
		Args:    args(1, 1),
		RunE: func(cmd *cobra.Command, args []string) error {
			req := &billingv1alpha1.MarkStatementPaidRequest{Name: args[0], Unpaid: undo}
			if at != "" {
				if undo {
					return usageErr("--at and --undo do not go together")
				}
				t, err := time.Parse(time.RFC3339, at)
				if err != nil {
					return usageErr("--at %q is not a time such as 2026-12-03T10:00:00+08:00", at)
				}
				req.PaidTime = timestamppb.New(t)
			}
			c, err := a.current()
			if err != nil {
				return err
			}
			creds, err := a.credentials(c)
			if err != nil {
				return err
			}
			hc, base := a.httpClient(c)
			pay := billingv1alpha1connect.NewStatementPaymentServiceClient(hc, base, connect.WithInterceptors(creds))
			res, err := pay.MarkStatementPaid(cmd.Context(), connect.NewRequest(req))
			if err != nil {
				return err
			}
			st := res.Msg.GetStatement()
			w := cmd.OutOrStdout()
			if paid := st.GetStatus().GetPaidTime(); paid != nil {
				_, err = fmt.Fprintf(w, "statement %s (%s %s) paid at %s\n", st.GetMetadata().GetName(),
					st.GetSpec().GetTotal(), st.GetSpec().GetCurrency(), paid.AsTime().Format(time.RFC3339))
			} else {
				_, err = fmt.Fprintf(w, "statement %s (%s %s) not paid\n", st.GetMetadata().GetName(),
					st.GetSpec().GetTotal(), st.GetSpec().GetCurrency())
			}
			return err
		},
	}
	cmd.Flags().StringVar(&at, "at", "", "when it was paid, in RFC 3339; now if left out")
	cmd.Flags().BoolVar(&undo, "undo", false, "record it not paid")
	return cmd
}
