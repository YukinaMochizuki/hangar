import { create } from "@bufbuild/protobuf";
import { timestampFromMs } from "@bufbuild/protobuf/wkt";
import { ContainerSchema, RestartPolicy } from "@hangar/api/hangar/compute/v1alpha1/container_pb";
import { MachineLogRecordSchema, MachineLogStream } from "@hangar/api/hangar/compute/v1alpha1/machine_capability_pb";
import { describe, expect, it } from "vitest";
import type { MachineView } from "@/lib/machines";
import { bootEnd, restartLoop, toLines, type LogLine } from "@/pages/machine-detail";

const container = (restartPolicy = RestartPolicy.UNSPECIFIED, stopped = false) =>
  ({ kind: "Container", stopped, container: create(ContainerSchema, { spec: { restartPolicy } }) }) as unknown as MachineView;

const ended = (boot: number, msAgo: number, now: number, code = 0, signal = 0): LogLine => {
  let n = 0;
  return toLines(
    create(MachineLogRecordSchema, {
      boot: BigInt(boot),
      stream: MachineLogStream.EXIT,
      time: timestampFromMs(now - msAgo),
      content: { case: "exit", value: { code, signal } },
    }),
    () => n++,
  )[0];
};

describe("the line that ends a boot", () => {
  const now = Date.UTC(2026, 9, 19, 1, 0);

  it("says a boot followed it in the log, under the default restart policy", () => {
    expect(bootEnd(ended(1, 0, now), 2, container())).toBe("boot 1 ended · the process exited with code 0 · booted again (restart: always)");
  });

  it("says what the last boot's end leads to", () => {
    expect(bootEnd(ended(6, 0, now), 6, container())).toBe("boot 6 ended · the process exited with code 0 · booting again (restart: always)");
    expect(bootEnd(ended(2, 0, now), 2, container(RestartPolicy.ON_FAILURE))).toBe(
      "boot 2 ended · the process exited with code 0 · stays off (restart: on failure)",
    );
    expect(bootEnd(ended(2, 0, now, 1), 2, container(RestartPolicy.ON_FAILURE))).toBe(
      "boot 2 ended · the process exited with code 1 · booting again (restart: on failure)",
    );
    expect(bootEnd(ended(2, 0, now), 2, container(RestartPolicy.NEVER))).toBe("boot 2 ended · the process exited with code 0 · stays off (restart: never)");
    expect(bootEnd(ended(2, 0, now, 143, 15), 2, container(RestartPolicy.ALWAYS, true))).toBe(
      "boot 2 ended · the process was ended by signal 15 (exit 143) · stopped",
    );
  });
});

describe("a container starting over", () => {
  const now = Date.UTC(2026, 9, 19, 1, 0);
  const minutes = 60_000;

  it("is three or more boots ended in the last ten minutes", () => {
    const lines = [ended(1, 9 * minutes, now), ended(2, 5 * minutes, now), ended(3, 1 * minutes, now)];
    expect(restartLoop(lines, container(), now)).toBe(3);
    expect(restartLoop(lines.slice(1), container(), now)).toBe(0);
  });

  it("forgets boots that ended longer ago", () => {
    const lines = [ended(1, 30 * minutes, now), ended(2, 20 * minutes, now), ended(3, 11 * minutes, now)];
    expect(restartLoop(lines, container(), now)).toBe(0);
  });

  it("is not said of a stopped container", () => {
    const lines = [ended(1, 3 * minutes, now), ended(2, 2 * minutes, now), ended(3, 1 * minutes, now)];
    expect(restartLoop(lines, container(RestartPolicy.ALWAYS, true), now)).toBe(0);
  });
});
