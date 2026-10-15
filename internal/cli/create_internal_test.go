package cli

import "testing"

func TestSlug(t *testing.T) {
	for in, want := range map[string]string{
		"My Demo":                       "my-demo",
		"  --Alice's   Lab!  ":          "alice-s-lab",
		"Ünïcode café":                  "unicode-cafe",
		"Ｗｉｄｅ":                          "wide",
		"我的":                            "",
		"a very long display name here": "a-very-long-display",
		"abcdefghijklmnopqrst-uvw":      "abcdefghijklmnopqrst",
		"abcdefghijklmnopqrs uvw":       "abcdefghijklmnopqrs",
		"2024 plans":                    "2024-plans",
	} {
		if got := slug(in); got != want {
			t.Errorf("slug(%q) = %q, want %q", in, got, want)
		}
		if got := slug(in); len(got) > maxSlug {
			t.Errorf("slug(%q) is %d characters", in, len(got))
		}
	}
}
