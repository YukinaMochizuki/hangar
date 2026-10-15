package login

import "testing"

// TestSafeReturn: only a clean path on this origin comes back; anything a
// browser could read as another host is "/".
func TestSafeReturn(t *testing.T) {
	for in, want := range map[string]string{
		"":                                 "/",
		"/":                                "/",
		"/console/home":                    "/console/home",
		"/auth/device?user_code=BCDF-GHJK": "/auth/device?user_code=BCDF-GHJK",
		"/a%20b":                           "/a%20b",
		"//evil.example":                   "/",
		"///evil.example":                  "/",
		"/\\evil.example":                  "/",
		"/\t/evil.example":                 "/",
		"/\n/evil.example":                 "/",
		"/\r/evil.example":                 "/",
		"/\x7f/evil.example":               "/",
		"/%09/evil.example":                "/",
		"/%0a/evil.example":                "/",
		"/%0D/evil.example":                "/",
		"/%5Cevil.example":                 "/",
		"/%2F/evil.example":                "/",
		"\t//evil.example":                 "/",
		"https://evil.example/":            "/",
		"javascript:alert(1)":              "/",
		"evil.example":                     "/",
		"/ok\\":                            "/",
	} {
		if got := safeReturn(in); got != want {
			t.Errorf("safeReturn(%q) = %q, want %q", in, got, want)
		}
	}
}
