package boot

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"golang.org/x/sys/unix"

	guestv1 "github.com/YukinaMochizuki/hangar/gen/go/hangar/guest/v1"
)

// TestASharesMountIsTheDesigns: NFSv4.2 from the server's root, hard with a
// 5 s timeout, nosuid and nodev always, read-only only as declared.
func TestASharesMountIsTheDesigns(t *testing.T) {
	share := func(addr string, ro bool) *guestv1.Volume {
		return &guestv1.Volume{Name: "s", Target: "/data", ReadOnly: ro, FileShare: &guestv1.FileShareMount{Address: addr}}
	}
	for name, tt := range map[string]struct {
		v            *guestv1.Volume
		source, data string
		flags        uintptr
	}{
		"IPv4": {share("10.0.0.7", false), "10.0.0.7:/", "vers=4.2,hard,timeo=50,addr=10.0.0.7", unix.MS_NOSUID | unix.MS_NODEV},
		"read-only": {share("10.0.0.7", true), "10.0.0.7:/", "vers=4.2,hard,timeo=50,addr=10.0.0.7",
			unix.MS_NOSUID | unix.MS_NODEV | unix.MS_RDONLY},
		"IPv6":         {share("fd00::7", false), "[fd00::7]:/", "vers=4.2,hard,timeo=50,addr=fd00::7", unix.MS_NOSUID | unix.MS_NODEV},
		"IPv4 in IPv6": {share("::ffff:10.0.0.7", false), "10.0.0.7:/", "vers=4.2,hard,timeo=50,addr=10.0.0.7", unix.MS_NOSUID | unix.MS_NODEV},
	} {
		m, err := shareMount(tt.v, "/volumes/0")
		if err != nil {
			t.Errorf("%s: %v", name, err)
			continue
		}
		want := mount{tt.source, "/volumes/0", "nfs4", tt.data, tt.flags}
		if m != want {
			t.Errorf("%s: mounted as %+v, want %+v", name, m, want)
		}
	}
	for name, v := range map[string]*guestv1.Volume{
		"a host name":              share("files.internal", false),
		"an address with a prefix": share("10.0.0.7/24", false),
		"an address with a zone":   share("fe80::1%eth0", false),
		"no address":               share("", false),
		"a device too":             {Device: "/dev/vdc", FileShare: &guestv1.FileShareMount{Address: "10.0.0.7"}},
		"an option in the address": share("10.0.0.7,suid", false),
	} {
		if m, err := shareMount(v, "/volumes/0"); err == nil {
			t.Errorf("%s: mounted as %+v", name, m)
		}
	}
}

// TestTheClientIdentityIsTheMachines: the machine's uid becomes the NFS
// client identity, and a FileShare without one fails the boot.
func TestTheClientIdentityIsTheMachines(t *testing.T) {
	file := filepath.Join(t.TempDir(), "identifier")
	if err := os.WriteFile(file, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	defer func(old string) { nfsClientID = old }(nfsClientID)
	nfsClientID = file

	if err := setNFSClientID(""); err == nil {
		t.Error("an empty identity was taken")
	}
	if err := setNFSClientID("0f6c2a7e-uid"); err != nil {
		t.Fatal(err)
	}
	if got, _ := os.ReadFile(file); strings.TrimSpace(string(got)) != "0f6c2a7e-uid" {
		t.Errorf("the identity written is %q", got)
	}
}
