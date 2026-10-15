package login

import (
	"encoding/base64"
	"testing"
)

// TestSeal: a sealed sign-in opens to what was sealed, and nothing else
// opens: not a changed byte, not what another apiserver (another key)
// sealed.
func TestSeal(t *testing.T) {
	newFlow := func() *Flow {
		s, err := newSealer()
		if err != nil {
			t.Fatal(err)
		}
		return &Flow{sealer: s}
	}
	f := newFlow()
	in := inFlight{Pending: pending{Nonce: "n", Verifier: "v", Return: "/x"}, State: "st", Expires: 42}
	s, err := f.seal(in)
	if err != nil {
		t.Fatal(err)
	}
	got, err := f.open(s)
	if err != nil || got != in {
		t.Fatalf("open: %+v, %v", got, err)
	}
	raw, err := base64.RawURLEncoding.DecodeString(s)
	if err != nil {
		t.Fatal(err)
	}
	for i := range raw {
		b := append([]byte(nil), raw...)
		b[i] ^= 1
		if _, err := f.open(base64.RawURLEncoding.EncodeToString(b)); err == nil {
			t.Fatalf("opened with byte %d changed", i)
		}
	}
	if _, err := newFlow().open(s); err == nil {
		t.Error("another key opened it")
	}
	for _, bad := range []string{"", "x", "not base64!"} {
		if _, err := f.open(bad); err == nil {
			t.Errorf("opened %q", bad)
		}
	}
}
