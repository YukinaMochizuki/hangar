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
// starts again. That is the rotation.

// sealAD binds a sealed value to this use, so nothing else sealed with the
// key could pass for it.
var sealAD = []byte("hangar sign-in v1")

// maxSealed bounds the cookie, which browsers cap at about 4 KB.
const maxSealed = 3800

// inFlight is a sign-in in progress, as sealed into loginCookie.
type inFlight struct {
	Pending pending `json:"p"`
	// State is what the provider must send back with the browser.
	State string `json:"s"`
	// Expires is when the sign-in ends, in Unix milliseconds.
	Expires int64 `json:"e"`
}

func newSealer() (cipher.AEAD, error) {
	key := make([]byte, 32)
	if _, err := rand.Read(key); err != nil {
		return nil, err
	}
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, err
	}
	return cipher.NewGCM(block)
}

// seal returns v sealed, as a cookie value.
func (f *Flow) seal(v inFlight) (string, error) {
	plain, err := json.Marshal(v)
	if err != nil {
		return "", err
	}
	nonce := make([]byte, f.sealer.NonceSize())
	if _, err := rand.Read(nonce); err != nil {
		return "", err
	}
	s := base64.RawURLEncoding.EncodeToString(f.sealer.Seal(nonce, nonce, plain, sealAD))
	if len(s) > maxSealed {
		return "", errors.New("the sign-in does not fit in a cookie")
	}
	return s, nil
}

// open returns what seal sealed, or an error for anything this apiserver
// did not seal since it started.
func (f *Flow) open(s string) (inFlight, error) {
	var v inFlight
	b, err := base64.RawURLEncoding.DecodeString(s)
	if err != nil || len(b) < f.sealer.NonceSize() {
		return v, errors.New("not a sealed sign-in")
	}
	n := f.sealer.NonceSize()
	plain, err := f.sealer.Open(nil, b[:n], b[n:], sealAD)
	if err != nil {
		return v, errors.New("not a sealed sign-in")
	}
	err = json.Unmarshal(plain, &v)
	return v, err
}
