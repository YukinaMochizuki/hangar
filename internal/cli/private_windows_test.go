package cli

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"golang.org/x/sys/windows"
)

// widen grants Everyone read on path, besides whoever may already.
func widen(t *testing.T, path string) {
	t.Helper()
	sd, err := windows.GetNamedSecurityInfo(path, windows.SE_FILE_OBJECT, windows.DACL_SECURITY_INFORMATION)
	if err != nil {
		t.Fatal(err)
	}
	dacl, _, err := sd.DACL()
	if err != nil {
		t.Fatal(err)
	}
	everyone, err := windows.CreateWellKnownSid(windows.WinWorldSid)
	if err != nil {
		t.Fatal(err)
	}
	wider, err := windows.ACLFromEntries([]windows.EXPLICIT_ACCESS{{
		AccessPermissions: windows.GENERIC_READ,
		AccessMode:        windows.GRANT_ACCESS,
		Trustee: windows.TRUSTEE{
			TrusteeForm:  windows.TRUSTEE_IS_SID,
			TrusteeType:  windows.TRUSTEE_IS_WELL_KNOWN_GROUP,
			TrusteeValue: windows.TrusteeValueFromSID(everyone),
		},
	}}, dacl)
	if err != nil {
		t.Fatal(err)
	}
	if err := windows.SetNamedSecurityInfo(path, windows.SE_FILE_OBJECT, windows.DACL_SECURITY_INFORMATION, nil, nil, wider, nil); err != nil {
		t.Fatal(err)
	}
}

func exposureOf(t *testing.T, path string) (string, string) {
	t.Helper()
	f, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = f.Close() }()
	who, fix, err := exposure(f)
	if err != nil {
		t.Fatal(err)
	}
	return who, fix
}

// TestPrivateFileRoundTrip: what createPrivate and mkdirPrivate make,
// exposure finds private; once Everyone may read it, exposure names them
// and says how to take it back; setPrivate takes it back.
func TestPrivateFileRoundTrip(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "hangar")
	if err := mkdirPrivate(dir); err != nil {
		t.Fatal(err)
	}
	if seen, err := privateConfigDir(dir, false); err != nil || seen != "" {
		t.Errorf("the directory mkdirPrivate made: seen %q, %v", seen, err)
	}
	path := filepath.Join(dir, "credentials.yaml")
	if err := writePrivate(path, []byte("logins: {}\n")); err != nil {
		t.Fatal(err)
	}
	if who, _ := exposureOf(t, path); who != "" {
		t.Errorf("a file writePrivate made is exposed to %s", who)
	}
	widen(t, path)
	who, fix := exposureOf(t, path)
	if !strings.Contains(who, "Everyone") || !strings.Contains(fix, "icacls") || !strings.Contains(fix, path) {
		t.Errorf("a file Everyone may read: who %q, fix %q", who, fix)
	}
	if err := setPrivate(path, false); err != nil {
		t.Fatal(err)
	}
	if who, _ := exposureOf(t, path); who != "" {
		t.Errorf("setPrivate left it exposed to %s", who)
	}
}

// TestOthersReadsTheDescriptor: what others makes of security descriptors
// that differ in one thing each from a private one.
func TestOthersReadsTheDescriptor(t *testing.T) {
	me, err := currentUser()
	if err != nil {
		t.Fatal(err)
	}
	private := fmt.Sprintf("(A;;FA;;;SY)(A;;FA;;;BA)(A;;FA;;;%s)", me)
	for _, c := range []struct {
		name, sddl string
		// want is in what others says; "" is private.
		want string
	}{
		{"private", "O:%[1]sD:P" + private, ""},
		{"owned by the Administrators", "O:BAD:P" + private, ""},
		{"owned by TrustedInstaller", "O:" + trustedInstaller + "D:P" + private, ""},
		{"owned by Everyone", "O:WDD:P" + private, "its owner"},
		{"a NULL DACL", "O:%[1]sD:NO_ACCESS_CONTROL", noDACL},
		{"no DACL at all", "O:%[1]s", noDACL},
		{"Everyone's generic read", "O:%[1]sD:P" + private + "(A;;GR;;;WD)", "Everyone"},
		{"Users' delete", "O:%[1]sD:P" + private + "(A;;SD;;;BU)", `BUILTIN\Users`},
		{"Users' delete child", "O:%[1]sD:P" + private + "(A;;0x40;;;BU)", `BUILTIN\Users`},
		{"Everyone's WRITE_DAC", "O:%[1]sD:P" + private + "(A;;WD;;;WD)", "Everyone"},
		{"Everyone's read of the security alone", "O:%[1]sD:P" + private + "(A;;RC;;;WD)", ""},
		{"Everyone's, inherited only", "O:%[1]sD:P" + private + "(A;OICIIO;FA;;;WD)", ""},
		{"Everyone denied", "O:%[1]sD:P(D;;FA;;;WD)" + private, ""},
		{"an object ACE", "O:%[1]sD:P" + private + "(OA;;RP;00000000-0000-0000-0000-000000000001;;WD)", "does not read"},
	} {
		t.Run(c.name, func(t *testing.T) {
			sddl := c.sddl
			if strings.Contains(sddl, "%[1]s") {
				sddl = fmt.Sprintf(sddl, me)
			}
			sd, err := windows.SecurityDescriptorFromString(sddl)
			if err != nil {
				t.Fatalf("%s: %v", sddl, err)
			}
			f, err := others(sd)
			if err != nil {
				t.Fatal(err)
			}
			switch got := f.String(); {
			case c.want == "" && !f.none():
				t.Errorf("%s: exposed to %s", sddl, got)
			case c.want != "" && (f.none() || !strings.Contains(got, c.want)):
				t.Errorf("%s: exposed to %q, want %q in it", sddl, got, c.want)
			}
		})
	}
}

// TestFixTakesBackWhatOthersFound: the icacls line names the user and
// every principal by SID, and gives the file back to the user when someone
// else owns it.
func TestFixTakesBackWhatOthersFound(t *testing.T) {
	me, err := currentUser()
	if err != nil {
		t.Fatal(err)
	}
	sd, err := windows.SecurityDescriptorFromString("O:WDD:P(A;;FA;;;" + me.String() + ")(A;;GR;;;WD)(A;;FR;;;WD)(A;;FW;;;BU)")
	if err != nil {
		t.Fatal(err)
	}
	f, err := others(sd)
	if err != nil {
		t.Fatal(err)
	}
	fix, err := f.fix(`C:\a b\token`)
	if err != nil {
		t.Fatal(err)
	}
	want := `icacls "C:\a b\token" /setowner *` + me.String() + `, then icacls "C:\a b\token" /inheritance:r /grant:r *` + me.String() + `:F /remove *S-1-1-0 *S-1-5-32-545`
	if fix != want {
		t.Errorf("fix\n%s\nwant\n%s", fix, want)
	}
}

// TestCreatePrivateExcl: an excl createPrivate refuses a file that exists
// as os.OpenFile's O_EXCL does, which openSecretOut tells by os.ErrExist.
func TestCreatePrivateExcl(t *testing.T) {
	path := filepath.Join(t.TempDir(), "secret")
	f, err := createPrivate(path, true)
	if err != nil {
		t.Fatal(err)
	}
	_ = f.Close()
	if _, err := createPrivate(path, true); !os.IsExist(err) {
		t.Errorf("a second excl create: %v", err)
	}
}

// TestConfigDirWidenedIsTakenBack: the CLI's own config directory, once
// others may read it, is made private again; one HANGAR_CONFIG names is
// only reported.
func TestConfigDirWidenedIsTakenBack(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "hangar")
	if err := mkdirPrivate(dir); err != nil {
		t.Fatal(err)
	}
	widen(t, dir)
	if seen, err := privateConfigDir(dir, false); err != nil || !strings.Contains(seen, "Everyone") {
		t.Errorf("not its own: seen %q, %v", seen, err)
	}
	if seen, err := privateConfigDir(dir, true); err != nil || seen != "" {
		t.Errorf("its own: seen %q, %v", seen, err)
	}
	if seen, err := privateConfigDir(dir, false); err != nil || seen != "" {
		t.Errorf("after it was taken back: seen %q, %v", seen, err)
	}
}

// TestConfigPathIsAppData: the config is under %APPDATA% unless
// HANGAR_CONFIG names one.
func TestConfigPathIsAppData(t *testing.T) {
	env := map[string]string{"APPDATA": `C:\Users\alice\AppData\Roaming`, "HOME": `C:\elsewhere`, "XDG_CONFIG_HOME": `C:\xdg`}
	if got := configPath(func(k string) string { return env[k] }); got != `C:\Users\alice\AppData\Roaming\hangar\config.yaml` {
		t.Errorf("config path %s", got)
	}
	env["HANGAR_CONFIG"] = `D:\cfg.yaml`
	if got := configPath(func(k string) string { return env[k] }); got != `D:\cfg.yaml` {
		t.Errorf("config path with HANGAR_CONFIG %s", got)
	}
}

// TestProxyCommandSplitsBack: the ProxyCommand, as CreateProcess gets it,
// splits back into the words it was made of, its program quoted first as
// Win32-OpenSSH needs.
func TestProxyCommandSplitsBack(t *testing.T) {
	argv := []string{`C:\Program Files\hangar\hangar.exe`, "--context", `a "b" c\`, "--project", "web", "ssh-proxy", "virtualmachine", "vm"}
	cmd := proxyCommand(argv)
	if !strings.HasPrefix(cmd, `"C:\Program Files\hangar\hangar.exe" `) {
		t.Errorf("the program not quoted first: %s", cmd)
	}
	got, err := windows.DecomposeCommandLine(cmd)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Join(got, "\x00") != strings.Join(argv, "\x00") {
		t.Errorf("%s splits into %q", cmd, got)
	}
	if plain := proxyCommand([]string{`C:\hangar.exe`, "x"}); !strings.HasPrefix(plain, `"C:\hangar.exe" `) {
		t.Errorf("a program needing no quotes not quoted: %s", plain)
	}
}
