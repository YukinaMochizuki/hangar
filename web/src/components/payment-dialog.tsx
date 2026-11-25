// Recording a statement paid, or taking that back, for platform
// administrators: Hangar collects nothing, and records what was paid
// outside it (docs/decisions/billing.md).

import { useMutation } from "@connectrpc/connect-query";
import { timestampFromDate } from "@bufbuild/protobuf/wkt";
import { StatementPaymentService } from "@hangar/api/hangar/billing/v1alpha1/statement_payment_pb";
import { useQueryClient } from "@tanstack/react-query";
import { Wallet } from "lucide-react";
import { useState } from "react";
import { Dialog } from "@/components/dialog";
import { Input } from "@/components/form";
import { Notice } from "@/components/notice";
import { Button } from "@/components/ui";
import { errorMessage } from "@/lib/api";
import { formatMoney, type Statement } from "@/lib/billing";

/** A moment as a datetime-local field holds it, in the browser's zone. */
function localValue(d: Date): string {
  const pad = (n: number) => String(n).padStart(2, "0");
  return `${d.getFullYear()}-${pad(d.getMonth() + 1)}-${pad(d.getDate())}T${pad(d.getHours())}:${pad(d.getMinutes())}`;
}

/** The statement's payment action: Record payment, or Record as not paid once paid. */
export function PaymentAction({ s }: { s: Statement }) {
  const [open, setOpen] = useState(false);
  const [at, setAt] = useState("");
  // When the dialog opened: a payment is recorded at or before it.
  const [opened, setOpened] = useState(0);
  const client = useQueryClient();
  const pay = useMutation(StatementPaymentService.method.markStatementPaid);
  const paid = !!s.status?.paidTime;
  const name = s.metadata?.name ?? "";
  const close = () => {
    pay.reset();
    setOpen(false);
  };
  const when = at ? new Date(at) : undefined;
  const valid = !!when && !Number.isNaN(when.getTime()) && when.getTime() <= opened + 60_000;
  const submit = () =>
    pay.mutate(paid ? { name, unpaid: true } : { name, paidTime: timestampFromDate(when!) }, {
      onSuccess: () => {
        void client.invalidateQueries({ predicate: (q) => /Statement/.test(JSON.stringify(q.queryKey)) });
        close();
      },
    });
  return (
    <>
      {paid ? (
        <Button variant="secondary" size="sm" onClick={() => setOpen(true)}>
          Record as not paid
        </Button>
      ) : (
        <Button
          variant="primary"
          size="sm"
          onClick={() => {
            const now = new Date();
            setAt(localValue(now));
            setOpened(now.getTime());
            setOpen(true);
          }}
        >
          <Wallet /> Record payment
        </Button>
      )}
      <Dialog open={open} onClose={close} title={paid ? "Record as not paid" : "Record payment"}>
        {paid ? (
          <p>
            <span className="font-mono">{name}</span> shows as due again, for {formatMoney(s.spec?.total, s.spec?.currency)}.
          </p>
        ) : (
          <>
            <p>
              {formatMoney(s.spec?.total, s.spec?.currency)} from <span className="font-mono">{s.spec?.tenant}</span>, paid outside Hangar.
            </p>
            <label className="mt-4 block text-[12px] font-medium text-foreground">
              Paid at
              <Input
                type="datetime-local"
                value={at}
                onChange={(e) => setAt(e.target.value)}
                className="mt-1.5"
                tone={valid ? "default" : "danger"}
              />
            </label>
            {!valid && <p className="mt-1.5 text-[12px] text-fail">A time up to now.</p>}
          </>
        )}
        {pay.error != null && (
          <Notice tone="error" title="Recording it failed" className="mt-3">
            {errorMessage(pay.error)}
          </Notice>
        )}
        <div className="mt-5 flex justify-end gap-2">
          <Button onClick={close}>Cancel</Button>
          <Button variant={paid ? "secondary" : "primary"} disabled={pay.isPending || (!paid && !valid)} onClick={submit}>
            {pay.isPending ? "Recording…" : paid ? "Record as not paid" : "Record payment"}
          </Button>
        </div>
      </Dialog>
    </>
  );
}
