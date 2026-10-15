// Package pages renders every page a person sees during sign-in, from the
// apiserver and from the CLI's loopback listener. They are deliberately
// plain placeholders: the templates under templates/ are the one place the
// visual design replaces, and nothing else in the sign-in code writes HTML.
package pages

import (
	"bytes"
	"embed"
	"html/template"
	"net/http"
)

//go:embed templates/*.html
var files embed.FS

// Page names a template under templates/, without its extension.
type Page string

// Every page there is.
const (
	// SignInFailed: a sign-in through the apiserver failed. Data: Reason.
	SignInFailed Page = "signin-failed"
	// NotInvited: the email signed in, but no one invited it. Data: Email.
	NotInvited Page = "not-invited"
	// CLISignedIn: the CLI's loopback got its code. Data: Endpoint, Email.
	CLISignedIn Page = "cli-signed-in"
	// CLIFailed: the CLI's loopback got an error. Data: Reason.
	CLIFailed Page = "cli-failed"
	// DeviceEnter asks for a device login's code. Data: UserCode, if known.
	DeviceEnter Page = "device-enter"
	// DeviceConfirm asks to approve a device login. Data: Email, UserCode.
	DeviceConfirm Page = "device-confirm"
	// DeviceApproved: the device login was approved.
	DeviceApproved Page = "device-approved"
	// DeviceDenied: the device login was denied.
	DeviceDenied Page = "device-denied"
	// DeviceUnknown: no device login has the code.
	DeviceUnknown Page = "device-unknown"
)

// All is every page, for tests and screenshots.
var All = []Page{SignInFailed, NotInvited, CLISignedIn, CLIFailed, DeviceEnter, DeviceConfirm, DeviceApproved, DeviceDenied, DeviceUnknown}

var templates = func() map[Page]*template.Template {
	m := map[Page]*template.Template{}
	for _, p := range All {
		m[p] = template.Must(template.ParseFS(files, "templates/layout.html", "templates/"+string(p)+".html"))
	}
	return m
}()

// Data is what a page shows; each page uses the fields its doc names.
type Data struct {
	Reason   string
	Email    string
	Endpoint string
	UserCode string
}

// Render writes the page with the status. The page may not be framed,
// load anything, or leak its URL, which can hold a one-time code, to
// another site. The referrer goes to its own origin only: under
// no-referrer a browser sends a form's POST with Origin null, which the
// device approval's exact-Origin check must refuse.
func Render(w http.ResponseWriter, status int, p Page, d Data) {
	var b bytes.Buffer
	if err := templates[p].ExecuteTemplate(&b, "layout", d); err != nil {
		http.Error(w, "could not render the page", http.StatusInternalServerError)
		return
	}
	h := w.Header()
	h.Set("Content-Type", "text/html; charset=utf-8")
	h.Set("Content-Security-Policy", "default-src 'none'; form-action 'self'; frame-ancestors 'none'; base-uri 'none'")
	h.Set("X-Frame-Options", "DENY")
	h.Set("Referrer-Policy", "same-origin")
	h.Set("Cache-Control", "no-store")
	h.Set("X-Content-Type-Options", "nosniff")
	w.WriteHeader(status)
	_, _ = w.Write(b.Bytes())
}
