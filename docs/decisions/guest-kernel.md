# One platform guest kernel for containers, LTS only

Status: accepted, 2026-10.

## Context

Every container is a Cloud Hypervisor guest booted directly from a kernel
the host supplies (`docs/decisions/machine-model.md`). Containers run their
image's userspace on that kernel, and users run Docker inside them, so the
kernel decides what every container can do: which filesystems, which
netfilter interface, which cgroup controllers. A distribution kernel is built
for real hardware and boots through firmware and an initramfs; it is large
and slow to boot for a guest that only ever sees virtio devices.
VirtualMachines are not containers: they boot their image's own kernel
through firmware (`docs/decisions/vm-boot.md`), and nothing here applies to
them.

## Decision

- Hangar ships one guest kernel, built from `guest/kernel/` and booted by
  every container's machine. Container images do not bring their own
  kernel. One kernel is one config to audit, one build to test, and one
  boot path to keep fast.
- The config is x86_64 `allnoconfig` plus `guest/kernel/hangar.fragment`, with
  everything built in and no modules: there is no module tree in the guest
  to keep in step with the kernel. The build fails if
  Kconfig drops anything the fragment asks for, and the resolved config is
  committed as `guest/kernel/config` so every change is reviewed as a diff of
  what the kernel actually contains.
- `allnoconfig` sets every bool it may to n, whatever its Kconfig default,
  so nothing the guest relies on is left to a default: every option this
  file names with its `CONFIG_` prefix is a line of the fragment, and a test
  (`guest/kernel/fragment_test.go`, part of `make check`) holds the two in
  step and checks the committed config against the fragment. The build uses
  `ARCH=x86_64`, so the config is a 64-bit one from the start.
- Only longterm (LTS) releases, currently the 6.18 line. Patch releases are
  taken as they come; the move to the next LTS happens once it is released,
  not to stable-only releases in between. The version and the tarball's
  sha256 are pinned in `guest/kernel/kernel.env`.
- The output is an uncompressed `vmlinux`, which CH boots through the PVH entry
  point; it starts faster than a `bzImage`, which has to decompress itself.
- One platform initramfs, built from `guest/initramfs/`, carries only
  hangar-init as `/init` and the empty mount points it needs. hangar-init
  mounts the image's root disk, switches to it and stays PID 1, so images
  need nothing of ours inside them. The archive is an uncompressed,
  reproducible newc cpio (`CONFIG_BLK_DEV_INITRD`), and the kernel builds in
  no `RD_*` decompressor: CH copies the file from the host page cache
  straight into guest memory, so compressing its few MB would only add
  decompression time to every boot. `CONFIG_ACPI_TABLE_UPGRADE`, which
  initrd support turns on, is off, since the initramfs never carries ACPI
  tables.
- The command line includes `quiet`. The console is an emulated 8250 UART and
  every character the kernel prints to it is a VM exit; a full boot log
  costs hundreds of milliseconds per boot. Panics and warnings still print.

## Hardening: named and on

The guest runs the tenant's code as root on a kernel the tenant cannot
change, so the kernel carries the hardening a distribution kernel would,
each option named in the fragment:

- CPU vulnerability mitigations, all built in, the kernel choosing per CPU
  at boot: `CONFIG_CPU_MITIGATIONS`, `CONFIG_MITIGATION_PAGE_TABLE_ISOLATION`,
  `CONFIG_MITIGATION_RETPOLINE`, `CONFIG_MITIGATION_RETHUNK`,
  `CONFIG_MITIGATION_UNRET_ENTRY`, `CONFIG_MITIGATION_CALL_DEPTH_TRACKING`,
  `CONFIG_MITIGATION_IBPB_ENTRY`, `CONFIG_MITIGATION_IBRS_ENTRY`,
  `CONFIG_MITIGATION_SRSO`, `CONFIG_MITIGATION_GDS`, `CONFIG_MITIGATION_RFDS`,
  `CONFIG_MITIGATION_SPECTRE_BHI`, `CONFIG_MITIGATION_MDS`,
  `CONFIG_MITIGATION_TAA`, `CONFIG_MITIGATION_MMIO_STALE_DATA`,
  `CONFIG_MITIGATION_L1TF`, `CONFIG_MITIGATION_RETBLEED`,
  `CONFIG_MITIGATION_SPECTRE_V1`, `CONFIG_MITIGATION_SPECTRE_V2`,
  `CONFIG_MITIGATION_SRBDS`, `CONFIG_MITIGATION_SSB`, `CONFIG_MITIGATION_ITS`,
  `CONFIG_MITIGATION_TSA`. `CONFIG_MITIGATION_SLS` stays off, as upstream
  defaults it. The command line adds `spectre_bhi=on`: by default the kernel
  mitigates Branch History Injection only when it also defends against its
  own guests, which a kernel without KVM never does, and leaves user to
  kernel attacks open. The testbed checks that the guest reports nothing in
  `/sys/devices/system/cpu/vulnerabilities` as vulnerable.
- Memory safety: `CONFIG_VMAP_STACK`, `CONFIG_STACKPROTECTOR`,
  `CONFIG_STACKPROTECTOR_STRONG`, `CONFIG_FORTIFY_SOURCE`,
  `CONFIG_HARDENED_USERCOPY`, `CONFIG_SLAB_FREELIST_RANDOM`,
  `CONFIG_SLAB_FREELIST_HARDENED`, `CONFIG_SHUFFLE_PAGE_ALLOCATOR`,
  `CONFIG_INIT_ON_ALLOC_DEFAULT_ON`, `CONFIG_INIT_STACK_ALL_ZERO`,
  `CONFIG_LIST_HARDENED`, `CONFIG_BUG_ON_DATA_CORRUPTION`.
- Layout and control flow: `CONFIG_RANDOMIZE_BASE`, `CONFIG_RANDOMIZE_MEMORY`,
  `CONFIG_RANDOMIZE_KSTACK_OFFSET`, `CONFIG_STRICT_KERNEL_RWX`,
  `CONFIG_X86_KERNEL_IBT`, `CONFIG_LEGACY_VSYSCALL_XONLY`.
- Less for an attacker to read or reach: `CONFIG_SECURITY_DMESG_RESTRICT`,
  `CONFIG_BPF_UNPRIV_DEFAULT_OFF`, `CONFIG_X86_INTEL_MEMORY_PROTECTION_KEYS`
  for userspace that uses it, and `CONFIG_UNWINDER_ORC` for oops reports
  that can be read.

It costs boot time and syscall speed; `docs/runbook/testbed.md` records
what was measured when it was turned on.

**User namespaces stay on, as a trade-off.** `CONFIG_USER_NS` lets any
process in the guest create a namespace in which it is root, which is the
entry point of many kernel exploits. It stays on because Docker in a VM
needs it for rootless containers, `userns-remap` and the build features
based on it, and images that sandbox themselves (browsers, build tools)
expect it. A guest that does not need it can turn it off for itself with
the `user.max_user_namespaces` sysctl.

## What the fragment must keep supporting

- virtio blk, net, vsock and console (root disk, network, the guest agent's
  channel, the console), plus virtio-rng, balloon and mem:
  `CONFIG_VIRTIO_BLK`, `CONFIG_VIRTIO_NET`, `CONFIG_VIRTIO_VSOCKETS`,
  `CONFIG_VIRTIO_CONSOLE`, `CONFIG_HW_RANDOM_VIRTIO`, `CONFIG_VIRTIO_BALLOON`,
  `CONFIG_VIRTIO_MEM`.
- cgroup v2 with the controllers Docker and runc use (cpu, cpuset, memory with
  swap accounting, io, pids, devices through BPF, freezer, hugetlb):
  `CONFIG_CGROUPS`, `CONFIG_CGROUP_SCHED`, `CONFIG_CPUSETS`, `CONFIG_MEMCG`,
  `CONFIG_SWAP`, `CONFIG_BLK_CGROUP`, `CONFIG_CGROUP_PIDS`, `CONFIG_CGROUP_BPF`,
  `CONFIG_CGROUP_FREEZER`, `CONFIG_CGROUP_HUGETLB`.
- Namespaces (user, pid, net, ipc, uts, mount), seccomp filters, overlayfs and
  ext4: `CONFIG_USER_NS`, `CONFIG_PID_NS`, `CONFIG_NET_NS`, `CONFIG_IPC_NS`,
  `CONFIG_UTS_NS`, `CONFIG_SECCOMP_FILTER`, `CONFIG_OVERLAY_FS`,
  `CONFIG_EXT4_FS`.
- nftables, and the xtables matches and targets that iptables-nft uses through
  `CONFIG_NFT_COMPAT` (`CONFIG_NF_TABLES`). The legacy x_tables tables depend
  on `*_IPTABLES_LEGACY` and are not enabled, so guests have iptables-nft only
  (Docker works with it; an image that insists on `iptables-legacy` does not).
- Bridge (with netfilter and VLAN filtering), veth, tun, macvlan, ipvlan,
  vxlan and WireGuard, so container networks and overlays work inside a VM:
  `CONFIG_BRIDGE`, `CONFIG_BRIDGE_NETFILTER`, `CONFIG_BRIDGE_VLAN_FILTERING`,
  `CONFIG_VETH`, `CONFIG_TUN`, `CONFIG_MACVLAN`, `CONFIG_IPVLAN`, `CONFIG_VXLAN`,
  `CONFIG_WIREGUARD`.

## Consequences

- A guest that needs a kernel feature outside this list gets it by changing
  the fragment for everyone, or not at all.
- Kernel bumps are a platform-wide change: every machine runs the new kernel
  on its next boot. `docs/runbook/guest-kernel.md` covers how to make one.
- No loadable modules means no out-of-tree drivers in guests.
