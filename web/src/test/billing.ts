// A fake of the billing API: a pay-as-you-go account (acme), a fixed one on
// a plan (globex), their statements and a price list, and what the caller
// holds at the platform.

import { create } from "@bufbuild/protobuf";
import { timestampFromDate } from "@bufbuild/protobuf/wkt";
import { Code, ConnectError } from "@connectrpc/connect";
import { BillingAccountSchema, BillingAccountService, BillingType, type BillingAccount } from "@hangar/api/hangar/billing/v1alpha1/billing_account_pb";
import { PriceListSchema, PriceListService, type PriceList } from "@hangar/api/hangar/billing/v1alpha1/price_list_pb";
import { StatementEnd, StatementSchema, StatementService, type Statement } from "@hangar/api/hangar/billing/v1alpha1/statement_pb";
import { StatementPaymentService } from "@hangar/api/hangar/billing/v1alpha1/statement_payment_pb";
import { AccountService } from "@hangar/api/hangar/iam/v1alpha1/account_pb";
import { fakeApi } from "@/test/harness";

const at = (iso: string) => timestampFromDate(new Date(iso));

const line = (item: string, quantity: string, unit: string, unitPrice: string, amount: string, from = "2025-12-31T16:00:00Z") => ({
  item,
  quantity,
  unit,
  unitPrice,
  amount,
  priceEffectiveTime: at(from),
});

export function paygAccount(): BillingAccount {
  return create(BillingAccountSchema, {
    metadata: { name: "acme" },
    spec: { type: BillingType.PAY_AS_YOU_GO, currency: "TWD", priceList: "standard", periodMonths: 3, anchorTime: at("2026-02-28T16:00:00Z"), timeZone: "Asia/Taipei" },
    status: {
      periodType: BillingType.PAY_AS_YOU_GO,
      periodPriceList: "standard",
      periodMonths: 3,
      periodAnchorTime: at("2026-02-28T16:00:00Z"),
      periodStartTime: at("2026-08-31T16:00:00Z"),
      periodEndTime: at("2026-11-30T16:00:00Z"),
      accrued: "9025.84",
      metered: "9025.84",
      accruedTime: at("2026-11-25T08:00:00Z"),
      usageLines: [
        line("cpu", "8224", "vCPU-hour", "0.55", "4523.2", "2026-07-31T16:00:00Z"),
        line("memory", "16448", "GiB-hour", "0.18", "2960.64", "2026-07-31T16:00:00Z"),
        line("disk", "287840", "GiB-hour", "0.005", "1439.2", "2026-07-31T16:00:00Z"),
      ],
    },
  });
}

export function fixedAccount(): BillingAccount {
  return create(BillingAccountSchema, {
    metadata: { name: "globex" },
    spec: { type: BillingType.FIXED, currency: "TWD", priceList: "standard", anchorTime: at("2026-09-14T16:00:00Z"), timeZone: "Asia/Taipei" },
    status: {
      periodType: BillingType.FIXED,
      periodPlan: "standard",
      periodMonthlyFee: "300",
      periodPriceList: "standard",
      periodMonths: 1,
      periodAnchorTime: at("2026-09-14T16:00:00Z"),
      periodStartTime: at("2026-11-14T16:00:00Z"),
      periodEndTime: at("2026-12-14T16:00:00Z"),
      accrued: "300",
      metered: "258.56",
      accruedTime: at("2026-11-25T08:00:00Z"),
      usageLines: [line("cpu", "256", "vCPU-hour", "0.55", "140.8")],
    },
  });
}

export function statement(name: string, o: { tenant?: string; total?: string; paid?: boolean; fixed?: boolean } = {}): Statement {
  return create(StatementSchema, {
    metadata: { name, createTime: at("2026-09-01T00:00:00Z") },
    spec: {
      tenant: o.tenant ?? "acme",
      type: o.fixed ? BillingType.FIXED : BillingType.PAY_AS_YOU_GO,
      currency: "TWD",
      periodStartTime: at("2026-05-31T16:00:00Z"),
      periodEndTime: at("2026-08-31T16:00:00Z"),
      end: StatementEnd.PERIOD,
      total: o.total ?? "10220.16",
      metered: o.fixed ? "258.56" : "",
      lines: o.fixed
        ? [{ item: "monthly_fee", quantity: "1", unit: "month", unitPrice: "300", amount: "300" }]
        : [
            line("cpu", "5856", "vCPU-hour", "0.6", "3513.6"),
            line("cpu", "2976", "vCPU-hour", "0.55", "1636.8", "2026-07-31T16:00:00Z"),
            line("disk", "204960", "GiB-hour", "0.005", "1024.8"),
          ],
    },
    status: o.paid ? { paidTime: at("2026-09-04T02:30:00Z"), paidRecordedBy: "user:root@example.com" } : {},
  });
}

function priceList(): PriceList {
  return create(PriceListSchema, {
    metadata: { name: "standard" },
    spec: {
      displayName: "Standard",
      currency: "TWD",
      entries: [{ effectiveTime: at("2026-07-31T16:00:00Z"), prices: { cpu: "0.55", memory: "0.18", disk: "0.005" } }],
    },
  });
}

/**
 * An API serving billing: the accounts named (acme pay as you go, globex
 * fixed, unless said), their statements, the price list, and the caller
 * holding perms at the platform. A tenant with no account is NotFound, and
 * one denied is PermissionDenied.
 */
export function billingApi(
  f: { accounts?: BillingAccount[]; statements?: Statement[]; perms?: string[]; denied?: boolean } = {},
) {
  const accounts = f.accounts ?? [paygAccount(), fixedAccount()];
  const statements = f.statements ?? [statement("acme-202606010000"), statement("acme-202603010000", { paid: true, total: "10488" })];
  return fakeApi((r) => {
    r.service(AccountService, {
      testPermissions: (req) => ({ permissions: req.permissions.filter((p) => (f.perms ?? []).includes(p)) }),
    });
    r.service(BillingAccountService, {
      getBillingAccount: (req) => {
        if (f.denied) throw new ConnectError("may not get billingaccounts", Code.PermissionDenied);
        const a = accounts.find((x) => x.metadata?.name === req.name);
        if (!a) throw new ConnectError(`billingaccount ${req.name} not found`, Code.NotFound);
        return { billingAccount: a };
      },
      listBillingAccounts: () => ({ items: accounts }),
    });
    r.service(StatementService, {
      listStatements: () => ({ items: f.denied ? [] : statements }),
      getStatement: (req) => {
        const s = statements.find((x) => x.metadata?.name === req.name);
        if (!s) throw new ConnectError(`statement ${req.name} not found`, Code.NotFound);
        return { statement: s };
      },
    });
    r.service(PriceListService, {
      getPriceList: () => ({ priceList: priceList() }),
      listPriceLists: () => ({ items: [priceList()] }),
    });
    r.service(StatementPaymentService, {
      markStatementPaid: (req) => {
        const s = statements.find((x) => x.metadata?.name === req.name)!;
        s.status = req.unpaid ? { $typeName: "hangar.billing.v1alpha1.StatementStatus", observedGeneration: 0n, conditions: [], paidRecordedBy: "" } : { ...s.status!, paidTime: req.paidTime, paidRecordedBy: "user:root@example.com" };
        return { statement: s };
      },
    });
  });
}
