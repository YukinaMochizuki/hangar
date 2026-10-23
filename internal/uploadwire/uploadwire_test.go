package uploadwire_test

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"connectrpc.com/connect"

	"github.com/YukinaMochizuki/hangar/internal/uploadwire"
)

func TestPathEscapesItsNames(t *testing.T) {
	if got := uploadwire.Path("web", "nixos"); got != "/v1/projects/web/customimages/nixos:upload" {
		t.Errorf("Path %q", got)
	}
	if got := uploadwire.Path("a/b", "c d"); got != "/v1/projects/a%2Fb/customimages/c%20d:upload" {
		t.Errorf("Path of names to escape %q", got)
	}
}

// TestContentRangeIsReadAsWritten: a range written reads back, and a
// header not of a range of bytes within the file is refused.
func TestContentRangeIsReadAsWritten(t *testing.T) {
	for _, tt := range []struct{ first, n, size int64 }{
		{0, 1, 1}, {0, 1000, 2500}, {2000, 500, 2500}, {9, 1, 10}, {1<<40 - 1, 1, 1 << 40},
	} {
		h := uploadwire.ContentRange(tt.first, tt.n, tt.size)
		first, n, size, err := uploadwire.ParseContentRange(h)
		if err != nil || first != tt.first || n != tt.n || size != tt.size {
			t.Errorf("%q read as %d %d %d %v", h, first, n, size, err)
		}
	}
	if h := uploadwire.ContentRange(1000, 1000, 2500); h != "bytes 1000-1999/2500" {
		t.Errorf("ContentRange %q", h)
	}
	for _, h := range []string{
		"",
		"bytes 0-9/10 ",
		" bytes 0-9/10",
		"bytes=0-9/10",
		"bytes 0-9/*",
		"bytes */10",
		"bytes -1-9/10",
		"bytes 0--9/10",
		"items 0-9/10",
		"bytes 0-9",
		"bytes 5-4/10",                   // last before first
		"bytes 0-10/10",                  // past the file
		"bytes 10-10/10",                 // starting past it
		"bytes 0-0/0",                    // no file
		"bytes 0-1/99999999999999999999", // more digits than an int64 has
		"bytes 0-1/9999999999999999999",  // past an int64
		"bytes 9999999999999999999-0/1",  // past an int64
		"bytes 0x0-9/10",
	} {
		if first, n, size, err := uploadwire.ParseContentRange(h); err == nil {
			t.Errorf("%q read as %d %d %d, want it refused", h, first, n, size)
		}
	}
}

// TestRefusalsGoOverTheWireAsTheyAre: each refusal is written as its
// status and Connect code, the three that keep the upload where it is
// carrying where that is, which reads back as the same error.
func TestRefusalsGoOverTheWireAsTheyAre(t *testing.T) {
	for _, tt := range []struct {
		name       string
		err        error
		status     int
		code       string
		retryAfter string
		check      func(error) bool
	}{
		{"offset", &uploadwire.OffsetError{ReceivedBytes: 1000}, http.StatusConflict, "aborted", "", func(err error) bool {
			var e *uploadwire.OffsetError
			return errors.As(err, &e) && e.ReceivedBytes == 1000
		}},
		{"offset at 0", &uploadwire.OffsetError{ReceivedBytes: 0}, http.StatusConflict, "aborted", "", func(err error) bool {
			var e *uploadwire.OffsetError
			return errors.As(err, &e) && e.ReceivedBytes == 0
		}},
		{"busy", &uploadwire.BusyError{ReceivedBytes: 64 << 20}, http.StatusServiceUnavailable, "unavailable", "1", func(err error) bool {
			var e *uploadwire.BusyError
			return errors.As(err, &e) && e.ReceivedBytes == 64<<20
		}},
		{"garbled", &uploadwire.GarbledError{ReceivedBytes: 7}, http.StatusInternalServerError, "data_loss", "", func(err error) bool {
			var e *uploadwire.GarbledError
			return errors.As(err, &e) && e.ReceivedBytes == 7
		}},
		{"too large", uploadwire.TooLarge, http.StatusRequestEntityTooLarge, "resource_exhausted", "", func(err error) bool {
			return connect.CodeOf(err) == connect.CodeResourceExhausted
		}},
		{"too many calls", connect.NewError(connect.CodeResourceExhausted, errors.New("too many calls")), http.StatusTooManyRequests, "resource_exhausted", "", nil},
		{"node not connected", connect.NewError(connect.CodeUnavailable, errors.New("node n1 is not connected")), http.StatusServiceUnavailable, "unavailable", "", func(err error) bool {
			var e *uploadwire.BusyError
			return !errors.As(err, &e) && connect.CodeOf(err) == connect.CodeUnavailable
		}},
		{"malformed", connect.NewError(connect.CodeInvalidArgument, errors.New("bad range")), http.StatusBadRequest, "invalid_argument", "", nil},
		{"failed", connect.NewError(connect.CodeFailedPrecondition, errors.New("expired")), http.StatusBadRequest, "failed_precondition", "", nil},
		{"unauthenticated", connect.NewError(connect.CodeUnauthenticated, errors.New("sign in")), http.StatusUnauthorized, "unauthenticated", "", nil},
		{"denied", connect.NewError(connect.CodePermissionDenied, errors.New("no")), http.StatusForbidden, "permission_denied", "", nil},
		{"not found", connect.NewError(connect.CodeNotFound, errors.New("no such image")), http.StatusNotFound, "not_found", "", nil},
		{"internal", errors.New("disk on fire"), http.StatusInternalServerError, "unknown", "", nil},
	} {
		rec := httptest.NewRecorder()
		uploadwire.WriteError(rec, tt.err)
		resp := rec.Result()
		if resp.StatusCode != tt.status || resp.Header.Get("Retry-After") != tt.retryAfter || resp.Header.Get("Content-Type") != "application/json" {
			t.Errorf("%s: %d, Retry-After %q, %s; want %d, %q", tt.name, resp.StatusCode, resp.Header.Get("Retry-After"), resp.Header.Get("Content-Type"), tt.status, tt.retryAfter)
		}
		body := rec.Body.String()
		if !strings.Contains(body, `"code":"`+tt.code+`"`) {
			t.Errorf("%s: body %s, want code %s", tt.name, body, tt.code)
		}
		// Only the three that keep the upload where it is say where.
		carries := strings.HasPrefix(tt.name, "offset") || tt.name == "busy" || tt.name == "garbled"
		if strings.Contains(body, `"received_bytes"`) != carries {
			t.Errorf("%s: body %s, want received_bytes in it: %t", tt.name, body, carries)
		}
		got := uploadwire.ReadError(rec.Result())
		if tt.check != nil && !tt.check(got) {
			t.Errorf("%s: read back as %#v", tt.name, got)
		}
		if tt.check == nil && connect.CodeOf(got).String() != tt.code {
			t.Errorf("%s: read back as %v, want %s", tt.name, got, tt.code)
		}
	}
}

// TestAnAnswerThatIsNoRefusalReadsAsUnknown: a body that is not the
// wire's JSON, as a proxy's page, is an unknown error naming the status.
func TestAnAnswerThatIsNoRefusalReadsAsUnknown(t *testing.T) {
	for _, body := range []string{"<html>Bad Gateway</html>", `{"code":"no such code","message":"x"}`, ""} {
		rec := httptest.NewRecorder()
		rec.WriteHeader(http.StatusBadGateway)
		_, _ = rec.WriteString(body)
		err := uploadwire.ReadError(rec.Result())
		if connect.CodeOf(err) != connect.CodeUnknown || !strings.Contains(err.Error(), "502") {
			t.Errorf("%q: %v, want unknown naming 502", body, err)
		}
	}
	rec := httptest.NewRecorder()
	rec.WriteHeader(http.StatusConflict)
	_, _ = rec.WriteString(`{"code":"aborted","message":"another writer"}`)
	var off *uploadwire.OffsetError
	if err := uploadwire.ReadError(rec.Result()); errors.As(err, &off) || connect.CodeOf(err) != connect.CodeAborted {
		t.Errorf("an aborted carrying no received_bytes: %v, want a plain aborted", err)
	}
}
