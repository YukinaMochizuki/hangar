package tailnet

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"strings"

	guestv1 "github.com/YukinaMochizuki/hangar/gen/go/hangar/guest/v1"
)

// currentProfile is the key of tailscaled's state file naming the login
// profile in use, set once the node has logged in (ipn.CurrentProfileStateKey
// in tailscale).
const currentProfile = "_current-profile"

// HasNodeState reports whether the state file holds a node that has logged
// in, so the guest comes up as that node and never uses an auth key again.
// tailscaled writes the file at its first start, before any login, so its
// existence alone says nothing. A file that cannot be read is no state.
func HasNodeState(stateFile string) (bool, error) {
	b, err := os.ReadFile(stateFile)
	if errors.Is(err, os.ErrNotExist) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	var state map[string][]byte
	if err := json.Unmarshal(b, &state); err != nil {
		return false, fmt.Errorf("%s is not tailscaled's state", stateFile)
	}
	return len(state[currentProfile]) > 0, nil
}

// Config is how the guest joins.
type Config struct {
	Hostname     string
	AcceptRoutes bool
}

// keyFD is the descriptor the CLI reads the auth key from: a pipe, so the
// key is on no disk and in no argument list.
const keyFD = 3

// UpArgs is the CLI's command line that brings the node up as cfg says,
// with the auth key from keyFD when withKey. --reset makes the flags the
// whole of the node's settings; tailscaled never touches resolv.conf.
func UpArgs(cfg Config, withKey bool) []string {
	args := []string{"up", "--reset", "--hostname=" + cfg.Hostname, "--accept-dns=false", "--timeout=" + upTimeout.String()}
	if cfg.AcceptRoutes {
		args = append(args, "--accept-routes")
	}
	if withKey {
		args = append(args, fmt.Sprintf("--auth-key=file:/proc/self/fd/%d", keyFD))
	}
	return args
}

// rejections are what the CLI says when the control server refuses an auth
// key.
var rejections = []string{"invalid key", "key expired", "key has expired", "not valid", "revoked", "unable to validate"}

// maxMessage bounds a message taken from tailscale's output.
const maxMessage = 512

// upFailed is the state and message of a failed up whose combined output
// was out. The key, if the CLI ever echoed it, is cut out.
func upFailed(out, key string, withKey bool) (guestv1.TailnetState, string) {
	msg := lastLine(out)
	if key != "" {
		msg = strings.ReplaceAll(msg, key, "[auth key]")
	}
	if len(msg) > maxMessage {
		msg = msg[:maxMessage]
	}
	if withKey {
		low := strings.ToLower(msg)
		for _, r := range rejections {
			if strings.Contains(low, r) {
				return guestv1.TailnetState_TAILNET_STATE_KEY_REJECTED, "the tailnet refused the auth key: " + msg
			}
		}
	}
	if msg == "" {
		msg = "tailscale up failed, saying nothing"
	}
	return guestv1.TailnetState_TAILNET_STATE_FAILED, "tailscale up: " + msg
}

func lastLine(s string) string {
	lines := strings.Split(strings.TrimSpace(s), "\n")
	return strings.TrimSpace(lines[len(lines)-1])
}

// backend is what tailscaled's LocalAPI says of the node.
type backend struct {
	// State is ipn.State's name: NoState, NeedsLogin, NeedsMachineAuth,
	// Stopped, Starting or Running.
	State string
	// DNSName is the node's MagicDNS name, such as box.tail1234.ts.net.
	DNSName string
	IPs     []string
	// Suffix is the tailnet's MagicDNS suffix, such as tail1234.ts.net.
	Suffix string
}

// stateOf is the state the guest reports once tailscaled says b, the
// guest having node state or not.
func stateOf(b backend, hasState bool) (guestv1.TailnetState, string) {
	switch b.State {
	case "Running":
		msg := "joined"
		if b.DNSName != "" {
			msg += " as " + strings.TrimSuffix(b.DNSName, ".")
		}
		if len(b.IPs) > 0 {
			msg += " (" + strings.Join(b.IPs, ", ") + ")"
		}
		return guestv1.TailnetState_TAILNET_STATE_JOINED, msg
	case "NeedsLogin":
		if hasState {
			return guestv1.TailnetState_TAILNET_STATE_FAILED, "the node's login expired or it was removed from the tailnet; it does not join again with the auth key while its state is on the root disk (" + StateDir + ")"
		}
		return guestv1.TailnetState_TAILNET_STATE_NEEDS_KEY, "waiting for the auth key"
	case "NeedsMachineAuth":
		return guestv1.TailnetState_TAILNET_STATE_FAILED, "the node waits for a tailnet admin to approve it"
	case "Stopped":
		return guestv1.TailnetState_TAILNET_STATE_FAILED, "the node is down: tailscale down was run in the guest"
	}
	return guestv1.TailnetState_TAILNET_STATE_STARTING, "tailscaled is starting"
}
