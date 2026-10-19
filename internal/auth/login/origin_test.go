package login

import "testing"

// TestParseOriginTakesHTTPOnlyOnALoopbackIP: plain http is for a loopback
// IP during development; localhost, which a hosts file can point anywhere,
// needs https like any other name.
func TestParseOriginTakesHTTPOnlyOnALoopbackIP(t *testing.T) {
	for in, want := range map[string]string{
		"https://console.example.com": "https://console.example.com",
		"https://localhost:5173":      "https://localhost:5173",
		"http://127.0.0.1:5173/":      "http://127.0.0.1:5173",
		"http://127.9.9.9:5173":       "http://127.9.9.9:5173",
		"http://[::1]:5173":           "http://[::1]:5173",
	} {
		got, err := ParseOrigin(in)
		if err != nil || got.String() != want {
			t.Errorf("%s: %v, %v; want %s", in, got, err, want)
		}
	}
	for _, in := range []string{"http://localhost:5173", "http://LocalHost:5173", "http://localhost.:5173", "http://console.example.com", "http://10.0.0.1:5173", "http://0.0.0.0:5173"} {
		if got, err := ParseOrigin(in); err == nil {
			t.Errorf("%s: took %v", in, got)
		}
	}
}
