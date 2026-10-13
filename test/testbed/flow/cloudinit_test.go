//go:build testbed

package flow

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/YukinaMochizuki/hangar/test/testbed/internal/diskimgtest"
)

// cloudInitKey is a key the VM is given; nothing logs in with it, the
// guest only shows it was given.
const cloudInitKey = "ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAIJ8eq2bLz6uO0j+5GcUpS0P6S1vUfQb5n6S2K9zUj1n3 hangar-testbed"

// cloudInitUserData writes a file, then shows on the serial console what it
// holds, and what else cloud-init was told: the hostname, the instance id,
// and the default user's keys from vendor-data.
const cloudInitUserData = `#cloud-config
write_files:
- path: /etc/hangar-v4
  content: "written by user-data\n"
runcmd:
- - sh
  - -c
  - |
    {
      echo "hangar-v4 file: $(cat /etc/hangar-v4)"
      echo "hangar-v4 host: $(hostname)"
      echo "hangar-v4 instance: $(cat /run/cloud-init/.instance-id)"
      sed 's/^/hangar-v4 key: /' /home/ubuntu/.ssh/authorized_keys
      echo "hangar-v4 done"
    } > /dev/ttyS0
`

// TestAVMBootsWithItsCloudInit: an Ubuntu cloud image, from the image
// catalogue, boots with a key and a user-data: its cloud-init finds the
// metadata service through its network's DHCP, runs the user-data, which
// writes a file, and gives the key to the default user, ubuntu, as
// hangar logs vm shows from the serial console.
func TestAVMBootsWithItsCloudInit(t *testing.T) {
	if installed {
		t.Skip("the installed agent trusts no test server's certificate")
	}
	srv := diskimgtest.New(t)
	url, _ := srv.Serve(t, diskimgtest.UbuntuName, diskimgtest.Ubuntu(t))
	fw, certs := diskimgtest.Firmware(t), srv.CertFile(t)
	c := startClusterWith(t, func(c *cluster) {
		c.nodeArgs = []string{"--vm-firmware", fw}
		c.nodeEnv = []string{"SSL_CERT_FILE=" + certs}
	})
	c.applyYAML(fmt.Sprintf(`apiVersion: storage.hangarlab.cloud/v1alpha1
kind: Image
metadata: {name: noble}
spec: {url: %q, sha256: %s, os: ubuntu, version: "24.04"}
`, url, diskimgtest.UbuntuSHA256))
	ud, _ := json.Marshal(cloudInitUserData)
	key, _ := json.Marshal(cloudInitKey)
	c.applyYAML(fmt.Sprintf(`apiVersion: compute.hangarlab.cloud/v1alpha1
kind: VirtualMachine
metadata: {name: cloudy}
spec:
  rootDisk: {fromImage: {image: {name: noble}}}
  resources: {cpu: 2, memory: 1Gi}
  sshAuthorizedKeys: [%s]
  userData: %s
`, key, ud))
	start := time.Now()
	c.cli("wait", "vm", "cloudy", "--for=condition=Ready", "--timeout=10m")
	t.Logf("Ready %v after the apply", time.Since(start).Round(time.Second))
	var vm struct {
		Metadata struct{ UID string }
	}
	if err := json.Unmarshal([]byte(c.cli("get", "vm", "cloudy", "-o", "json")), &vm); err != nil {
		t.Fatal(err)
	}

	var out string
	eventually(t, 12*time.Minute, "the user-data's report on the serial console", func() (bool, string) {
		out = c.cli("logs", "vm", "cloudy")
		lines := strings.Split(out, "\n")
		return strings.Contains(out, "hangar-v4 done"), strings.Join(lines[max(0, len(lines)-15):], "\n")
	})
	t.Logf("cloud-init ran %v after the apply", time.Since(start).Round(time.Second))
	for _, want := range []string{
		"hangar-v4 file: written by user-data",
		"hangar-v4 host: cloudy",
		"hangar-v4 instance: " + vm.Metadata.UID,
		"hangar-v4 key: " + cloudInitKey,
	} {
		if !strings.Contains(out, want) {
			t.Errorf("the serial console lacks %q", want)
		}
	}
	for _, line := range strings.Split(out, "\n") {
		if strings.Contains(line, "hangar-v4") || strings.Contains(strings.ToLower(line), "datasource") {
			t.Logf("console: %s", strings.TrimSpace(line))
		}
	}

	c.cli("delete", "vm", "cloudy")
	c.cli("wait", "vm", "cloudy", "--for=delete", "--timeout=3m")
}
