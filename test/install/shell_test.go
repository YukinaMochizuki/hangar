//go:build !windows

package install_test

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"testing"
)

const (
	cliName     = "hangar"
	installerIn = "install.sh.in"
)

func archiveName(tag string) string {
	return fmt.Sprintf("hangar_%s_%s_%s.tar.gz", tag, runtime.GOOS, runtime.GOARCH)
}

// packCLI packs a hangar that answers `which-release` with tag and runs cli
// otherwise.
func packCLI(t *testing.T, tag, cli string) []byte {
	script := fmt.Sprintf("#!/bin/sh\n[ \"$1\" = which-release ] && { echo %s; exit 0; }\nexec %s \"$@\"\n", tag, cli)
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	if err := tw.WriteHeader(&tar.Header{Name: "hangar", Mode: 0o755, Size: int64(len(script)), Typeflag: tar.TypeReg}); err != nil {
		t.Fatal(err)
	}
	if _, err := tw.Write([]byte(script)); err != nil {
		t.Fatal(err)
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	if err := gz.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

// keepUserPath has nothing to keep: install.sh only reads PATH.
func keepUserPath(*testing.T) {}

// run runs the installer made for tag with /bin/sh, with args.
func (s *standIn) run(tag, home string, args ...string) result {
	s.t.Helper()
	script := filepath.Join(s.t.TempDir(), "install.sh")
	if err := os.WriteFile(script, installer(s.t, tag), 0o755); err != nil {
		s.t.Fatal(err)
	}
	dir := filepath.Join(home, "bin")
	cmd := exec.Command("/bin/sh", append([]string{script, "--dir", dir}, args...)...)
	cmd.Env = []string{
		"PATH=/usr/bin:/bin",
		"HOME=" + home,
		"HANGAR_CONFIG=" + filepath.Join(home, "config", "config.yaml"),
		"HANGAR_INSTALL_API_URL=" + s.srv.URL + "/api",
	}
	if !s.githubBase {
		cmd.Env = append(cmd.Env, "HANGAR_INSTALL_BASE_URL="+s.srv.URL)
	}
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	err := cmd.Run()
	code := 0
	if err != nil {
		var ee *exec.ExitError
		if !errors.As(err, &ee) {
			s.t.Fatal(err)
		}
		code = ee.ExitCode()
	}
	return result{code: code, stderr: stderr.String(), dir: dir}
}
