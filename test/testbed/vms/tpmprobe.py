#!/usr/bin/env python3
# The TPM tests' probe (tpm_test.go), run in an Ubuntu guest, which has no
# tpm2-tools: raw TPM 2.0 commands on /dev/tpmrm0. It defines the NV index
# NV and writes NV_DATA to it, and makes an ECC primary in the endorsement
# hierarchy persistent at PERSIST, each unless the TPM holds it already,
# then prints one line, hangar-tpm:[TAG] ..., with what each command
# answered, what the index holds and the persistent key's public area's
# sha256, which are the TPM's own across boots only if its state is.
import hashlib
import os
import struct
import sys

NV = 0x01500016
NV_DATA = b"hangar01"
PERSIST = 0x81010001
OWNER, ENDORSEMENT = 0x40000001, 0x4000000B
# TPM_RS_PW: no nonce, continueSession, an empty password.
PW = struct.pack(">IHBH", 0x40000009, 0, 1, 0)
# One open for the whole run: the resource manager flushes a closed file's
# transient objects.
FD = os.open("/dev/tpmrm0", os.O_RDWR)


def cmd(cc, handles=b"", params=b"", auth=False):
    body = handles + (struct.pack(">I", len(PW)) + PW if auth else b"") + params
    req = struct.pack(">HII", 0x8002 if auth else 0x8001, 10 + len(body), cc) + body
    os.write(FD, req)
    rsp = os.read(FD, 4096)
    return struct.unpack(">I", rsp[6:10])[0], rsp[10:]


def tpm2b(b):
    return struct.pack(">H", len(b)) + b


def handles(first):
    # TPM2_GetCapability(TPM_CAP_HANDLES).
    rc, r = cmd(0x17A, params=struct.pack(">III", 1, first, 64))
    if rc:
        return []
    n = struct.unpack(">I", r[5:9])[0]
    return [h for (h,) in struct.iter_unpack(">I", r[9:9 + 4 * n])]


def main():
    tag = sys.argv[1] if len(sys.argv) > 1 else ""
    out = []
    if NV not in handles(0x01000000):
        # OWNERWRITE, OWNERREAD, NO_DA; ordinary; sha256.
        pub = struct.pack(">IHI", NV, 0x000B, 0x02020002) + tpm2b(b"") + struct.pack(">H", len(NV_DATA))
        rc, _ = cmd(0x12A, struct.pack(">I", OWNER), tpm2b(b"") + tpm2b(pub), auth=True)
        out.append("nv-define=%#x" % rc)
        rc, _ = cmd(0x137, struct.pack(">II", OWNER, NV), tpm2b(NV_DATA) + struct.pack(">H", 0), auth=True)
        out.append("nv-write=%#x" % rc)
    rc, r = cmd(0x14E, struct.pack(">II", OWNER, NV), struct.pack(">HH", len(NV_DATA), 0), auth=True)
    out.append("nv=%s" % (r[6:6 + len(NV_DATA)].decode(errors="replace") if rc == 0 else "rc=%#x" % rc))

    if PERSIST not in handles(0x81000000):
        # An ECC P-256 restricted decryption key, an EK's template.
        tmpl = struct.pack(">HHI", 0x0023, 0x000B, 0x00030072) + tpm2b(b"") + \
            struct.pack(">HHHHHHH", 0x0006, 0x0080, 0x0043, 0x0010, 0x0003, 0x0010, 0) + struct.pack(">H", 0)
        rc, r = cmd(0x131, struct.pack(">I", ENDORSEMENT),
                    tpm2b(tpm2b(b"") + tpm2b(b"")) + tpm2b(tmpl) + tpm2b(b"") + struct.pack(">I", 0), auth=True)
        out.append("create-primary=%#x" % rc)
        if rc == 0:
            h = struct.unpack(">I", r[:4])[0]
            rc, _ = cmd(0x120, struct.pack(">II", OWNER, h), struct.pack(">I", PERSIST), auth=True)
            out.append("evict=%#x" % rc)
            cmd(0x165, params=struct.pack(">I", h))
    rc, r = cmd(0x173, struct.pack(">I", PERSIST))
    if rc == 0:
        n = struct.unpack(">H", r[:2])[0]
        out.append("persistent=%s" % hashlib.sha256(r[2:2 + n]).hexdigest()[:16])
    else:
        out.append("persistent=rc=%#x" % rc)
    rc, _ = cmd(0x17B, params=struct.pack(">H", 8))
    out.append("getrandom=%s" % ("ok" if rc == 0 else "rc=%#x" % rc))
    print("hangar-tpm:[%s] %s" % (tag, " ".join(out)), flush=True)


main()
