package cli

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"unsafe"

	"golang.org/x/sys/windows"
)

// On Windows a file is private when its DACL lets no one but the user,
// SYSTEM and the Administrators group at it, as Win32-OpenSSH requires of a
// private key. What hangar makes private gets a protected DACL granting
// those three full control and nothing inherited.

// userConfigDir is where the CLI's config directory goes: %APPDATA%, the
// user's roaming application data, which only the user may read.
func userConfigDir(env func(string) string) string { return env("APPDATA") }

// currentUser is the SID of the user this process runs as.
var currentUser = sync.OnceValues(func() (*windows.SID, error) {
	u, err := windows.GetCurrentProcessToken().GetTokenUser()
	if err != nil {
		return nil, fmt.Errorf("reading who runs hangar: %w", err)
	}
	return u.User.Sid.Copy()
})

// privateSD is the security descriptor of a private file, or with dir of a
// private directory, whose files and subdirectories inherit it.
func privateSD(dir bool) (*windows.SECURITY_DESCRIPTOR, error) {
	me, err := currentUser()
	if err != nil {
		return nil, err
	}
	inherit := ""
	if dir {
		inherit = "OICI"
	}
	// P protects the DACL from inheriting; FA is full control; SY is
	// SYSTEM and BA the Administrators group.
	return windows.SecurityDescriptorFromString(fmt.Sprintf("D:P(A;%[1]s;FA;;;SY)(A;%[1]s;FA;;;BA)(A;%[1]s;FA;;;%[2]s)", inherit, me))
}

// mkdirPrivate makes dir, and any parent missing, and makes dir private if
// it makes it; one that exists is left as it is, as os.MkdirAll leaves its
// mode.
func mkdirPrivate(dir string) error {
	// As os.MkdirAll does: a drive's root, say, is not made again.
	if fi, err := os.Stat(dir); err == nil && fi.IsDir() {
		return nil
	}
	if err := os.MkdirAll(filepath.Dir(dir), 0o700); err != nil {
		return err
	}
	sd, err := privateSD(true)
	if err != nil {
		return err
	}
	p, err := windows.UTF16PtrFromString(dir)
	if err != nil {
		return err
	}
	sa := &windows.SecurityAttributes{SecurityDescriptor: sd}
	sa.Length = uint32(unsafe.Sizeof(*sa))
	err = windows.CreateDirectory(p, sa)
	if errors.Is(err, windows.ERROR_ALREADY_EXISTS) {
		if fi, serr := os.Stat(dir); serr == nil && fi.IsDir() {
			return nil
		}
	}
	if err != nil {
		return &os.PathError{Op: "mkdir", Path: dir, Err: err}
	}
	return nil
}

// createPrivate opens path for writing, creating it private; excl refuses
// a file that exists, else one that does is truncated and keeps its DACL,
// as os.WriteFile keeps a mode.
func createPrivate(path string, excl bool) (*os.File, error) {
	sd, err := privateSD(false)
	if err != nil {
		return nil, err
	}
	p, err := windows.UTF16PtrFromString(path)
	if err != nil {
		return nil, err
	}
	sa := &windows.SecurityAttributes{SecurityDescriptor: sd}
	sa.Length = uint32(unsafe.Sizeof(*sa))
	disposition := uint32(windows.CREATE_ALWAYS)
	if excl {
		disposition = windows.CREATE_NEW
	}
	h, err := windows.CreateFile(p, windows.GENERIC_WRITE, windows.FILE_SHARE_READ|windows.FILE_SHARE_WRITE, sa, disposition, windows.FILE_ATTRIBUTE_NORMAL, 0)
	if err != nil {
		return nil, &os.PathError{Op: "open", Path: path, Err: err}
	}
	return os.NewFile(uintptr(h), path), nil
}

// setPrivate gives the file at path, or the directory, a private DACL.
func setPrivate(path string, dir bool) error {
	sd, err := privateSD(dir)
	if err != nil {
		return err
	}
	dacl, _, err := sd.DACL()
	if err != nil {
		return err
	}
	if err := windows.SetNamedSecurityInfo(path, windows.SE_FILE_OBJECT,
		windows.DACL_SECURITY_INFORMATION|windows.PROTECTED_DACL_SECURITY_INFORMATION, nil, nil, dacl, nil); err != nil {
		return &os.PathError{Op: "set the DACL of", Path: path, Err: err}
	}
	return nil
}

// exposure says who besides the user, SYSTEM and the Administrators may
// read or change the open file f, "" when no one may, and the command that
// makes it private.
func exposure(f *os.File) (who, fix string, err error) {
	sd, err := windows.GetSecurityInfo(windows.Handle(f.Fd()), windows.SE_FILE_OBJECT,
		windows.OWNER_SECURITY_INFORMATION|windows.DACL_SECURITY_INFORMATION)
	if err != nil {
		return "", "", &os.PathError{Op: "read the security of", Path: f.Name(), Err: err}
	}
	fr, err := others(sd)
	if err != nil || fr.none() {
		return "", "", err
	}
	fix, err = fr.fix(f.Name())
	return fr.String(), fix, err
}

// privateConfigDir makes dir, the config's, private where it may: own is
// whether it is the CLI's own directory, which is made private if the user
// may change its DACL and owner. Otherwise it is left as it is, and seen
// says who may list it, "" when no one but the user, SYSTEM and the
// Administrators may. Who may change them stands in for the owner Unix
// asks for: an elevated administrator's files are the Administrators
// group's.
func privateConfigDir(dir string, own bool) (seen string, err error) {
	if err := mkdirPrivate(dir); err != nil {
		return "", err
	}
	sd, err := windows.GetNamedSecurityInfo(dir, windows.SE_FILE_OBJECT,
		windows.OWNER_SECURITY_INFORMATION|windows.DACL_SECURITY_INFORMATION)
	if err != nil {
		return "", &os.PathError{Op: "read the security of", Path: dir, Err: err}
	}
	fr, err := others(sd)
	if err != nil || fr.none() {
		return "", err
	}
	if own {
		err := takeBack(dir, fr.owner != nil)
		if !errors.Is(err, windows.ERROR_ACCESS_DENIED) && !errors.Is(err, windows.ERROR_INVALID_OWNER) {
			return "", err
		}
	}
	return "may be reached by " + fr.String(), nil
}

// takeBack makes the directory dir private, and with owner makes the user
// its owner first.
func takeBack(dir string, owner bool) error {
	if owner {
		me, err := currentUser()
		if err != nil {
			return err
		}
		if err := windows.SetNamedSecurityInfo(dir, windows.SE_FILE_OBJECT, windows.OWNER_SECURITY_INFORMATION, me, nil, nil, nil); err != nil {
			return &os.PathError{Op: "set the owner of", Path: dir, Err: err}
		}
	}
	return setPrivate(dir, true)
}

// fileDeleteChild is a directory's right to delete what it holds, whatever
// their own DACLs say.
const fileDeleteChild = 0x40

// exposing is the access that makes a file not private: reading or
// changing its data, deleting it (and so replacing it), or changing its
// DACL or owner, and the generic rights that include them. On a directory
// the same bits list, add and delete what it holds.
const exposing = windows.GENERIC_ALL | windows.GENERIC_READ | windows.GENERIC_WRITE |
	windows.FILE_READ_DATA | windows.FILE_WRITE_DATA | windows.FILE_APPEND_DATA |
	windows.DELETE | fileDeleteChild | windows.WRITE_DAC | windows.WRITE_OWNER

// The ACE types that grant access. Allowed and allowed-callback ACEs share
// a layout, the SID after the mask; the rest name objects first, and are
// not read but counted as granting access to anyone.
const (
	aceAllowedCallback       = 0x9
	aceAllowedCompound       = 0x4
	aceAllowedObject         = 0x5
	aceAllowedCallbackObject = 0xB
)

// trustedInstaller is the service that owns Windows' own files, which
// Win32-OpenSSH also lets own and reach a private key.
const trustedInstaller = "S-1-5-80-956008885-3418522649-1831038044-1853292631-2271478464"

// trusted says whether sid may own or reach a private file: the user,
// SYSTEM, the Administrators or TrustedInstaller, as Win32-OpenSSH has it
// (check_secure_file_permission in its w32-sshfileperm.c).
func trusted(sid, me *windows.SID) bool {
	return sid.Equals(me) || sid.IsWellKnown(windows.WinLocalSystemSid) ||
		sid.IsWellKnown(windows.WinBuiltinAdministratorsSid) || sid.String() == trustedInstaller
}

// foreign is what in a security descriptor lets someone untrusted at the
// file: an owner, who may always change the DACL whatever it says;
// principals the DACL grants exposing access; or a DACL hangar cannot
// tell, absent or holding an ACE it does not read.
type foreign struct {
	owner   *windows.SID
	granted []*windows.SID
	opaque  string
}

func (f foreign) none() bool { return f.owner == nil && len(f.granted) == 0 && f.opaque == "" }

func (f foreign) String() string {
	var parts []string
	if f.owner != nil {
		parts = append(parts, "its owner, "+accountOf(f.owner))
	}
	for _, sid := range f.granted {
		parts = append(parts, accountOf(sid))
	}
	if f.opaque != "" {
		parts = append(parts, f.opaque)
	}
	return strings.Join(parts, ", ")
}

// fix is what makes the file at path private: icacls, given SIDs so that
// it reads the same in cmd and PowerShell, and whatever the names.
func (f foreign) fix(path string) (string, error) {
	me, err := currentUser()
	if err != nil {
		return "", err
	}
	var steps []string
	if f.owner != nil {
		steps = append(steps, fmt.Sprintf(`icacls "%s" /setowner *%s`, path, me))
	}
	dacl := fmt.Sprintf(`icacls "%s" /inheritance:r /grant:r *%s:F`, path, me)
	if len(f.granted) > 0 {
		dacl += " /remove"
		for _, sid := range f.granted {
			dacl += " *" + sid.String()
		}
	}
	steps = append(steps, dacl)
	fix := strings.Join(steps, ", then ")
	if f.opaque != "" && f.opaque != noDACL {
		fix += "; or copy it to a new file, which hangar makes private, and delete it"
	}
	return fix, nil
}

const noDACL = "everyone, as it has no DACL"

// others is what in sd lets anyone untrusted at the file.
func others(sd *windows.SECURITY_DESCRIPTOR) (foreign, error) {
	var f foreign
	me, err := currentUser()
	if err != nil {
		return f, err
	}
	owner, _, err := sd.Owner()
	if err != nil {
		return f, err
	}
	if owner != nil && !trusted(owner, me) {
		f.owner = owner
	}
	dacl, _, err := sd.DACL()
	if errors.Is(err, windows.ERROR_OBJECT_NOT_FOUND) {
		dacl, err = nil, nil
	}
	if err != nil {
		return f, err
	}
	if dacl == nil {
		f.opaque = noDACL
		return f, nil
	}
	for i := range uint32(dacl.AceCount) {
		var ace *windows.ACCESS_ALLOWED_ACE
		if err := windows.GetAce(dacl, i, &ace); err != nil {
			return f, err
		}
		if ace.Header.AceFlags&windows.INHERIT_ONLY_ACE != 0 {
			continue // for what the directory holds, not the directory
		}
		switch ace.Header.AceType {
		case windows.ACCESS_ALLOWED_ACE_TYPE, aceAllowedCallback:
		case aceAllowedCompound, aceAllowedObject, aceAllowedCallbackObject:
			f.opaque = fmt.Sprintf("whoever an ACE of type %d names, which hangar does not read", ace.Header.AceType)
			continue
		default:
			continue // denies, or audits
		}
		if ace.Mask&exposing == 0 {
			continue
		}
		sid := (*windows.SID)(unsafe.Pointer(&ace.SidStart))
		if trusted(sid, me) || slices.ContainsFunc(f.granted, sid.Equals) {
			continue
		}
		copied, err := sid.Copy()
		if err != nil {
			return f, err
		}
		f.granted = append(f.granted, copied)
	}
	return f, nil
}

// accountOf names sid as DOMAIN\name, or as its string when it names no
// account.
func accountOf(sid *windows.SID) string {
	account, domain, _, err := sid.LookupAccount("")
	switch {
	case err != nil:
		return sid.String()
	case domain == "":
		return account
	}
	return domain + `\` + account
}
