package apiserver

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"connectrpc.com/connect"
	"github.com/jonboulle/clockwork"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/timestamppb"

	iamv1alpha1 "github.com/YukinaMochizuki/hangar/gen/go/hangar/iam/v1alpha1"
	metav1 "github.com/YukinaMochizuki/hangar/gen/go/hangar/meta/v1"
	nodev1 "github.com/YukinaMochizuki/hangar/gen/go/hangar/node/v1"
	"github.com/YukinaMochizuki/hangar/gen/go/hangar/node/v1/nodev1connect"
	"github.com/YukinaMochizuki/hangar/gen/go/hangar/registry"
	storagev1alpha1 "github.com/YukinaMochizuki/hangar/gen/go/hangar/storage/v1alpha1"
	"github.com/YukinaMochizuki/hangar/internal/apimachinery/conditions"
	"github.com/YukinaMochizuki/hangar/internal/apimachinery/resource"
	"github.com/YukinaMochizuki/hangar/internal/auth"
	"github.com/YukinaMochizuki/hangar/internal/auth/session"
	"github.com/YukinaMochizuki/hangar/internal/computeapi"
	"github.com/YukinaMochizuki/hangar/internal/store"
	"github.com/YukinaMochizuki/hangar/internal/testutil"
	"github.com/YukinaMochizuki/hangar/internal/uploadwire"
)

// uploadNode serves a node's upload calls from memory: each image's file
// as its chunks came, refusing, as a node does, a chunk not at its count
// (aborted), while another of the image is written (unavailable), short of
// its length, or not matching its sha256 (data_loss). hold, when set, keeps
// a chunk's call open once its first request is in, until closed; drop
// has the next chunk's call find the tunnel gone, unavailable, and down
// every call from then on.
type uploadNode struct {
	nodev1connect.UnimplementedNodeServiceHandler
	mu      sync.Mutex
	files   map[string][]byte
	sizes   map[string]int64
	largest int // the most bytes one request carried
	streams int // the chunks' calls begun
	asked   int // GetCustomImageUpload calls
	hold    chan struct{}
	held    chan struct{}
	writing map[string]bool // the images a chunk is being written of
	drop    bool
	down    bool
}

func newUploadNode() *uploadNode {
	return &uploadNode{files: map[string][]byte{}, sizes: map[string]int64{}, writing: map[string]bool{}, held: make(chan struct{}, 10)}
}

func (n *uploadNode) expect(uid string, size int64) {
	n.mu.Lock()
	defer n.mu.Unlock()
	n.sizes[uid] = size
}

func (n *uploadNode) file(uid string) []byte {
	n.mu.Lock()
	defer n.mu.Unlock()
	return bytes.Clone(n.files[uid])
}

func (n *uploadNode) counts() (streams, largest int) {
	n.mu.Lock()
	defer n.mu.Unlock()
	return n.streams, n.largest
}

// calls counts every call the node took.
func (n *uploadNode) calls() int {
	n.mu.Lock()
	defer n.mu.Unlock()
	return n.streams + n.asked
}

func (n *uploadNode) GetCustomImageUpload(_ context.Context, req *connect.Request[nodev1.GetCustomImageUploadRequest]) (*connect.Response[nodev1.GetCustomImageUploadResponse], error) {
	n.mu.Lock()
	defer n.mu.Unlock()
	n.asked++
	if n.down {
		return nil, connect.NewError(connect.CodeUnavailable, errors.New("node not connected"))
	}
	if _, ok := n.sizes[req.Msg.GetImageUid()]; !ok {
		return nil, connect.NewError(connect.CodeFailedPrecondition, errors.New("not taking an upload of this image"))
	}
	return connect.NewResponse(&nodev1.GetCustomImageUploadResponse{ReceivedBytes: int64(len(n.files[req.Msg.GetImageUid()]))}), nil
}

func (n *uploadNode) UploadCustomImage(ctx context.Context, s *connect.ClientStream[nodev1.UploadCustomImageRequest]) (*connect.Response[nodev1.UploadCustomImageResponse], error) {
	n.mu.Lock()
	n.streams++
	hold, down := n.hold, n.down || n.drop
	n.down = down
	n.mu.Unlock()
	if down {
		return nil, connect.NewError(connect.CodeUnavailable, errors.New("node not connected"))
	}
	if !s.Receive() {
		return nil, connect.NewError(connect.CodeInvalidArgument, errors.New("no chunk"))
	}
	c := s.Msg().GetChunk()
	n.mu.Lock()
	have, busy := int64(len(n.files[c.GetImageUid()])), n.writing[c.GetImageUid()]
	if !busy {
		n.writing[c.GetImageUid()] = true
	}
	n.mu.Unlock()
	if busy {
		return nil, connect.NewError(connect.CodeUnavailable, errors.New("another chunk of this upload is being written"))
	}
	defer func() {
		n.mu.Lock()
		defer n.mu.Unlock()
		delete(n.writing, c.GetImageUid())
	}()
	if c.GetOffset() != have {
		return nil, connect.NewError(connect.CodeAborted, fmt.Errorf("at %d", have))
	}
	if hold != nil {
		n.held <- struct{}{}
		select {
		case <-hold:
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
	var data []byte
	for s.Receive() {
		d := s.Msg().GetData()
		n.mu.Lock()
		n.largest = max(n.largest, len(d))
		n.mu.Unlock()
		data = append(data, d...)
	}
	if err := s.Err(); err != nil {
		return nil, err
	}
	if int64(len(data)) != c.GetLength() {
		return nil, connect.NewError(connect.CodeInvalidArgument, fmt.Errorf("%d bytes of %d", len(data), c.GetLength()))
	}
	if sum := sha256.Sum256(data); hex.EncodeToString(sum[:]) != c.GetSha256() {
		return nil, connect.NewError(connect.CodeDataLoss, errors.New("the chunk's sha256 does not match"))
	}
	n.mu.Lock()
	defer n.mu.Unlock()
	n.files[c.GetImageUid()] = append(n.files[c.GetImageUid()], data...)
	return connect.NewResponse(&nodev1.UploadCustomImageResponse{ReceivedBytes: int64(len(n.files[c.GetImageUid()]))}), nil
}

// uploadWorld is an execWorld whose node n1 takes uploads, with alice's
// image img to upload, of uploadFileSize bytes, bob an editor of web and
// carol a reader of the tenant's containers. Its listener takes dev
// users, browser sessions and CLI and service tokens.
type uploadWorld struct {
	*execWorld
	node   *uploadNode
	clock  *clockwork.FakeClock
	system context.Context
	user   context.Context
	url    string
	img    string // its uid
	file   []byte
}

const uploadFileSize = 3*uploadPiece + 1000

func newUploadWorld(t *testing.T, configure func(*Config)) *uploadWorld {
	t.Helper()
	w := &uploadWorld{node: newUploadNode(), execWorld: newExecWorld(t, func(c *Config) {
		c.Credentials = c.Store.(store.Credentials)
		if configure != nil {
			configure(c)
		}
	})}
	s := w.s
	w.clock = s.clock.(*clockwork.FakeClock)
	w.system = auth.WithPrincipal(context.Background(), auth.SystemPrincipal)
	w.user = auth.WithPrincipal(context.Background(), auth.Principal{Type: auth.User, ID: "alice@example.com"})
	if _, err := s.backend(registry.RoleBinding).Apply(w.system, binding("bob-web", bob.ID, "project.editor",
		&iamv1alpha1.BindingScope{Target: &iamv1alpha1.BindingScope_Project{Project: "web"}})); err != nil {
		t.Fatal(err)
	}
	uploadingNode(t, s, w.system, "n1")
	w.file = bytes.Repeat([]byte("nixos!\n"), uploadFileSize/7+1)[:uploadFileSize]
	res, err := s.backend(registry.CustomImage).Apply(w.user, uploadImage("img", uploadFileSize, ""))
	if err != nil {
		t.Fatal(err)
	}
	w.img = res.Object.(*storagev1alpha1.CustomImage).GetMetadata().GetUid()
	w.node.expect(w.img, uploadFileSize)
	w.connectNodeServing(t, w.node)
	srv := httptest.NewUnstartedServer(nil)
	w.url = "http://" + srv.Listener.Addr().String()
	srv.Config.Handler = s.Handler(auth.Chain{auth.Dev{}, session.Cookie{M: s.sessions, Origin: w.url}, session.Bearer{M: s.sessions}})
	srv.Start()
	t.Cleanup(srv.Close)
	return w
}

func devHeader(email string) http.Header { return http.Header{auth.DevUserHeader: {email}} }

func chunkSum(b []byte) string {
	s := sha256.Sum256(b)
	return hex.EncodeToString(s[:])
}

// get asks where img's upload is, as h.
func (w *uploadWorld) get(t *testing.T, h http.Header, name string) (uploadwire.State, error) {
	t.Helper()
	return w.request(t, h, http.MethodGet, name, nil, nil)
}

// put sends img's bytes from first to first+n, with their sha256 unless
// sum says otherwise, as h.
func (w *uploadWorld) put(t *testing.T, h http.Header, first, n int64, sum string) (uploadwire.State, error) {
	t.Helper()
	body := w.file[first : first+n]
	if sum == "" {
		sum = chunkSum(body)
	}
	set := http.Header{"Content-Range": {uploadwire.ContentRange(first, n, uploadFileSize)}, uploadwire.ChunkSHA256: {sum}}
	return w.request(t, h, http.MethodPut, "img", set, body)
}

func (w *uploadWorld) request(t *testing.T, h http.Header, method, name string, set http.Header, body []byte) (uploadwire.State, error) {
	t.Helper()
	req, err := http.NewRequestWithContext(t.Context(), method, w.url+uploadwire.Path("web", name), bytes.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	for k, v := range h {
		req.Header[k] = v
	}
	for k, v := range set {
		req.Header[k] = v
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return uploadwire.State{}, uploadwire.ReadError(resp)
	}
	var st uploadwire.State
	if err := json.NewDecoder(resp.Body).Decode(&st); err != nil {
		t.Fatal(err)
	}
	return st, nil
}

// putAnswer sends img's bytes from first to first+n as put does, and
// returns the response's status and Retry-After, and the error it reads
// as, if any.
func (w *uploadWorld) putAnswer(t *testing.T, h http.Header, first, n int64, sum string) (status int, retryAfter string, _ error) {
	t.Helper()
	body := w.file[first : first+n]
	if sum == "" {
		sum = chunkSum(body)
	}
	req, err := http.NewRequestWithContext(t.Context(), http.MethodPut, w.url+uploadwire.Path("web", "img"), bytes.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	for k, v := range h {
		req.Header[k] = v
	}
	req.Header.Set("Content-Range", uploadwire.ContentRange(first, n, uploadFileSize))
	req.Header.Set(uploadwire.ChunkSHA256, sum)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode == http.StatusOK {
		return resp.StatusCode, resp.Header.Get("Retry-After"), nil
	}
	return resp.StatusCode, resp.Header.Get("Retry-After"), uploadwire.ReadError(resp)
}

// putRaw sends a PUT of body to img with the headers given, and returns
// the response's status.
func (w *uploadWorld) putRaw(t *testing.T, h http.Header, body io.Reader) int {
	t.Helper()
	req, err := http.NewRequestWithContext(t.Context(), http.MethodPut, w.url+uploadwire.Path("web", "img"), body)
	if err != nil {
		t.Fatal(err)
	}
	req.Header = h
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	return resp.StatusCode
}

func wantCode(t *testing.T, err error, code connect.Code, what string) {
	t.Helper()
	if connect.CodeOf(err) != code {
		t.Errorf("%s: %v, want %s", what, err, code)
	}
}

// TestAnUploadIsRelayedChunkByChunk: the node's count answers a GET, and
// each chunk is relayed to the node a message of at most uploadPiece at
// a time; the file is the node's as sent.
func TestAnUploadIsRelayedChunkByChunk(t *testing.T) {
	t.Parallel()
	w := newUploadWorld(t, nil)
	alice := devHeader("alice@example.com")
	st, err := w.get(t, alice, "img")
	if err != nil || st != (uploadwire.State{ChunkSize: uploadwire.ChunkSize, Size: uploadFileSize}) {
		t.Fatalf("GET: %+v %v", st, err)
	}
	half := int64(uploadFileSize / 2)
	if st, err := w.put(t, alice, 0, half, ""); err != nil || st.ReceivedBytes != half {
		t.Fatalf("the first chunk: %+v %v", st, err)
	}
	if st, err := w.get(t, alice, "img"); err != nil || st.ReceivedBytes != half {
		t.Errorf("GET after a chunk: %+v %v", st, err)
	}
	if st, err := w.put(t, alice, half, uploadFileSize-half, ""); err != nil || st.ReceivedBytes != uploadFileSize {
		t.Fatalf("the last chunk: %+v %v", st, err)
	}
	if !bytes.Equal(w.node.file(w.img), w.file) {
		t.Error("the node's file is not the one sent")
	}
	if _, largest := w.node.counts(); largest > uploadPiece || largest == 0 {
		t.Errorf("a message to the node carried %d bytes, more than %d", largest, uploadPiece)
	}
}

// TestAChunkAtTheWrongOffsetIsRefused: a chunk not at the node's count is
// a 409 carrying the count, and nothing of it reaches the node.
func TestAChunkAtTheWrongOffsetIsRefused(t *testing.T) {
	t.Parallel()
	w := newUploadWorld(t, nil)
	alice := devHeader("alice@example.com")
	if _, err := w.put(t, alice, 0, 1000, ""); err != nil {
		t.Fatal(err)
	}
	before, _ := w.node.counts()
	for _, first := range []int64{0, 2000} {
		_, err := w.put(t, alice, first, 1000, "")
		var off *uploadwire.OffsetError
		if !errors.As(err, &off) || off.ReceivedBytes != 1000 {
			t.Errorf("a chunk at %d: %v, want a 409 at 1000", first, err)
		}
	}
	if streams, _ := w.node.counts(); streams != before {
		t.Errorf("%d chunks at the wrong offset reached the node", streams-before)
	}
	status := w.putRaw(t, http.Header{auth.DevUserHeader: {"alice@example.com"}, "Content-Range": {uploadwire.ContentRange(0, 10, uploadFileSize)},
		uploadwire.ChunkSHA256: {chunkSum(w.file[:10])}}, bytes.NewReader(w.file[:10]))
	if status != http.StatusConflict {
		t.Errorf("a chunk at the wrong offset answered %d, want 409", status)
	}
}

// TestATooLargeChunkIsRefused: a chunk of more than the chunk size is a
// 413, refused from its Content-Range before any of it is read.
func TestATooLargeChunkIsRefused(t *testing.T) {
	t.Parallel()
	w := newUploadWorld(t, nil)
	h := http.Header{auth.DevUserHeader: {"alice@example.com"},
		"Content-Range": {uploadwire.ContentRange(0, uploadwire.ChunkSize+1, uploadFileSize+uploadwire.ChunkSize)}, uploadwire.ChunkSHA256: {chunkSum(nil)}}
	if status := w.putRaw(t, h, strings.NewReader("")); status != http.StatusRequestEntityTooLarge {
		t.Errorf("a chunk of %d bytes answered %d, want 413", uploadwire.ChunkSize+1, status)
	}
	if streams, _ := w.node.counts(); streams != 0 {
		t.Errorf("a chunk too large reached the node")
	}
}

// TestAChunkNotMatchingItsSHA256IsDropped: a chunk whose bytes are not
// its sha256's is a 500 data_loss, to send again, carrying where the
// upload stays; a header that is no sha256 is refused before anything
// reaches the node.
func TestAChunkNotMatchingItsSHA256IsDropped(t *testing.T) {
	t.Parallel()
	w := newUploadWorld(t, nil)
	alice := devHeader("alice@example.com")
	if _, err := w.put(t, alice, 0, 1000, ""); err != nil {
		t.Fatal(err)
	}
	status, _, err := w.putAnswer(t, alice, 1000, 1000, chunkSum([]byte("something else")))
	var garbled *uploadwire.GarbledError
	if status != http.StatusInternalServerError || !errors.As(err, &garbled) || garbled.ReceivedBytes != 1000 {
		t.Errorf("a chunk not matching its sha256: %d %v, want a 500 data_loss at 1000", status, err)
	}
	if st, err := w.get(t, alice, "img"); err != nil || st.ReceivedBytes != 1000 {
		t.Errorf("after a chunk dropped: %+v %v, want 1000 received", st, err)
	}
	before, _ := w.node.counts()
	_, err = w.put(t, alice, 1000, 1000, strings.ToUpper(chunkSum(w.file[1000:2000])))
	wantCode(t, err, connect.CodeInvalidArgument, "a chunk with a sha256 in upper case")
	if streams, _ := w.node.counts(); streams != before {
		t.Error("a chunk with no sha256 reached the node")
	}
}

// TestAChunkWhileAnotherIsWrittenIsBusy: a chunk sent while another of the
// same upload is being written is a 503 unavailable with Retry-After,
// carrying where the upload is, the other written on; a node gone is a
// 503 too, but neither says where the upload is nor when to come back.
func TestAChunkWhileAnotherIsWrittenIsBusy(t *testing.T) {
	t.Parallel()
	w := newUploadWorld(t, nil)
	alice := devHeader("alice@example.com")
	w.node.mu.Lock()
	w.node.hold = make(chan struct{})
	w.node.mu.Unlock()
	first := make(chan error, 1)
	go func() {
		_, err := w.put(t, alice, 0, 1000, "")
		first <- err
	}()
	<-w.node.held
	w.node.mu.Lock()
	hold := w.node.hold
	w.node.hold = nil
	w.node.mu.Unlock()

	status, retryAfter, err := w.putAnswer(t, alice, 0, 1000, "")
	var busy *uploadwire.BusyError
	if status != http.StatusServiceUnavailable || !errors.As(err, &busy) || busy.ReceivedBytes != 0 {
		t.Errorf("a chunk while another is written: %d %v, want a 503 at 0", status, err)
	}
	if retryAfter != "1" {
		t.Errorf("Retry-After %q, want 1", retryAfter)
	}
	close(hold)
	if err := <-first; err != nil {
		t.Errorf("the chunk written meanwhile: %v", err)
	}
	if st, err := w.get(t, alice, "img"); err != nil || st.ReceivedBytes != 1000 {
		t.Errorf("after both: %+v %v, want the first chunk counted", st, err)
	}

	w.node.mu.Lock()
	w.node.drop = true
	w.node.mu.Unlock()
	status, retryAfter, err = w.putAnswer(t, alice, 1000, 1000, "")
	if status != http.StatusServiceUnavailable || errors.As(err, &busy) || retryAfter != "" {
		t.Errorf("a chunk to a node gone: %d %v, Retry-After %q; want a 503 saying nothing of the upload", status, err, retryAfter)
	}
}

// TestAnImageNotAwaitingAnUploadIsRefused: a captured image, one whose
// file is whole, and one that failed take no chunks.
func TestAnImageNotAwaitingAnUploadIsRefused(t *testing.T) {
	t.Parallel()
	w := newUploadWorld(t, nil)
	alice := devHeader("alice@example.com")
	madeDisk(t, w.s, w.system, w.user, "data", "n1", "4Gi")
	if _, err := w.s.backend(registry.CustomImage).Apply(w.user, customImage("captured", "data")); err != nil {
		t.Fatal(err)
	}
	_, err := w.get(t, alice, "captured")
	wantCode(t, err, connect.CodeFailedPrecondition, "a captured image")
	if n := w.node.calls(); n != 0 {
		t.Errorf("asking of a captured image made %d calls to its node", n)
	}
	for name, c := range map[string]*metav1.Condition{
		"one whose file is whole": {Type: computeapi.CustomImageUploaded, Status: metav1.ConditionStatus_CONDITION_STATUS_TRUE, Reason: computeapi.ReasonUploaded},
		"one that failed":         uploadFailed(computeapi.ReasonDigestMismatch),
	} {
		w.setImageStatus(t, func(st *storagev1alpha1.CustomImageStatus) {
			st.Conditions = conditions.Set(nil, c, time.Now())
		})
		_, err := w.put(t, alice, 0, 1000, "")
		wantCode(t, err, connect.CodeFailedPrecondition, name)
		if n := w.node.calls(); n != 0 {
			t.Errorf("a chunk of %s made %d calls to its node", name, n)
		}
	}
	_, err = w.get(t, alice, "nope")
	wantCode(t, err, connect.CodeNotFound, "an image that is not there")
}

// setImageStatus writes img's status as its node does, changed by change.
func (w *uploadWorld) setImageStatus(t *testing.T, change func(*storagev1alpha1.CustomImageStatus)) {
	t.Helper()
	ci := getCustomImage(t, w.s, w.system, "img")
	st := proto.Clone(ci.GetStatus()).(*storagev1alpha1.CustomImageStatus)
	change(st)
	if _, err := w.s.backend(registry.CustomImage).UpdateStatus(asNode(w.system, "n1"), resource.Key{Project: "web", Name: "img"}, st, nil); err != nil {
		t.Fatal(err)
	}
}

// TestAnExpiredUploadIsRefused: past upload_expires, by the apiserver's
// clock, the image takes no chunks, whatever its node says.
func TestAnExpiredUploadIsRefused(t *testing.T) {
	t.Parallel()
	w := newUploadWorld(t, nil)
	alice := devHeader("alice@example.com")
	w.setImageStatus(t, func(st *storagev1alpha1.CustomImageStatus) {
		st.UploadExpires = timestamppb.New(w.clock.Now().Add(time.Hour))
	})
	if _, err := w.put(t, alice, 0, 1000, ""); err != nil {
		t.Fatalf("a chunk before the upload expires: %v", err)
	}
	w.clock.Advance(time.Hour)
	_, err := w.put(t, alice, 1000, 1000, "")
	wantCode(t, err, connect.CodeFailedPrecondition, "a chunk once the upload expired")
	if err == nil || !strings.Contains(err.Error(), "expired") {
		t.Errorf("%v; want it to say the upload expired", err)
	}
}

// TestUploadingIsApplying: a caller who may not apply the image, a viewer
// of its project among them, gets nothing of it, and one with no
// credential is unauthenticated; an editor of its project uploads.
func TestUploadingIsApplying(t *testing.T) {
	t.Parallel()
	w := newUploadWorld(t, nil)
	addMember(t, w.s, "dave@example.com")
	if _, err := w.s.backend(registry.RoleBinding).Apply(w.system, binding("dave-web", "dave@example.com", "project.viewer",
		&iamv1alpha1.BindingScope{Target: &iamv1alpha1.BindingScope_Project{Project: "web"}})); err != nil {
		t.Fatal(err)
	}
	_, err := w.put(t, devHeader("dave@example.com"), 0, 1000, "")
	wantCode(t, err, connect.CodePermissionDenied, "a viewer of web, who reads the image")
	_, err = w.get(t, devHeader("carol@example.com"), "img")
	wantCode(t, err, connect.CodePermissionDenied, "a reader of the tenant's containers")
	_, err = w.put(t, devHeader("carol@example.com"), 0, 1000, "")
	wantCode(t, err, connect.CodePermissionDenied, "a reader's chunk")
	_, err = w.get(t, devHeader("carol@example.com"), "nope")
	wantCode(t, err, connect.CodePermissionDenied, "a reader asking of an image that is not there")
	_, err = w.get(t, http.Header{}, "img")
	wantCode(t, err, connect.CodeUnauthenticated, "no credential")
	if n := w.node.calls(); n != 0 {
		t.Errorf("callers refused made %d calls to the node", n)
	}
	if _, err := w.put(t, devHeader(bob.ID), 0, 1000, ""); err != nil {
		t.Errorf("an editor of web: %v", err)
	}
}

// TestUploadsTakeEveryCredential: a browser's session uploads from the
// console's origin alone, and a GET without it is refused, as every
// request the cookie authenticates; a CLI login and a service token
// upload.
func TestUploadsTakeEveryCredential(t *testing.T) {
	t.Parallel()
	w := newUploadWorld(t, nil)
	b, err := w.s.sessions.StartBrowser(t.Context(), bob)
	if err != nil {
		t.Fatal(err)
	}
	cookie := session.CookieName + "=" + b.Secret.Reveal()
	_, err = w.get(t, http.Header{"Cookie": {cookie}}, "img")
	wantCode(t, err, connect.CodeUnauthenticated, "a session's GET with no Origin")
	_, err = w.put(t, http.Header{"Cookie": {cookie}, "Origin": {"https://evil.example"}}, 0, 1000, "")
	wantCode(t, err, connect.CodeUnauthenticated, "a session's PUT from another origin")
	_, err = w.put(t, http.Header{"Cookie": {cookie}, "Origin": {w.url}, "Sec-Fetch-Site": {"cross-site"}}, 0, 1000, "")
	wantCode(t, err, connect.CodeUnauthenticated, "a session's PUT a cross-site page made")
	if streams, _ := w.node.counts(); streams != 0 {
		t.Error("a chunk refused for its origin reached the node")
	}
	at := int64(0)
	if st, err := w.put(t, http.Header{"Cookie": {cookie}, "Origin": {w.url}, "Sec-Fetch-Site": {"same-origin"}}, at, 1000, ""); err != nil {
		t.Errorf("a session's PUT from the console: %v", err)
	} else {
		at = st.ReceivedBytes
	}
	tokens, err := w.s.sessions.StartCLI(t.Context(), bob)
	if err != nil {
		t.Fatal(err)
	}
	if st, err := w.put(t, http.Header{"Authorization": {"Bearer " + tokens.Access.Reveal()}}, at, 1000, ""); err != nil {
		t.Errorf("a CLI login's PUT: %v", err)
	} else {
		at = st.ReceivedBytes
	}
	sw := &streamWorld{execWorld: w.execWorld, user: w.user}
	if _, err := w.put(t, sw.serviceToken(t), at, 1000, ""); err != nil {
		t.Errorf("a service token's PUT: %v", err)
	}
}

// TestUploadsCountAgainstTheCallsToNodes: a chunk under way holds one of
// its caller's calls to nodes; over the cap, the next is refused at once.
func TestUploadsCountAgainstTheCallsToNodes(t *testing.T) {
	t.Parallel()
	w := newUploadWorld(t, func(c *Config) { c.DefaultMachineCalls = 1 })
	alice := devHeader("alice@example.com")
	hold := make(chan struct{})
	w.node.mu.Lock()
	w.node.hold = hold
	w.node.mu.Unlock()
	done := make(chan error, 1)
	go func() {
		_, err := w.put(t, alice, 0, 1000, "")
		done <- err
	}()
	<-w.node.held
	_, err := w.get(t, alice, "img")
	wantCode(t, err, connect.CodeResourceExhausted, "a second call while a chunk is under way")
	close(hold)
	if err := <-done; err != nil {
		t.Fatalf("the held chunk: %v", err)
	}
	testutil.Eventually(t, "the chunk's call given back", func() bool {
		_, err := w.get(t, alice, "img")
		return err == nil
	})
}

// TestAChunkEndsWithWhatLetItIn: a chunk is held as a stream is: once its
// caller's CLI token is revoked, or the binding that let them apply the
// image deleted, the next recheck ends it, and its call to the node, while
// the node writes it or while its body comes slowly; nothing of it counts.
func TestAChunkEndsWithWhatLetItIn(t *testing.T) {
	t.Parallel()
	for _, tt := range []struct {
		name     string
		slowBody bool
		revoke   func(*uploadWorld, *testing.T)
		code     connect.Code
		status   int
	}{
		{"token revoked, the node writing", false, func(w *uploadWorld, t *testing.T) {
			if _, err := w.s.sessions.RevokePrincipal(t.Context(), bob); err != nil {
				t.Fatal(err)
			}
		}, connect.CodeUnauthenticated, http.StatusUnauthorized},
		{"binding deleted, the body coming slowly", true, func(w *uploadWorld, t *testing.T) {
			if _, err := w.s.backend(registry.RoleBinding).Delete(w.system, resource.Key{Name: "bob-web"}, nil); err != nil {
				t.Fatal(err)
			}
		}, connect.CodePermissionDenied, http.StatusForbidden},
	} {
		t.Run(tt.name, func(t *testing.T) {
			w := newUploadWorld(t, nil)
			tokens, err := w.s.sessions.StartCLI(t.Context(), bob)
			if err != nil {
				t.Fatal(err)
			}
			if !tt.slowBody {
				w.node.mu.Lock()
				w.node.hold = make(chan struct{})
				w.node.mu.Unlock()
			}
			pr, pw := io.Pipe()
			defer func() { _ = pw.Close() }()
			req, err := http.NewRequestWithContext(t.Context(), http.MethodPut, w.url+uploadwire.Path("web", "img"), pr)
			if err != nil {
				t.Fatal(err)
			}
			req.ContentLength = 2000
			req.Header.Set("Authorization", "Bearer "+tokens.Access.Reveal())
			req.Header.Set("Content-Range", uploadwire.ContentRange(0, 2000, uploadFileSize))
			req.Header.Set(uploadwire.ChunkSHA256, chunkSum(w.file[:2000]))
			type answer struct {
				status int
				err    error
			}
			done := make(chan answer, 1)
			go func() {
				resp, err := http.DefaultClient.Do(req)
				if err != nil {
					done <- answer{err: err}
					return
				}
				defer func() { _ = resp.Body.Close() }()
				done <- answer{resp.StatusCode, uploadwire.ReadError(resp)}
			}()
			if tt.slowBody {
				if _, err := pw.Write(w.file[:1000]); err != nil {
					t.Fatal(err)
				}
				// The apiserver reads on for the rest of the piece it relays.
				testutil.Eventually(t, "the chunk's call to the node", func() bool {
					streams, _ := w.node.counts()
					return streams == 1
				})
			} else {
				go func() { _, _ = pw.Write(w.file[:2000]) }()
				<-w.node.held
			}
			tt.revoke(w, t)
			w.clock.Advance(auth.RecheckInterval)
			a := <-done
			if a.status != tt.status || connect.CodeOf(a.err) != tt.code {
				t.Errorf("the chunk: %d %v, want %d %s", a.status, a.err, tt.status, tt.code)
			}
			if got := w.node.file(w.img); len(got) != 0 {
				t.Errorf("the node holds %d bytes of a chunk ended", len(got))
			}
		})
	}
}

// TestAMalformedChunkIsRefusedBeforeTheNode: a chunk whose Content-Range
// names another file's size, or whose body is not the range's length,
// shorter or longer, is invalid_argument; nothing of the first three
// reaches the node, and nothing of any counts.
func TestAMalformedChunkIsRefusedBeforeTheNode(t *testing.T) {
	t.Parallel()
	w := newUploadWorld(t, nil)
	alice := "alice@example.com"
	header := func(first, n, size int64, sum string) http.Header {
		return http.Header{auth.DevUserHeader: {alice}, "Content-Range": {uploadwire.ContentRange(first, n, size)}, uploadwire.ChunkSHA256: {sum}}
	}
	for _, tt := range []struct {
		name string
		h    http.Header
		body io.Reader
		// A chunk found malformed only as it is relayed may reach the node,
		// or not, as its call is cut.
		mayReach bool
		status   int
	}{
		{"another file's size", header(0, 1000, uploadFileSize+1, chunkSum(w.file[:1000])), bytes.NewReader(w.file[:1000]), false, http.StatusBadRequest},
		{"a body shorter than its range", header(0, 1000, uploadFileSize, chunkSum(w.file[:1000])), bytes.NewReader(w.file[:999]), false, http.StatusBadRequest},
		{"a body longer than its range", header(0, 1000, uploadFileSize, chunkSum(w.file[:1000])), bytes.NewReader(w.file[:1001]), false, http.StatusBadRequest},
		// Sent without a length, the body is read to the range's end and
		// one byte more.
		{"a body of no length, longer than its range", header(0, 1000, uploadFileSize, chunkSum(w.file[:1000])), io.MultiReader(bytes.NewReader(w.file[:1001])), true, http.StatusBadRequest},
	} {
		before, _ := w.node.counts()
		held := len(w.node.file(w.img))
		if status := w.putRaw(t, tt.h, tt.body); status != tt.status {
			t.Errorf("%s: answered %d, want %d", tt.name, status, tt.status)
		}
		if streams, _ := w.node.counts(); !tt.mayReach && streams != before {
			t.Errorf("%s: %d calls to the node, want none", tt.name, streams-before)
		}
		if got := len(w.node.file(w.img)); got != held {
			t.Errorf("%s: the node holds %d bytes, %d before the chunk", tt.name, got, held)
		}
	}
}

// TestAnImageBeingDeletedTakesNoChunks: an upload's image being deleted,
// which a finalizer might keep a while, takes no chunk.
func TestAnImageBeingDeletedTakesNoChunks(t *testing.T) {
	t.Parallel()
	w := newUploadWorld(t, nil)
	ci := getCustomImage(t, w.s, w.system, "img")
	if err := w.s.awaitsUpload(ci); err != nil {
		t.Fatalf("the image as made: %v", err)
	}
	ci.Metadata.DeleteTime = timestamppb.New(w.clock.Now())
	err := w.s.awaitsUpload(ci)
	if connect.CodeOf(err) != connect.CodeFailedPrecondition || !strings.Contains(err.Error(), "being deleted") {
		t.Errorf("an image being deleted: %v, want failed_precondition saying so", err)
	}
}
