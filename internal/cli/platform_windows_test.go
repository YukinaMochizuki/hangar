package cli_test

import (
	"errors"
	"fmt"
	"testing"
	"unsafe"

	"golang.org/x/sys/windows"
)

// What private means differs by platform (private_unix.go and
// private_windows.go); these helpers let one test check it on either.

// configHome is the variable whose directory holds the CLI's own config
// directory.
const configHome = "APPDATA"

// fixHint is in the error that refuses a file others may read.
const fixHint = "icacls"

func me(t *testing.T) *windows.SID {
	t.Helper()
	u, err := windows.GetCurrentProcessToken().GetTokenUser()
	if err != nil {
		t.Fatal(err)
	}
	return u.User.Sid
}

func securityOf(t *testing.T, path string) *windows.SECURITY_DESCRIPTOR {
	t.Helper()
	sd, err := windows.GetNamedSecurityInfo(path, windows.SE_FILE_OBJECT, windows.DACL_SECURITY_INFORMATION)
	if err != nil {
		t.Fatal(err)
	}
	return sd
}

// isPrivate says whether the DACL of the file or directory at path is
// protected and grants only the user, SYSTEM and the Administrators, the
// user among them.
func isPrivate(t *testing.T, path string) bool {
	t.Helper()
	sd := securityOf(t, path)
	control, _, err := sd.Control()
	if err != nil {
		t.Fatal(err)
	}
	if control&windows.SE_DACL_PROTECTED == 0 {
		return false
	}
	dacl, _, err := sd.DACL()
	if err != nil || dacl == nil {
		return false
	}
	user := me(t)
	mine := false
	for i := range uint32(dacl.AceCount) {
		var ace *windows.ACCESS_ALLOWED_ACE
		if err := windows.GetAce(dacl, i, &ace); err != nil {
			t.Fatal(err)
		}
		if ace.Header.AceType != windows.ACCESS_ALLOWED_ACE_TYPE {
			return false
		}
		sid := (*windows.SID)(unsafe.Pointer(&ace.SidStart))
		switch {
		case sid.Equals(user):
			mine = true
		case sid.IsWellKnown(windows.WinLocalSystemSid), sid.IsWellKnown(windows.WinBuiltinAdministratorsSid):
		default:
			return false
		}
	}
	return mine
}

// openUp lets Everyone read the file or directory at path, besides whoever
// may already.
func openUp(t *testing.T, path string) {
	t.Helper()
	dacl, _, err := securityOf(t, path).DACL()
	if err != nil {
		t.Fatal(err)
	}
	everyone, err := windows.CreateWellKnownSid(windows.WinWorldSid)
	if err != nil {
		t.Fatal(err)
	}
	opened, err := windows.ACLFromEntries([]windows.EXPLICIT_ACCESS{{
		AccessPermissions: windows.GENERIC_READ,
		AccessMode:        windows.GRANT_ACCESS,
		Inheritance:       windows.NO_INHERITANCE,
		Trustee: windows.TRUSTEE{
			TrusteeForm:  windows.TRUSTEE_IS_SID,
			TrusteeType:  windows.TRUSTEE_IS_WELL_KNOWN_GROUP,
			TrusteeValue: windows.TrusteeValueFromSID(everyone),
		},
	}}, dacl)
	if err != nil {
		t.Fatal(err)
	}
	if err := windows.SetNamedSecurityInfo(path, windows.SE_FILE_OBJECT, windows.DACL_SECURITY_INFORMATION, nil, nil, opened, nil); err != nil {
		t.Fatal(err)
	}
}

// closeUp makes the file at path private again.
func closeUp(t *testing.T, path string) {
	t.Helper()
	sd, err := windows.SecurityDescriptorFromString(fmt.Sprintf("D:P(A;;FA;;;SY)(A;;FA;;;BA)(A;;FA;;;%s)", me(t)))
	if err != nil {
		t.Fatal(err)
	}
	dacl, _, err := sd.DACL()
	if err != nil {
		t.Fatal(err)
	}
	if err := windows.SetNamedSecurityInfo(path, windows.SE_FILE_OBJECT,
		windows.DACL_SECURITY_INFORMATION|windows.PROTECTED_DACL_SECURITY_INFORMATION, nil, nil, dacl, nil); err != nil {
		t.Fatal(err)
	}
}

// accessOf is who may at path, to tell whether it changed.
func accessOf(t *testing.T, path string) string {
	t.Helper()
	return securityOf(t, path).String()
}

func mkfifo(string) error { return errors.ErrUnsupported }
