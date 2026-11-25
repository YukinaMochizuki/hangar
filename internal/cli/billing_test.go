package cli_test

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/YukinaMochizuki/hangar/internal/apiserver"
	"github.com/YukinaMochizuki/hangar/internal/store"
	"github.com/YukinaMochizuki/hangar/internal/testutil"
)

// TestAStatementIsRecordedPaid: root applies a tenant's billing account
// from a manifest, its period that has ended is billed, and only root
// records the statement paid, and takes it back.
func TestAStatementIsRecordedPaid(t *testing.T) {
	w := newWorld(t, func(c *apiserver.Config) { c.Meter = c.Store.(store.Meter) })
	// alice owns the one tenant signing up opened her.
	w.as("alice@example.com")
	tenant := strings.TrimPrefix(strings.TrimSpace(w.must("", "get", "tenants", "-o", "name")), "tenant/")

	w.as("root@example.com")
	// Forty days back, whatever the date, is past the first period's end:
	// a month, which has at most 31 days.
	anchor := time.Now().UTC().Truncate(time.Minute).AddDate(0, 0, -40)
	w.must(fmt.Sprintf(`apiVersion: tenancy.hangarlab.cloud/v1alpha1
kind: Plan
metadata: {name: standard}
spec: {limits: {}, price: {monthlyFee: "300", currency: TWD}}
---
apiVersion: tenancy.hangarlab.cloud/v1alpha1
kind: Tenant
metadata: {name: %s}
spec: {plan: standard}
---
apiVersion: billing.hangarlab.cloud/v1alpha1
kind: BillingAccount
metadata: {name: %s}
spec: {type: BILLING_TYPE_FIXED, currency: TWD, anchorTime: %q}
`, tenant, tenant, anchor.Format(time.RFC3339)), "apply", "-f", "-")
	name := tenant + "-" + anchor.Format("200601021504")
	testutil.Eventually(t, "the first period billed", func() (bool, string) {
		r := w.run("", "get", "statement", name, "-o", "name")
		return r.code == 0, r.err
	})

	w.as("alice@example.com")
	var st struct {
		Spec struct {
			Total, Currency string
		}
	}
	if err := json.Unmarshal([]byte(w.must("", "get", "statement", name, "-o", "json")), &st); err != nil || st.Spec.Total != "300" {
		t.Errorf("the tenant's owner reading its statement: %+v, %v", st, err)
	}
	if r := w.run("", "admin", "pay", name); r.code == 0 || !strings.Contains(r.err, "billing.statements.pay") {
		t.Errorf("the tenant's owner recording it paid: exit %d: %s", r.code, r.err)
	}

	w.as("root@example.com")
	if out := w.must("", "admin", "pay", name, "--at", "2026-12-03T10:00:00+08:00"); out != "statement "+name+" (300 TWD) paid at 2026-12-03T02:00:00Z\n" {
		t.Errorf("pay printed %q", out)
	}
	if out := w.must("", "admin", "pay", name, "--undo"); out != "statement "+name+" (300 TWD) not paid\n" {
		t.Errorf("pay --undo printed %q", out)
	}
	if r := w.run("", "admin", "pay", name, "--undo", "--at", "2026-12-03T10:00:00Z"); r.code == 0 {
		t.Error("--undo with --at was taken")
	}
}
