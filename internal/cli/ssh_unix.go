//go:build !windows

package cli

import "strings"

// proxyCommand is argv as a ProxyCommand, which ssh runs with sh -c: each
// word quoted for sh.
func proxyCommand(argv []string) string {
	quoted := make([]string, len(argv))
	for i, s := range argv {
		quoted[i] = shellQuote(s)
	}
	return strings.Join(quoted, " ")
}

// shellQuote quotes s for sh.
func shellQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}
