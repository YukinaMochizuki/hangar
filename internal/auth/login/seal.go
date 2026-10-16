package login

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"errors"
)

// A sign-in in progress lives in the browser that started it, in
// loginCookie, sealed with AES-256-GCM under a key the apiserver makes at
// start and holds only in memory. Nothing else needs the key: the cookie
// lives ten minutes and comes back only to this apiserver. Restarting it
// makes a new key, which cancels the sign-ins in progress; each person
// starts again. That is the rotation. hangar-edge seals its endpoint
// handoffs in progress the same way, with a Sealer of its own.

// signInPurpose binds a sealed sign-in to this use, so nothing else sealed
// with the key could pass for it.
const signInPurpose = "hangar sign-in v1"

// maxSealed bounds the cookie, which browsers cap at about 4 KB.
const maxSealed = 3800

// errNotSealed is any value a Sealer did not seal.
var errNotSealed = errors.New("not a sealed value")

// inFlight is a sign-in in progress, as sealed into loginCookie.
type inFlight struct {
	Pending pending `json:"p"`
	// State is what the provider must send back with the browser.
	State string `json:"s"`
	// Expires is when the sign-in ends, in Unix milliseconds.
	Expires int64 `json:"e"`
}

// Sealer seals values into cookies only it can open, under a random key it
// holds in memory for its life: values it sealed before a restart no longer
// open.
type Sealer struct {
	aead    cipher.AEAD
	purpose []byte
}

// NewSealer returns a Sealer with a fresh key, for one purpose: a value
// sealed for another purpose never opens.
func NewSealer(purpose string) (*Sealer, error) {
	key := make([]byte, 32)
	if _, err := rand.Read(key); err != nil {
		return nil, err
	}
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, err
	}
	aead, err := cipher.NewGCM(block)
	if err != nil {
		return nil, err
	}
	return &Sealer{aead: aead, purpose: []byte(purpose)}, nil
}

// Seal returns v, as JSON, sealed, as a cookie value.
func (s *Sealer) Seal(v any) (string, error) {
	plain, err := json.Marshal(v)
	if err != nil {
		return "", err
	}
	nonce := make([]byte, s.aead.NonceSize())
	if _, err := rand.Read(nonce); err != nil {
		return "", err
	}
	out := base64.RawURLEncoding.EncodeToString(s.aead.Seal(nonce, nonce, plain, s.purpose))
	if len(out) > maxSealed {
		return "", errors.New("the value does not fit in a cookie")
	}
	return out, nil
}

// Open reads into v what Seal sealed, or fails for anything this Sealer did
// not seal.
func (s *Sealer) Open(sealed string, v any) error {
	b, err := base64.RawURLEncoding.DecodeString(sealed)
	n := s.aead.NonceSize()
	if err != nil || len(b) < n {
		return errNotSealed
	}
	plain, err := s.aead.Open(nil, b[:n], b[n:], s.purpose)
	if err != nil {
		return errNotSealed
	}
	return json.Unmarshal(plain, v)
}

// seal returns a sign-in sealed, as a cookie value.
func (f *Flow) seal(v inFlight) (string, error) { return f.sealer.Seal(v) }

// open returns what seal sealed, or an error for anything this apiserver
// did not seal since it started.
func (f *Flow) open(s string) (inFlight, error) {
	var v inFlight
	err := f.sealer.Open(s, &v)
	return v, err
}
