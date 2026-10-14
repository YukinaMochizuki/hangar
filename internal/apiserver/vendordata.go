package apiserver

import (
	"fmt"
	"strings"

	"google.golang.org/protobuf/proto"

	computev1alpha1 "github.com/YukinaMochizuki/hangar/gen/go/hangar/compute/v1alpha1"
)

// vendorBoundary separates vendor-data's parts; nothing in them holds it.
const vendorBoundary = "==hangar-vendor-data=="

// vendorData is a VM guest's vendor-data, the platform's settings: its
// keys as cloud-config and, with a CA, a script trusting it for the VM's
// principal (sshScript); see docs/decisions/vm-ssh.md.
func vendorData(vm *computev1alpha1.VirtualMachine, caKey string) string {
	keys := keysConfig(vm.GetSpec())
	if caKey == "" {
		return keys
	}
	var b strings.Builder
	fmt.Fprintf(&b, "Content-Type: multipart/mixed; boundary=%q\nMIME-Version: 1.0\n\n", vendorBoundary)
	part := func(typ, body string) {
		fmt.Fprintf(&b, "--%s\nContent-Type: %s; charset=\"utf-8\"\nMIME-Version: 1.0\n\n%s", vendorBoundary, typ, body)
	}
	if keys != "" {
		part("text/cloud-config", keys)
	}
	part("text/x-shellscript", sshScript(caKey, vm.GetMetadata().GetUid()))
	fmt.Fprintf(&b, "--%s--\n", vendorBoundary)
	return b.String()
}

// sshScript has the guest's sshd trust the CA for principal, the VM's uid,
// for every user but root; why a script, and how, is in vm-ssh.md.
func sshScript(caKey, principal string) string {
	return `#!/bin/sh
# The platform's SSH CA, for hangar ssh (docs/decisions/vm-ssh.md).
set -e
dir=/etc/ssh/hangar
conf=/etc/ssh/sshd_config.d/10-hangar-ssh.conf
install -d -m 0755 -o root -g root "$dir"
cat > "$dir/ca.pub" <<'HANGAR_EOF'
` + caKey + `
HANGAR_EOF
printf '%s\n' '` + principal + `' > "$dir/principals"
cat > "$dir/principals-for" <<'HANGAR_EOF'
#!/bin/sh
# The VM's principal, for any user but root.
[ "$1" = root ] || cat /etc/ssh/hangar/principals
HANGAR_EOF
chmod 0644 "$dir/ca.pub" "$dir/principals"
chmod 0755 "$dir/principals-for"
install -d -m 0755 /etc/ssh/sshd_config.d
grep -q '^Include /etc/ssh/sshd_config.d/\*.conf' /etc/ssh/sshd_config ||
	sed -i '1i Include /etc/ssh/sshd_config.d/*.conf' /etc/ssh/sshd_config
cat > "$conf" <<'HANGAR_EOF'
TrustedUserCAKeys /etc/ssh/hangar/ca.pub
AuthorizedPrincipalsCommand /etc/ssh/hangar/principals-for %u
AuthorizedPrincipalsCommandUser nobody
HANGAR_EOF
sshd=$(command -v sshd || echo /usr/sbin/sshd)
# sshd -t wants the directory sshd's start makes, which a socket-activated
# sshd not yet started has not.
install -d -m 0755 /run/sshd
if ! "$sshd" -t; then
	rm -f "$conf"
	echo "hangar: sshd refused the SSH CA's config; left it out" >&2
	exit 1
fi
systemctl try-reload-or-restart ssh.service sshd.service 2>/dev/null || true
echo "hangar: sshd trusts the platform's SSH CA"
`
}

// vendorDataSize is how long vm's vendor-data is, before vm has its uid as
// after: a uid is as long as any other.
func vendorDataSize(vm *computev1alpha1.VirtualMachine, caKey string) int {
	if vm.GetMetadata().GetUid() == "" {
		vm = proto.Clone(vm).(*computev1alpha1.VirtualMachine)
		vm.Metadata.Uid = "00000000-0000-0000-0000-000000000000"
	}
	return len(vendorData(vm, caKey))
}
