package secrets

import (
	"bytes"
	"fmt"
)

// TailscaleAuthKey is the one key of a tailscale-auth secret's data: the
// auth key a container joins its tailnet with
// (docs/decisions/container-tailscale.md).
const TailscaleAuthKey = "authkey"

// maxTailscaleAuthKey bounds an auth key; Tailscale's are far shorter,
// an OAuth client secret with its options included.
const maxTailscaleAuthKey = 1024

// validateTailscaleAuth checks a tailscale-auth secret's data: one key,
// authkey, of printable ASCII with no space, but around it, which the
// tailscale CLI trims as it reads a key from a file. Its errors never quote
// the key.
func validateTailscaleAuth(data map[string][]byte) error {
	key, ok := data[TailscaleAuthKey]
	key = bytes.TrimSpace(key)
	if len(data) != 1 || !ok {
		return fmt.Errorf("a tailscale-auth secret's data holds one key, %q", TailscaleAuthKey)
	}
	if len(key) == 0 || len(key) > maxTailscaleAuthKey {
		return fmt.Errorf("%s is %d bytes; it must be 1 to %d", TailscaleAuthKey, len(key), maxTailscaleAuthKey)
	}
	for _, c := range key {
		if c <= ' ' || c > '~' {
			return fmt.Errorf("%s must be printable ASCII with no space or line break, as Tailscale gives auth keys", TailscaleAuthKey)
		}
	}
	return nil
}

// TailscaleAuthKeyOf is the auth key of a valid tailscale-auth secret's
// data, if it holds one.
func TailscaleAuthKeyOf(data map[string][]byte) (string, bool) {
	if validateTailscaleAuth(data) != nil {
		return "", false
	}
	return string(bytes.TrimSpace(data[TailscaleAuthKey])), true
}
