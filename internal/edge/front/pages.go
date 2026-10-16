package front

import (
	"net/http"
	"strconv"
	"time"
)

// Every page the front door itself answers with, in one place. They are
// deliberately plain placeholders: what a person sees in a browser is
// designed with the owner, separately, and these are not that design. The
// endpoints' own pages are in internal/edge.
const (
	pageNoSuchHost      = "no such host"
	pageMisdirected     = "misdirected request"
	pageTooManyRequests = "too many requests; try again shortly"
	pageBodyTooLarge    = "request body too large"
	pageAPIDown         = "the API server did not answer"
	pageNoConsole       = "the console is not installed"
	pageNotFound        = "not found"
	pageMethod          = "method not allowed"
)

// page answers with status and text, as plain text that is never sniffed
// into anything else.
func page(w http.ResponseWriter, status int, text string) {
	http.Error(w, text, status)
}

// tooMany answers a request over its client's limits, saying when to try
// again.
func tooMany(w http.ResponseWriter, after time.Duration) {
	secs := int(after / time.Second)
	if after%time.Second != 0 {
		secs++
	}
	w.Header().Set("Retry-After", strconv.Itoa(max(secs, 1)))
	page(w, http.StatusTooManyRequests, pageTooManyRequests)
}

// redirect sends a plain-HTTP request to the same URL over HTTPS, with no
// body.
func redirect(w http.ResponseWriter, host, requestURI string) {
	w.Header().Set("Location", "https://"+host+requestURI)
	w.WriteHeader(http.StatusPermanentRedirect)
}
