package login

import (
	"context"
	"crypto/rand"
	"encoding/json"
	"errors"
	"math/big"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/YukinaMochizuki/hangar/internal/auth"
	"github.com/YukinaMochizuki/hangar/internal/auth/pages"
	"github.com/YukinaMochizuki/hangar/internal/auth/session"
	"github.com/YukinaMochizuki/hangar/internal/auth/token"
	"github.com/YukinaMochizuki/hangar/internal/store"
)

// The device login (RFC 8628) is Hangar's own, so the provider needs no
// client for devices: the CLI gets a device code to poll with and a short
// user code; the person opens the device page in any browser, signs in to
// the console there, and approves that code.

// userCodeAlphabet has no vowels, so a code spells no word, and no digits
// that read as letters.
const userCodeAlphabet = "BCDFGHJKLMNPQRSTVWXZ"

const userCodeLen = 8

// device is a device login, under its device code.
type device struct {
	UserCode string `json:"userCode"`
	// Status is pending, approved or denied.
	Status    string `json:"status"`
	Principal string `json:"principal,omitempty"`
	// Family is the login reserved for it when approved
	// (session.Manager.Reserve), so revoking the person's logins before
	// the CLI polls ends it too.
	Family string `json:"family,omitempty"`
	// LastPoll is when the CLI last polled, in Unix milliseconds.
	LastPoll int64 `json:"lastPoll,omitempty"`
}

// userCodeRef points from a user code to its device login.
type userCodeRef struct {
	Device string `json:"device"`
}

// Device is a device login the CLI has started.
type Device struct {
	// DeviceCode is the CLI's secret to poll with.
	DeviceCode token.Secret
	// UserCode is what the person types, as XXXX-XXXX.
	UserCode string
	// VerificationURI is the page to type it in; VerificationURIComplete
	// has it filled in.
	VerificationURI, VerificationURIComplete string
	ExpiresAt                                time.Time
	Interval                                 time.Duration
}

// StartDevice starts a device login.
func (f *Flow) StartDevice(ctx context.Context) (Device, error) {
	if err := f.roomForOneMore(ctx, deviceKey); err != nil {
		return Device{}, err
	}
	code := token.Secret(token.Random())
	devKey := deviceKey + code.Hash()
	if err := f.put(ctx, devKey, device{Status: "pending"}, DeviceTTL); err != nil {
		return Device{}, err
	}
	for range 5 {
		user, err := newUserCode()
		if err != nil {
			return Device{}, err
		}
		err = f.put(ctx, "user:"+user, userCodeRef{Device: devKey}, DeviceTTL)
		if errors.Is(err, store.ErrExists) {
			continue
		}
		if err != nil {
			return Device{}, err
		}
		var d device
		r, err := f.get(ctx, devKey, &d)
		if err != nil {
			return Device{}, err
		}
		d.UserCode = user
		b, _ := json.Marshal(d)
		if err := f.cfg.Store.SwapAuthRequest(ctx, devKey, r.Data, b); err != nil {
			return Device{}, err
		}
		page := f.Origin() + PathDevice
		return Device{
			DeviceCode: code, UserCode: formatUserCode(user),
			VerificationURI: page, VerificationURIComplete: page + "?" + url.Values{"user_code": {formatUserCode(user)}}.Encode(),
			ExpiresAt: f.cfg.Sessions.Now().Add(DeviceTTL), Interval: f.cfg.DeviceInterval,
		}, nil
	}
	return Device{}, errors.New("could not find a free user code")
}

// PollDevice answers a CLI polling its device login: ErrPending until the
// person decides, then the tokens once, or ErrDenied.
func (f *Flow) PollDevice(ctx context.Context, code token.Secret) (session.Tokens, error) {
	key := deviceKey + code.Hash()
	var d device
	r, err := f.get(ctx, key, &d)
	if errors.Is(err, store.ErrNotFound) {
		return session.Tokens{}, ErrExpired
	}
	if err != nil {
		return session.Tokens{}, err
	}
	now := f.cfg.Sessions.Now()
	switch d.Status {
	case "approved":
		if err := f.take(ctx, key, &d); errors.Is(err, store.ErrNotFound) {
			return session.Tokens{}, ErrExpired
		} else if err != nil {
			return session.Tokens{}, err
		}
		p, err := auth.ParsePrincipal(d.Principal)
		if err != nil {
			return session.Tokens{}, err
		}
		return f.startCLILogin(ctx, p, d.Family)
	case "denied":
		_, _ = f.cfg.Store.TakeAuthRequest(ctx, key)
		return session.Tokens{}, ErrDenied
	}
	if d.LastPoll != 0 && now.Sub(time.UnixMilli(d.LastPoll)) < f.cfg.DeviceInterval {
		return session.Tokens{}, ErrSlowDown
	}
	d.LastPoll = now.UnixMilli()
	b, _ := json.Marshal(d)
	// A conflict is a decision that just landed; the next poll sees it.
	if err := f.cfg.Store.SwapAuthRequest(ctx, key, r.Data, b); err != nil && !errors.Is(err, store.ErrConflict) && !errors.Is(err, store.ErrNotFound) {
		return session.Tokens{}, err
	}
	return session.Tokens{}, ErrPending
}

// devicePage asks a signed-in person for the code, then whether to approve
// it. Someone not signed in signs in first and comes back.
func (f *Flow) devicePage(w http.ResponseWriter, r *http.Request) {
	userCode := r.URL.Query().Get("user_code")
	p, sessionKey, ok := f.browserPrincipal(r)
	if !ok {
		back := PathDevice
		if userCode != "" {
			back += "?" + url.Values{"user_code": {userCode}}.Encode()
		}
		http.Redirect(w, r, PathLogin+"?"+url.Values{"return": {back}}.Encode(), http.StatusFound)
		return
	}
	if userCode == "" {
		pages.Render(w, http.StatusOK, pages.DeviceEnter, pages.Data{})
		return
	}
	if !f.attempts.allow(sessionKey, f.cfg.Sessions.Now(), codeAttempts) {
		f.fail(w, http.StatusTooManyRequests, "too many codes tried; wait a minute")
		return
	}
	var ref userCodeRef
	if _, err := f.get(r.Context(), "user:"+normalizeUserCode(userCode), &ref); err != nil {
		if errors.Is(err, store.ErrNotFound) {
			pages.Render(w, http.StatusNotFound, pages.DeviceUnknown, pages.Data{})
			return
		}
		f.internal(w, err)
		return
	}
	pages.Render(w, http.StatusOK, pages.DeviceConfirm, pages.Data{Email: p.ID, UserCode: formatUserCode(normalizeUserCode(userCode))})
}

// deviceDecision records the signed-in person's approval or denial. It is a
// form post, so only one from the console's own origin counts, and the user
// code is spent by the first decision.
func (f *Flow) deviceDecision(w http.ResponseWriter, r *http.Request) {
	if !session.SameOrigin(r.Header, f.Origin()) {
		f.fail(w, http.StatusForbidden, "the approval did not come from this site")
		return
	}
	p, sessionKey, ok := f.browserPrincipal(r)
	if !ok {
		f.fail(w, http.StatusUnauthorized, "you are not signed in")
		return
	}
	if !f.attempts.allow(sessionKey, f.cfg.Sessions.Now(), codeAttempts) {
		f.fail(w, http.StatusTooManyRequests, "too many codes tried; wait a minute")
		return
	}
	approve := r.PostFormValue("decision") == "approve"
	if approve {
		// The invitation may have gone since this browser signed in.
		admitted, err := f.cfg.Admit(r.Context(), p)
		if err != nil {
			f.internal(w, err)
			return
		}
		if !admitted {
			pages.Render(w, http.StatusForbidden, pages.NotInvited, pages.Data{Email: p.ID})
			return
		}
	}
	var ref userCodeRef
	if err := f.take(r.Context(), "user:"+normalizeUserCode(r.PostFormValue("user_code")), &ref); err != nil {
		if errors.Is(err, store.ErrNotFound) {
			pages.Render(w, http.StatusNotFound, pages.DeviceUnknown, pages.Data{})
			return
		}
		f.internal(w, err)
		return
	}
	var d device
	rec, err := f.get(r.Context(), ref.Device, &d)
	if errors.Is(err, store.ErrNotFound) || (err == nil && d.Status != "pending") {
		pages.Render(w, http.StatusNotFound, pages.DeviceUnknown, pages.Data{})
		return
	}
	if err != nil {
		f.internal(w, err)
		return
	}
	d.Status = "denied"
	if approve {
		family, err := f.cfg.Sessions.Reserve(r.Context(), p, DeviceTTL+time.Minute)
		if err != nil {
			f.internal(w, err)
			return
		}
		d.Status, d.Principal, d.Family = "approved", p.String(), family
	}
	b, _ := json.Marshal(d)
	for {
		err = f.cfg.Store.SwapAuthRequest(r.Context(), ref.Device, rec.Data, b)
		if !errors.Is(err, store.ErrConflict) {
			break
		}
		// A poll moved its time; decide again on what it wrote.
		var cur device
		if rec, err = f.get(r.Context(), ref.Device, &cur); err != nil {
			break
		}
		cur.Status, cur.Principal, cur.Family = d.Status, d.Principal, d.Family
		b, _ = json.Marshal(cur)
	}
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			pages.Render(w, http.StatusNotFound, pages.DeviceUnknown, pages.Data{})
			return
		}
		f.internal(w, err)
		return
	}
	if !approve {
		pages.Render(w, http.StatusOK, pages.DeviceDenied, pages.Data{})
		return
	}
	f.cfg.Log.Info("approved a device login", "principal", p.String(), "client_ip", clientIP(r.Context()))
	pages.Render(w, http.StatusOK, pages.DeviceApproved, pages.Data{})
}

// browserPrincipal is who the request's session cookie signs in, if anyone,
// and a key for that session that is not its secret.
func (f *Flow) browserPrincipal(r *http.Request) (auth.Principal, string, bool) {
	s, ok := session.SessionFromHeader(r.Header)
	if !ok {
		return auth.Principal{}, "", false
	}
	p, err := f.cfg.Sessions.Lookup(r.Context(), s, session.TypeSession)
	return p, s.Hash(), err == nil
}

func newUserCode() (string, error) {
	var b strings.Builder
	n := big.NewInt(int64(len(userCodeAlphabet)))
	for range userCodeLen {
		i, err := rand.Int(rand.Reader, n)
		if err != nil {
			return "", err
		}
		b.WriteByte(userCodeAlphabet[i.Int64()])
	}
	return b.String(), nil
}

// normalizeUserCode reads a code as typed: any case, with or without its
// dash and spaces.
func normalizeUserCode(s string) string {
	return strings.Map(func(r rune) rune {
		if r == '-' || r == ' ' {
			return -1
		}
		return r
	}, strings.ToUpper(s))
}

func formatUserCode(s string) string {
	if len(s) != userCodeLen {
		return s
	}
	return s[:4] + "-" + s[4:]
}
