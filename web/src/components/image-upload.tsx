// The pieces of uploading an image file: the drop zone the New image page
// takes a file from, and the card an uploaded image's page shows until its
// file is whole, with the upload this tab runs (lib/upload.ts) or, when none
// runs here, a way to choose the file again and go on from the node's count.

import type { CustomImage } from "@hangar/api/hangar/storage/v1alpha1/custom_image_pb";
import { FileUp, Pause, Play, RotateCw, Upload } from "lucide-react";
import { useEffect, useRef, useState, type ReactNode } from "react";
import { Notice } from "@/components/notice";
import { Button, Card, CardHeader } from "@/components/ui";
import { cn } from "@/lib/cn";
import { customImageState, uploadSource } from "@/lib/custom-images";
import { pauseUpload, resumeUpload, sniffFormat, startUpload, useUpload, type ImageFormat, type UploadState } from "@/lib/upload";

/** bytes writes a byte count for people: "3.4 GiB". */
export function bytes(n: number): string {
  const units = ["B", "KiB", "MiB", "GiB", "TiB"];
  let i = 0;
  while (n >= 1024 && i < units.length - 1) {
    n /= 1024;
    i++;
  }
  const shown = i === 0 || n >= 100 ? Math.round(n).toString() : n.toFixed(1).replace(/\.0$/, "");
  return `${shown} ${units[i]}`;
}

/** duration writes a number of seconds for people: "4 min", "1 h 12 min". */
export function duration(s: number): string {
  if (!Number.isFinite(s) || s <= 0) return "–";
  if (s < 60) return `${Math.ceil(s)} s`;
  const m = Math.round(s / 60);
  if (m < 60) return `${m} min`;
  return m % 60 === 0 ? `${m / 60} h` : `${Math.floor(m / 60)} h ${m % 60} min`;
}

const formatText: Record<ImageFormat, string> = {
  qcow2: "qcow2",
  raw: "raw",
  unknown: "not raw or qcow2",
};

/** ChosenFile is a file the drop zone took, with what its first bytes say it is. */
export type ChosenFile = { file: File; format: ImageFormat };

/** FileDrop takes one image file, by dropping it or choosing it. */
export function FileDrop({ value, onChange, maxBytes }: { value?: ChosenFile; onChange: (f: ChosenFile | undefined) => void; maxBytes: number }) {
  const input = useRef<HTMLInputElement>(null);
  const [over, setOver] = useState(false);
  const take = async (file: File | undefined) => {
    if (!file) return;
    onChange({ file, format: await sniffFormat(file) });
  };
  if (value) {
    const tooBig = value.file.size > maxBytes;
    return (
      <div className="max-w-2xl">
        <div className="flex items-center gap-3 rounded-lg border bg-surface px-3 py-2.5">
          <span className="flex size-9 shrink-0 items-center justify-center rounded-md border bg-surface-2 text-muted">
            <FileUp className="size-4" strokeWidth={1.75} />
          </span>
          <span className="min-w-0 flex-1">
            <span className="block truncate font-mono text-[12.5px] font-medium">{value.file.name}</span>
            <span className="block text-[12px] text-muted tabular">
              {bytes(value.file.size)} · <span className={cn(value.format === "unknown" && "text-fail")}>{formatText[value.format]}</span>
            </span>
          </span>
          <Button type="button" onClick={() => onChange(undefined)}>
            Change
          </Button>
        </div>
        {tooBig && (
          <p className="mt-1.5 text-[12px] text-fail">
            {bytes(value.file.size)} is more than the {bytes(maxBytes)} a file may be.
          </p>
        )}
        {value.format === "unknown" && !tooBig && (
          <p className="mt-1.5 text-[12px] text-fail">Its first bytes are neither qcow2 nor a raw disk; Hangar converts only those two.</p>
        )}
      </div>
    );
  }
  return (
    <div
      onDragOver={(e) => {
        e.preventDefault();
        setOver(true);
      }}
      onDragLeave={() => setOver(false)}
      onDrop={(e) => {
        e.preventDefault();
        setOver(false);
        void take(e.dataTransfer.files[0]);
      }}
      className={cn(
        "flex max-w-2xl flex-col items-center justify-center gap-2 rounded-lg border border-dashed bg-surface px-6 py-8 text-center transition-colors",
        over ? "border-accent bg-accent-soft" : "hover:border-border-strong",
      )}
    >
      <span className="flex size-10 items-center justify-center rounded-full bg-surface-2 text-muted">
        <Upload className="size-4.5" strokeWidth={1.75} />
      </span>
      <div className="text-[13.5px] font-medium">Drop a disk image here</div>
      <div className="text-[12.5px] text-muted">
        qcow2 or raw, up to {bytes(maxBytes)}. Or{" "}
        <button type="button" onClick={() => input.current?.click()} className="font-medium text-accent hover:underline">
          choose a file
        </button>
        .
      </div>
      <input ref={input} type="file" aria-label="Image file" className="hidden" onChange={(e) => void take(e.target.files?.[0])} />
    </div>
  );
}

/** Bar is a progress bar, fraction 0 to 1; indeterminate moves while the end is not known. */
export function Bar({ fraction, indeterminate, tone = "accent" }: { fraction?: number; indeterminate?: boolean; tone?: "accent" | "fail" }) {
  return (
    <div className="h-1.5 w-full overflow-hidden rounded-full bg-surface-2" role="progressbar" aria-valuenow={fraction === undefined ? undefined : Math.round(fraction * 100)}>
      {indeterminate ? (
        <div className="bar-slide h-full w-1/3 rounded-full bg-accent" />
      ) : (
        <div className={cn("h-full rounded-full transition-[width]", tone === "fail" ? "bg-fail" : "bg-accent")} style={{ width: `${Math.round((fraction ?? 0) * 1000) / 10}%` }} />
      )}
    </div>
  );
}

function Row({ left, right }: { left: ReactNode; right?: ReactNode }) {
  return (
    <div className="flex items-baseline justify-between gap-3 text-[12.5px]">
      <span className="text-muted tabular">{left}</span>
      {right && <span className="text-subtle tabular">{right}</span>}
    </div>
  );
}

/** What the card shows of an upload: this tab's when it runs one, else the node's count. */
function shown(local: UploadState | undefined, received: number, size: number) {
  if (local && "received" in local) return { ...local, received: Math.max(local.received, received) };
  return { phase: "elsewhere" as const, received, size };
}

/**
 * UploadCard is an uploaded image's file on its way: the upload this tab
 * runs, paused or retrying; or, when none runs here, how much the node holds
 * and a way to choose the same file again and go on. A file of another
 * size is refused at once; one of the same size but other content fails
 * the image's sha256 when it was declared, and otherwise makes an image of
 * what was sent, which the node's sha256 in its status tells.
 */
export function UploadCard({ ci, project }: { ci: CustomImage; project: string }) {
  const name = ci.metadata?.name ?? "";
  const src = uploadSource(ci);
  const local = useUpload(project, name);
  const [wrong, setWrong] = useState<string>();
  const input = useRef<HTMLInputElement>(null);
  const size = src?.size ?? 0;
  const s = shown(local, Number(ci.status?.receivedBytes ?? 0n), size);
  const fraction = size > 0 ? s.received / size : 0;
  const expires = ci.status?.uploadExpires ? new Date(Number(ci.status.uploadExpires.seconds) * 1000) : undefined;
  const now = useNow();

  if (!src || customImageState(ci).step !== "awaiting-upload") return null;

  const choose = (file: File | undefined) => {
    if (!file) return;
    if (file.size !== size) {
      setWrong(`${file.name} is ${bytes(file.size)}; this image was made for a file of ${bytes(size)}.`);
      return;
    }
    setWrong(undefined);
    startUpload(project, name, file, s.received);
  };
  const left = s.phase === "uploading" && s.bytesPerSecond > 0 ? (size - s.received) / s.bytesPerSecond : undefined;

  return (
    <Card>
      <CardHeader title="Upload" />
      <div className="space-y-3 px-4 py-4">
        <div className="flex items-baseline justify-between gap-3">
          <span className="text-[22px] font-semibold tracking-tight tabular">{Math.floor(fraction * 100)}%</span>
          <span className="text-[12.5px] text-muted tabular">
            {bytes(s.received)} of {bytes(size)}
          </span>
        </div>
        <Bar fraction={fraction} tone={s.phase === "failed" ? "fail" : "accent"} />
        {s.phase === "uploading" && (
          <Row left={s.bytesPerSecond > 0 ? `${bytes(s.bytesPerSecond)}/s` : "Measuring speed…"} right={left !== undefined ? `about ${duration(left)} left` : undefined} />
        )}
        {s.phase === "retrying" && <Row left={`Retrying in ${s.inSeconds} s: ${s.error}`} />}
        {s.phase === "paused" && <Row left="Paused. The node keeps what it has; resume goes on from there." />}
        {s.phase === "failed" && (
          <Notice tone="error" title="The upload stopped">
            {s.error}
          </Notice>
        )}
        {s.phase === "elsewhere" && (
          <Notice tone="info" title={s.received > 0 ? "No upload runs in this tab" : "Waiting for the file"}>
            {s.received > 0
              ? "The node holds what was sent so far. Choose the same file to go on from there."
              : "Choose the file this image was made for to send it."}
            {expires && ` It must be whole within ${duration((expires.getTime() - now) / 1000)}, or the image fails.`}
          </Notice>
        )}
        {wrong && <p className="text-[12px] text-fail">{wrong}</p>}
        <div className="flex flex-wrap gap-2">
          {(s.phase === "uploading" || s.phase === "retrying") && (
            <Button type="button" onClick={() => pauseUpload(project, name)}>
              <Pause /> Pause
            </Button>
          )}
          {(s.phase === "paused" || s.phase === "failed") && (
            <Button type="button" variant="primary" onClick={() => resumeUpload(project, name)}>
              {s.phase === "failed" ? <RotateCw /> : <Play />} {s.phase === "failed" ? "Try again" : "Resume"}
            </Button>
          )}
          {s.phase === "elsewhere" && (
            <>
              <Button type="button" variant="primary" onClick={() => input.current?.click()}>
                <FileUp /> Choose file{s.received > 0 ? " to resume" : ""}
              </Button>
              <input ref={input} type="file" aria-label="Image file to resume" className="hidden" onChange={(e) => choose(e.target.files?.[0])} />
            </>
          )}
        </div>
      </div>
    </Card>
  );
}

/** useNow is the time, again every half minute. */
function useNow(): number {
  const [now, setNow] = useState(() => Date.now());
  useEffect(() => {
    const t = setInterval(() => setNow(Date.now()), 30_000);
    return () => clearInterval(t);
  }, []);
  return now;
}
