package e2e

import (
	"context"
	"fmt"
	"testing"
	"time"

	"google.golang.org/protobuf/types/known/timestamppb"

	billingv1alpha1 "github.com/YukinaMochizuki/hangar/gen/go/hangar/billing/v1alpha1"
	metav1 "github.com/YukinaMochizuki/hangar/gen/go/hangar/meta/v1"
	"github.com/YukinaMochizuki/hangar/gen/go/hangar/registry"
	"github.com/YukinaMochizuki/hangar/internal/apiserver"
	"github.com/YukinaMochizuki/hangar/internal/client"
	"github.com/YukinaMochizuki/hangar/internal/testutil"
)

// TestARunningContainerIsBilled: a container its node runs is metered and
// rated: its billing account's open period comes to its vCPUs, memory and
// disk at the price list's prices.
func TestARunningContainerIsBilled(t *testing.T) {
	t.Parallel()
	w := newIdleWorldWith(t, func(c *apiserver.Config) { c.MeterInterval = 200 * time.Millisecond })
	w.startAgent()
	w.apply(container("api", "nginx"))
	w.waitReason("api", "Running")
	ctx := context.Background()
	now := time.Now().UTC().Truncate(time.Minute)
	list := func(extra int) *billingv1alpha1.PriceList {
		entries := []*billingv1alpha1.PriceEntry{{
			EffectiveTime: timestamppb.New(now.AddDate(0, -1, 0)),
			Prices:        &billingv1alpha1.Prices{Cpu: "0.6", Memory: "0.2", Disk: "0.005"},
		}}
		// An entry that takes effect later changes no price now, and has the
		// account rated again.
		if extra > 0 {
			entries = append(entries, &billingv1alpha1.PriceEntry{
				EffectiveTime: timestamppb.New(now.Add(time.Duration(24*60+extra) * time.Minute)),
				Prices:        &billingv1alpha1.Prices{Cpu: "0.6"},
			})
		}
		return &billingv1alpha1.PriceList{Metadata: &metav1.ObjectMeta{Name: "std"}, Spec: &billingv1alpha1.PriceListSpec{Currency: "TWD", Entries: entries}}
	}
	if _, err := w.root.Apply(ctx, registry.PriceList, list(0)); err != nil {
		t.Fatal(err)
	}
	if _, err := w.root.Apply(ctx, registry.BillingAccount, &billingv1alpha1.BillingAccount{
		Metadata: &metav1.ObjectMeta{Name: "acme"},
		Spec: &billingv1alpha1.BillingAccountSpec{
			Type: billingv1alpha1.BillingType_BILLING_TYPE_PAY_AS_YOU_GO, Currency: "TWD", PriceList: "std",
			PeriodMonths: 1, AnchorTime: timestamppb.New(now.AddDate(0, 0, -1)),
		},
	}); err != nil {
		t.Fatal(err)
	}
	attempt := 0
	testutil.Eventually(t, "the open period rated at the running container's vCPUs, memory and disk", func() (bool, string) {
		attempt++
		if _, err := w.root.Apply(ctx, registry.PriceList, list(attempt)); err != nil {
			return false, err.Error()
		}
		obj, err := w.root.Get(ctx, registry.BillingAccount, client.Key{Name: "acme"})
		if err != nil {
			return false, err.Error()
		}
		st := obj.(*billingv1alpha1.BillingAccount).GetStatus()
		held := map[string]bool{}
		for _, l := range st.GetUsageLines() {
			held[l.GetItem()] = l.GetAmount() != "0"
		}
		return held["cpu"] && held["memory"] && held["disk"] && st.GetAccrued() != "0", fmt.Sprint(st)
	})
}
