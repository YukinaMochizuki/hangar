import { Code, ConnectError } from "@connectrpc/connect";
import { describe, expect, it, vi } from "vitest";
import { readModifyWrite } from "@/lib/actions";

type Obj = { version: number; runState?: string };

describe("readModifyWrite", () => {
  it("reads again and writes the change on the newer version after a conflict", async () => {
    let version = 1;
    const read = vi.fn(async (): Promise<Obj> => ({ version }));
    const written: Obj[] = [];
    const write = vi.fn(async (o: Obj) => {
      if (o.version !== 2) {
        version = 2; // Someone wrote in between.
        throw new ConnectError("resource version moved", Code.Aborted);
      }
      written.push(o);
    });
    await readModifyWrite(read, (o) => (o.runState = "stopped"), write);
    expect(read).toHaveBeenCalledTimes(2);
    expect(written).toEqual([{ version: 2, runState: "stopped" }]);
  });

  it("gives up after a few conflicts in a row", async () => {
    const read = vi.fn(async (): Promise<Obj> => ({ version: 1 }));
    const write = vi.fn(async () => {
      throw new ConnectError("resource version moved", Code.Aborted);
    });
    await expect(readModifyWrite(read, () => {}, write)).rejects.toMatchObject({ code: Code.Aborted });
    expect(write).toHaveBeenCalledTimes(4);
  });

  it("does not retry any other error", async () => {
    const read = vi.fn(async (): Promise<Obj> => ({ version: 1 }));
    const write = vi.fn(async () => {
      throw new ConnectError("no", Code.PermissionDenied);
    });
    await expect(readModifyWrite(read, () => {}, write)).rejects.toMatchObject({ code: Code.PermissionDenied });
    expect(read).toHaveBeenCalledOnce();
  });
});
