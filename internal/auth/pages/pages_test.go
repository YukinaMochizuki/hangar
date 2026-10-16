package pages_test

import (
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/YukinaMochizuki/hangar/internal/auth/pages"
)

// TestReferrerPolicyKeepsTheOrigin: every page sends its referrer to its own
// origin only. Under no-referrer, which a <meta> would also impose, a
// browser posts a page's form with Origin null, and the device approval's
// same-origin check refuses it.
func TestReferrerPolicyKeepsTheOrigin(t *testing.T) {
	for _, p := range pages.All {
		rec := httptest.NewRecorder()
		pages.Render(rec, 200, p, pages.Data{})
		if got := rec.Header().Get("Referrer-Policy"); got != "same-origin" {
			t.Errorf("%s: Referrer-Policy %q", p, got)
		}
		body := rec.Body.String()
		if strings.Contains(body, "no-referrer") || !strings.Contains(body, `<meta name="referrer" content="same-origin">`) {
			t.Errorf("%s: the page's own referrer policy is not same-origin", p)
		}
	}
}

// TestNoReferrerSendsNone: a page asked for no referrer says so in its
// header and its own meta, which would otherwise override the header.
func TestNoReferrerSendsNone(t *testing.T) {
	rec := httptest.NewRecorder()
	pages.Render(rec, 400, pages.EndpointFailed, pages.Data{NoReferrer: true})
	if got := rec.Header().Get("Referrer-Policy"); got != "no-referrer" {
		t.Errorf("Referrer-Policy %q", got)
	}
	if body := rec.Body.String(); !strings.Contains(body, `<meta name="referrer" content="no-referrer">`) || strings.Contains(body, "same-origin") {
		t.Errorf("the page's own referrer policy is not no-referrer")
	}
}
