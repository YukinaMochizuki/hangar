// Package uploadwire is the wire of a CustomImage's upload between a client
// and the apiserver, which both ends share: the image's :upload path, a GET
// of where the upload is, a PUT of one chunk with its Content-Range and
// sha256, the JSON they answer, and the Connect-style JSON errors that
// refuse a chunk before any of it is relayed (docs/decisions/custom-images.md).
// It is not the upload itself.
package uploadwire

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"strconv"

	"connectrpc.com/connect"
)

// Action is the suffix of an upload's path.
const Action = "upload"

// ChunkSize is the most bytes one chunk holds.
const ChunkSize = 64 << 20

// ChunkSHA256 is the header a chunk's sha256 comes in, as 64 lowercase hex
// digits.
const ChunkSHA256 = "X-Hangar-Chunk-SHA256"

// Path is the upload path of the CustomImage name in project.
func Path(project, name string) string {
	return "/v1/projects/" + url.PathEscape(project) + "/customimages/" + url.PathEscape(name) + ":" + Action
}

// State is what a GET answers: the bytes the image's node holds, every
// chunk checked, which is where the next chunk starts; the most bytes a
// chunk may hold; and the file's size. A PUT answers ReceivedBytes alone.
type State struct {
	ReceivedBytes int64 `json:"received_bytes"`
	ChunkSize     int64 `json:"chunk_size,omitempty"`
	Size          int64 `json:"size,omitempty"`
}

// ContentRange is a chunk's Content-Range: bytes First-Last/Size.
func ContentRange(first, n, size int64) string {
	return fmt.Sprintf("bytes %d-%d/%d", first, first+n-1, size)
}

var rangeRE = regexp.MustCompile(`^bytes (\d{1,19})-(\d{1,19})/(\d{1,19})$`)

// ParseContentRange reads a chunk's Content-Range: where it starts, how
// many bytes it holds, and the whole file's size.
func ParseContentRange(s string) (first, n, size int64, err error) {
	m := rangeRE.FindStringSubmatch(s)
	if m == nil {
		return 0, 0, 0, fmt.Errorf("content-range %q is not bytes FIRST-LAST/SIZE", s)
	}
	var v [3]int64
	for i := range v {
		if v[i], err = strconv.ParseInt(m[i+1], 10, 64); err != nil {
			return 0, 0, 0, fmt.Errorf("content-range %q: %w", s, err)
		}
	}
	if v[1] < v[0] || v[1] >= v[2] {
		return 0, 0, 0, fmt.Errorf("content-range %q is not a range of bytes within the file", s)
	}
	return v[0], v[1] - v[0] + 1, v[2], nil
}

// wireError is a Connect error as JSON, as a refusal carries it, and, for
// a chunk that does not start where the upload is, where it is.
type wireError struct {
	Code          string `json:"code"`
	Message       string `json:"message"`
	ReceivedBytes *int64 `json:"received_bytes,omitempty"`
}

// OffsetError is a chunk refused for not starting at ReceivedBytes, the
// bytes the image's node holds.
type OffsetError struct {
	ReceivedBytes int64
}

func (e *OffsetError) Error() string {
	return fmt.Sprintf("the upload is at %d bytes; send the chunk starting there", e.ReceivedBytes)
}

// TooLarge is a chunk of more than ChunkSize bytes.
var TooLarge = connect.NewError(connect.CodeResourceExhausted, fmt.Errorf("a chunk holds at most %d bytes", ChunkSize))

// WriteError refuses a request with err: an *OffsetError as 409 Aborted,
// carrying the offset; TooLarge as 413; any other with its Connect code,
// as Connect writes it.
func WriteError(w http.ResponseWriter, err error) {
	we := wireError{Code: connect.CodeOf(err).String(), Message: err.Error()}
	var ce *connect.Error
	if errors.As(err, &ce) {
		we.Message = ce.Message()
	}
	status := httpStatus(connect.CodeOf(err))
	var off *OffsetError
	switch {
	case errors.As(err, &off):
		we.Code, status = connect.CodeAborted.String(), http.StatusConflict
		we.ReceivedBytes = &off.ReceivedBytes
	case errors.Is(err, TooLarge):
		status = http.StatusRequestEntityTooLarge
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(we)
}

// ReadError is the error a refused request, resp, says: an *OffsetError
// for one carrying the offset.
func ReadError(resp *http.Response) error {
	b, _ := io.ReadAll(io.LimitReader(resp.Body, 4<<10))
	var we wireError
	var code connect.Code
	if json.Unmarshal(b, &we) != nil || code.UnmarshalText([]byte(we.Code)) != nil {
		return connect.NewError(connect.CodeUnknown, fmt.Errorf("the server refused the upload: %s", resp.Status))
	}
	if we.ReceivedBytes != nil && code == connect.CodeAborted {
		return &OffsetError{ReceivedBytes: *we.ReceivedBytes}
	}
	return connect.NewError(code, errors.New(we.Message))
}

// httpStatus is the HTTP status Connect gives an error of code.
func httpStatus(code connect.Code) int {
	switch code {
	case connect.CodeCanceled:
		return 499
	case connect.CodeInvalidArgument, connect.CodeFailedPrecondition, connect.CodeOutOfRange:
		return http.StatusBadRequest
	case connect.CodeDeadlineExceeded:
		return http.StatusGatewayTimeout
	case connect.CodeNotFound:
		return http.StatusNotFound
	case connect.CodeAlreadyExists, connect.CodeAborted:
		return http.StatusConflict
	case connect.CodePermissionDenied:
		return http.StatusForbidden
	case connect.CodeResourceExhausted:
		return http.StatusTooManyRequests
	case connect.CodeUnimplemented:
		return http.StatusNotImplemented
	case connect.CodeUnavailable:
		return http.StatusServiceUnavailable
	case connect.CodeUnauthenticated:
		return http.StatusUnauthorized
	}
	return http.StatusInternalServerError
}
