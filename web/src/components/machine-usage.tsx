// A machine's Usage: its CPU and memory over the last hour, as its node
// samples them (docs/decisions/machine-usage.md). A container's memory is
// what its guest uses; a virtual machine's is what it holds on the host.

import type { ReactNode } from "react";
import { SeriesPlot, SeriesValue, type ChartSeries } from "@/components/series-chart";
import { Card, CardHeader } from "@/components/ui";

/** What the card draws: the samples' series and which memory they are. */
export type Usage = {
  cpu: ChartSeries;
  memory: ChartSeries;
  /** "guest" for a container's guest's own count, "host" for what a VM holds on the host. */
  memoryKind: "guest" | "host";
};

/** The memory's name, by what it counts. */
export function memoryTitle(kind: Usage["memoryKind"]): string {
  return kind === "guest" ? "Memory used" : "Host memory";
}

function Frame({ children }: { children: ReactNode }) {
  return (
    <Card>
      <CardHeader title="Usage" action={<span className="text-[12px] text-subtle">last hour</span>} />
      <div className="divide-y">{children}</div>
    </Card>
  );
}

function Section({ title, value, children }: { title: string; value?: ReactNode; children: ReactNode }) {
  return (
    <section className="px-4 pb-3 pt-3">
      <div className="flex items-baseline justify-between text-[13px]">
        <span className="text-muted">{title}</span>
        <span className="whitespace-nowrap font-mono text-[12.5px] tabular">{value}</span>
      </div>
      {children}
    </section>
  );
}

/** The card with the hour's CPU and memory, each with its value now. */
export function UsageCard({ usage, now }: { usage: Usage; now?: Date }) {
  const memory = memoryTitle(usage.memoryKind);
  return (
    <Frame>
      <Section title="CPU" value={<SeriesValue series={usage.cpu} />}>
        <SeriesPlot title="CPU" series={usage.cpu} range="1h" now={now} axis="inside" />
      </Section>
      <Section title={memory} value={<SeriesValue series={usage.memory} />}>
        <SeriesPlot title={memory} series={usage.memory} range="1h" now={now} axis="inside" />
      </Section>
    </Frame>
  );
}

/** The card saying, in one line, why it has no samples to show, as for a machine not running. */
export function UsageNote({ children }: { children: ReactNode }) {
  return (
    <Frame>
      <div className="px-4 py-3 text-[13px] text-muted">{children}</div>
    </Frame>
  );
}

/** The card while its samples load or when they failed: as tall as the plots, so the column does not move. */
export function UsagePlaceholder({ memoryKind, children }: { memoryKind: Usage["memoryKind"]; children: ReactNode }) {
  return (
    <Frame>
      {["CPU", memoryTitle(memoryKind)].map((t) => (
        <Section key={t} title={t}>
          <div className="mt-2 flex h-[113px] items-center justify-center px-2 text-center text-[12px] text-subtle">{children}</div>
        </Section>
      ))}
    </Frame>
  );
}
