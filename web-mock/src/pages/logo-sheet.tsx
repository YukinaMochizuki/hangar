import { HangarMark } from "@/components/marks";

function Panel({ theme }: { theme: "light" | "dark" }) {
  return (
    <div data-theme={theme} data-accent="orange" className="bg-background p-8 text-foreground">
      <div className="mb-6 text-[12px] font-medium uppercase tracking-wider text-subtle">{theme}</div>
      <div className="flex items-end gap-8 rounded-lg border bg-surface p-6">
        <HangarMark className="size-24" />
        <HangarMark className="size-12" />
        <HangarMark className="size-6" />
        <HangarMark className="size-4" />
        <span className="ml-6 flex items-center gap-2">
          <HangarMark className="size-5" />
          <span className="text-[15px] font-semibold tracking-[-0.02em]">Hangar</span>
        </span>
      </div>
    </div>
  );
}

/** The mark at the sizes it is used at, in both themes. Proof of concept only. */
export function LogoSheet() {
  return (
    <div>
      <Panel theme="dark" />
      <Panel theme="light" />
    </div>
  );
}
