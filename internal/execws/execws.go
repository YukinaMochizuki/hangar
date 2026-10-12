// Package execws is the wire of a container's exec between a client and the
// apiserver, which both ends share: a WebSocket of proto messages, its path,
// and how it says why it failed (proto/hangar/compute/v1alpha1/
// container_exec.proto). It is not the exec itself.
package execws

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"unicode/utf8"

	"connectrpc.com/connect"
	"github.com/coder/websocket"
)

// Subprotocol is the WebSocket subprotocol of the messages this package
// describes; the server refuses a client that does not ask for it.
const Subprotocol = "hangar.exec.v1alpha1"

// MaxMessage bounds a message either way. Stdin and output travel in
// smaller pieces; it leaves room for a start's command and environment.
const MaxMessage = 256 << 10

// Path is the exec endpoint of a container.
func Path(project, name string) string {
	return "/v1/projects/" + url.PathEscape(project) + "/containers/" + url.PathEscape(name) + ":exec"
}

// Pattern is Path as an http.ServeMux pattern, its last segment the name
// with ":exec" after it: a wildcard cannot share a segment.
const Pattern = "GET /v1/projects/{project}/containers/{nameExec}"

// NameOf returns the container's name in a request to Pattern, if it is an
// exec's.
func NameOf(r *http.Request) (string, bool) {
	return strings.CutSuffix(r.PathValue("nameExec"), ":exec")
}

// closeBase plus a Connect code is the close status of an exec failed with
// that code.
const closeBase = 4000

// maxReason is the most a close frame's reason holds.
const maxReason = 123

// CloseStatus is the close status and reason of an exec failed with err.
func CloseStatus(err error) (websocket.StatusCode, string) {
	msg := err.Error()
	var ce *connect.Error
	if errors.As(err, &ce) {
		msg = ce.Message()
	}
	if len(msg) > maxReason {
		msg = msg[:maxReason]
		for !utf8.ValidString(msg) {
			msg = msg[:len(msg)-1]
		}
	}
	return websocket.StatusCode(closeBase + int(connect.CodeOf(err))), msg
}

// ErrorOf is the error an exec failed with, from what the WebSocket closed
// with; nil for a normal closure.
func ErrorOf(err error) error {
	var ce websocket.CloseError
	if !errors.As(err, &ce) {
		return err
	}
	if ce.Code == websocket.StatusNormalClosure {
		return nil
	}
	code := connect.Code(int(ce.Code) - closeBase)
	if code < connect.CodeCanceled || code > connect.CodeUnauthenticated {
		return connect.NewError(connect.CodeUnknown, fmt.Errorf("the exec closed with %d: %s", ce.Code, ce.Reason))
	}
	return connect.NewError(code, errors.New(ce.Reason))
}

// wireError is a Connect error as JSON, as a refusal before the upgrade
// carries it.
type wireError struct {
	Code    string `json:"code"`
	Message string `json:"message"`
}

// WriteError refuses an exec before the upgrade, as Connect writes an error.
func WriteError(w http.ResponseWriter, r *http.Request, err error) {
	_ = connect.NewErrorWriter().Write(w, r, err)
}

// ReadError is the error a refusal before the upgrade, resp, says.
func ReadError(resp *http.Response) error {
	b, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<10))
	var we wireError
	var code connect.Code
	if json.Unmarshal(b, &we) == nil && code.UnmarshalText([]byte(we.Code)) == nil {
		return connect.NewError(code, errors.New(we.Message))
	}
	return connect.NewError(connect.CodeUnknown, fmt.Errorf("the server refused the exec: %s", resp.Status))
}
