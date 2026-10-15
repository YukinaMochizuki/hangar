package main

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/YukinaMochizuki/hangar/internal/cli"
)

const execStack = `apiVersion: tenancy.hangarlab.cloud/v1alpha1
kind: Tenant
metadata: {name: acme}
spec: {displayName: Acme}
---
apiVersion: tenancy.hangarlab.cloud/v1alpha1
kind: Project
metadata: {name: web}
spec: {tenant: acme}
---
apiVersion: compute.hangarlab.cloud/v1alpha1
kind: Container
metadata: {name: api}
spec:
  image: nginx:1
  resources: {cpu: 1, memory: 256Mi, disk: 1Gi}
`

// hangarCLI runs the hangar command with a context for the admin socket at
// sock, and returns its exit code and stderr.
func hangarCLI(t *testing.T, sock, stdin string, args ...string) (int, string) {
	t.Helper()
	config := filepath.Join(t.TempDir(), "config.yaml")
	env := func(k string) string {
		if k == "HANGAR_CONFIG" {
			return config
		}
		return ""
	}
	run := func(args ...string) (int, string) {
		var out, errb bytes.Buffer
		code := cli.Run(context.Background(), args, cli.IO{In: strings.NewReader(stdin), Out: &out, Err: &errb, Env: env}, cli.Options{})
		return code, errb.String()
	}
	if code, e := run("config", "set-context", "local", "--endpoint", "unix://"+sock, "--project", "web", "--use"); code != 0 {
		t.Fatal(e)
	}
	return run(args...)
}

// TestExecOverTheAdminSocket: hangar exec through the admin socket, against
// the apiserver as main runs it. Root on the host gets past authentication
// and the exec permission, with no binding of its own, to the container's
// state; any other uid stops at authentication.
func TestExecOverTheAdminSocket(t *testing.T) {
	t.Run("root", func(t *testing.T) {
		l := start(t, uint32(os.Getuid()))
		if code, e := hangarCLI(t, l.admin, execStack, "apply", "-f", "-"); code != 0 {
			t.Fatalf("apply as root on the host: exit %d: %s", code, e)
		}
		code, e := hangarCLI(t, l.admin, "", "exec", "ct", "api", "--", "true")
		if code == 0 || !strings.Contains(e, "failed_precondition") || !strings.Contains(e, `container "api"`) {
			t.Fatalf("exec: exit %d: %s; want it refused for the container's state alone", code, e)
		}
		if got := l.log.String(); !strings.Contains(got, `msg="exec refused" principal=local:root@n1 client_ip="" project=web plural=containers name=api code=failed_precondition`) {
			t.Errorf("the exec is not audited as root on the host:\n%s", got)
		}
	})
	t.Run("another uid", func(t *testing.T) {
		l := start(t, uint32(os.Getuid())+1)
		code, e := hangarCLI(t, l.admin, "", "exec", "ct", "api", "--", "true")
		if code == 0 || !strings.Contains(e, "unauthenticated") || !strings.Contains(e, "is not root") {
			t.Fatalf("exec: exit %d: %s; want unauthenticated", code, e)
		}
		if got := l.log.String(); !strings.Contains(got, `msg="exec refused" principal="" client_ip="" project=web plural=containers name=api code=unauthenticated`) {
			t.Errorf("the refusal is not audited:\n%s", got)
		}
	})
}
