package cli_test

import (
	"bytes"
	"context"
	"encoding/base64"
	"path/filepath"
	"strings"
	"testing"

	"github.com/YukinaMochizuki/hangar/internal/cli"
)

// TestLoginNeverPrintsTheToken: `cloudflared access login` prints the
// Access token it fetched; hangar login drops it, says who logged in, and
// still passes through the URL cloudflared asks the user to open.
func TestLoginNeverPrintsTheToken(t *testing.T) {
	const url = "https://api.example.test"
	payload := base64.RawURLEncoding.EncodeToString([]byte(`{"email":"alice@example.com","exp":4102444800}`))
	token := "eyJhbGciOiJSUzI1NiJ9." + payload + ".c2lnbmF0dXJlLXZhbHVl"
	browser := "Please open the following URL and log in with your Cloudflare account:\n\n" +
		"https://team.cloudflareaccess.com/cdn-cgi/access/cli?aud=x&redirect_url=y\n\n" +
		"Leave cloudflared running to download the token automatically.\n"
	cloudflared := browser + "Successfully fetched your token:\n\n" + token + "\n\n"

	config := filepath.Join(t.TempDir(), "config.yaml")
	run := func(args ...string) (int, string, string) {
		var out, errb bytes.Buffer
		code := cli.Run(context.Background(), args, cli.IO{
			In: strings.NewReader(""), Out: &out, Err: &errb,
			Env: func(k string) string {
				if k == "HANGAR_CONFIG" {
					return config
				}
				return ""
			},
		}, cli.Options{RunCloudflared: func(context.Context, ...string) (string, error) {
			return cloudflared, nil
		}})
		return code, out.String(), errb.String()
	}
	if code, _, errs := run("config", "set-context", "prod", "--endpoint", url); code != 0 {
		t.Fatal(errs)
	}
	code, out, errs := run("login")
	if code != 0 {
		t.Fatalf("login exit %d: %s", code, errs)
	}
	for _, leak := range []string{token, payload, "Successfully fetched your token"} {
		if strings.Contains(out+errs, leak) {
			t.Errorf("login printed %q:\n%s%s", leak, out, errs)
		}
	}
	if !strings.Contains(out, "cdn-cgi/access/cli?aud=x") || !strings.Contains(out, "Leave cloudflared running") {
		t.Errorf("login dropped cloudflared's instructions:\n%s", out)
	}
	if !strings.HasSuffix(out, "logged in to "+url+" as alice@example.com\n") {
		t.Errorf("login did not say who logged in:\n%s", out)
	}
}
