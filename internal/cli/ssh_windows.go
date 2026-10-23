package cli

import (
	"strings"
	"syscall"
)

// proxyCommand is argv as a ProxyCommand. Win32-OpenSSH runs one with no
// shell: the command line is the ProxyCommand as it is, given to
// CreateProcess once it begins with a quote (build_commandline_string in
// its contrib/win32/win32compat/misc.c). So the program is always quoted,
// and the rest quoted as the program's runtime splits its command line,
// which for hangar is Go's.
func proxyCommand(argv []string) string {
	quoted := make([]string, len(argv))
	for i, s := range argv {
		quoted[i] = syscall.EscapeArg(s)
	}
	if !strings.HasPrefix(quoted[0], `"`) {
		// A path holds no quote, so quoting it needs no escapes.
		quoted[0] = `"` + argv[0] + `"`
	}
	return strings.Join(quoted, " ")
}
