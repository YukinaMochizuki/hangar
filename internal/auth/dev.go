package auth

import (
	"context"
	"net/http"
	"strings"
)

// DevUserHeader names the header dev mode reads the caller's email from.
const DevUserHeader = "X-Hangar-Dev-User"

// Dev trusts DevUserHeader as the caller's email. It exists for local
// development and tests; the apiserver only enables it on a loopback address.
type Dev struct{}

func (Dev) Authenticate(_ context.Context, header http.Header) (Principal, bool, error) {
	email := strings.ToLower(strings.TrimSpace(header.Get(DevUserHeader)))
	if email == "" {
		return Principal{}, false, nil
	}
	return Principal{Type: User, ID: email}, true, nil
}
