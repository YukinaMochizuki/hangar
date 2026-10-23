package cli_test

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"connectrpc.com/connect"

	"github.com/YukinaMochizuki/hangar/internal/cli"
	"github.com/YukinaMochizuki/hangar/internal/uploadwire"
)

// fakeUploads plays the :upload path of every image as an apiserver and
// node would, in chunks of chunk bytes: each image's file as its chunks
// came, refusing a chunk not at its count, as a 409 carrying it. fail, if
// set, answers a PUT with its error instead, before the chunk counts or,
// with lost, after.
type fakeUploads struct {
	chunk int64
	mu    sync.Mutex
	files map[string][]byte
	puts  []string // each PUT's Content-Range
	auth  []string // each request's Authorization
	fail  func(n int) (err error, lost bool)
}

func newFakeUploads(chunk int64) *fakeUploads {
	return &fakeUploads{chunk: chunk, files: map[string][]byte{}}
}

func (f *fakeUploads) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.auth = append(f.auth, r.Header.Get("Authorization"))
	name := strings.TrimSuffix(r.URL.Path[strings.LastIndexByte(r.URL.Path, '/')+1:], ":upload")
	have := f.files[name]
	if r.Method == http.MethodGet {
		_ = json.NewEncoder(w).Encode(uploadwire.State{ReceivedBytes: int64(len(have)), ChunkSize: f.chunk})
		return
	}
	f.puts = append(f.puts, r.Header.Get("Content-Range"))
	first, n, _, err := uploadwire.ParseContentRange(r.Header.Get("Content-Range"))
	if err != nil || n > f.chunk {
		uploadwire.WriteError(w, connect.NewError(connect.CodeInvalidArgument, errors.New("bad range")))
		return
	}
	var lost bool
	if f.fail != nil {
		if err, lost = f.fail(len(f.puts)); err != nil && !lost {
			uploadwire.WriteError(w, err)
			return
		}
	}
	if first != int64(len(have)) {
		uploadwire.WriteError(w, &uploadwire.OffsetError{ReceivedBytes: int64(len(have))})
		return
	}
	body, _ := io.ReadAll(r.Body)
	sum := sha256.Sum256(body)
	if int64(len(body)) != n || hex.EncodeToString(sum[:]) != r.Header.Get(uploadwire.ChunkSHA256) {
		uploadwire.WriteError(w, connect.NewError(connect.CodeInvalidArgument, errors.New("the chunk's sha256 does not match its bytes; it is dropped")))
		return
	}
	f.files[name] = append(have, body...)
	if lost {
		uploadwire.WriteError(w, connect.NewError(connect.CodeUnavailable, errors.New("the connection dropped")))
		return
	}
	_ = json.NewEncoder(w).Encode(uploadwire.State{ReceivedBytes: int64(len(f.files[name]))})
}

func (f *fakeUploads) file(name string) []byte {
	f.mu.Lock()
	defer f.mu.Unlock()
	return bytes.Clone(f.files[name])
}

func (f *fakeUploads) sent() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.puts...)
}

// uploadWorld is a world whose node takes images, with uploads in chunks
// of 1000 bytes, retried at once, and a file of 2500 bytes to upload.
func uploadWorld(t *testing.T) (*world, *fakeUploads, string, []byte) {
	t.Helper()
	w := newWorld(t)
	w.as("alice@example.com")
	w.must(tokenStack, "apply", "-f", "-")
	w.node().ready()
	up := newFakeUploads(1000)
	h := http.Handler(up)
	w.uploads.Store(&h)
	w.opts = cli.Options{UploadBackoff: time.Millisecond}
	data := bytes.Repeat([]byte("qcow"), 625)
	path := filepath.Join(t.TempDir(), "nixos.qcow2")
	if err := os.WriteFile(path, data, 0o644); err != nil {
		t.Fatal(err)
	}
	return w, up, path, data
}

func hexOf(b []byte) string {
	s := sha256.Sum256(b)
	return hex.EncodeToString(s[:])
}

// TestImageUploadSendsTheFileInChunks: the image is made of the file's
// size and sha256, and the file goes in chunks of the size the server
// says, in order, with a progress line on stderr.
func TestImageUploadSendsTheFileInChunks(t *testing.T) {
	w, up, path, data := uploadWorld(t)
	r := w.run("", "image", "upload", path, "--name", "nixos", "--os", "nixos", "--version", "25.05", "--default-user", "root")
	if r.code != 0 {
		t.Fatalf("upload: exit %d: %s", r.code, r.err)
	}
	if !bytes.Equal(up.file("nixos"), data) {
		t.Error("the server got another file")
	}
	if got := strings.Join(up.sent(), ","); got != "bytes 0-999/2500,bytes 1000-1999/2500,bytes 2000-2499/2500" {
		t.Errorf("chunks %s", got)
	}
	if !strings.Contains(r.err, "uploading nixos") || !strings.Contains(r.err, "(100%)") {
		t.Errorf("progress %q", r.err)
	}
	if !strings.Contains(r.out, "customimage/nixos uploaded") {
		t.Errorf("said %q", r.out)
	}
	got := w.must("", "get", "customimage", "nixos", "-o", "json")
	for _, want := range []string{`"size": "2500"`, `"sha256": "` + hexOf(data) + `"`, `"os": "nixos"`, `"defaultUser": "root"`} {
		if !strings.Contains(got, want) {
			t.Errorf("the image lacks %s: %s", want, got)
		}
	}
}

// TestImageUploadRetriesAndResumes: a chunk that fails on the way is sent
// again, after a wait; one whose answer was lost after it counted goes on
// from where the server says; an upload given up on goes on, when the
// command is run again, from where it stopped.
func TestImageUploadRetriesAndResumes(t *testing.T) {
	w, up, path, data := uploadWorld(t)
	up.fail = func(n int) (error, bool) {
		switch {
		case n <= 2:
			return connect.NewError(connect.CodeUnavailable, errors.New("node not connected")), false
		case n == 3:
			return connect.NewError(connect.CodeUnavailable, errors.New("dropped")), true
		case n >= 6:
			return connect.NewError(connect.CodeUnavailable, errors.New("node gone for good")), false
		}
		return nil, false
	}
	r := w.run("", "image", "upload", path, "--name", "nixos")
	if r.code == 0 || !strings.Contains(r.err, "node gone for good") {
		t.Fatalf("an upload whose node went away: exit %d: %s", r.code, r.err)
	}
	if got := len(up.file("nixos")); got != 2000 {
		t.Fatalf("the server holds %d bytes, want the two chunks sent before it went", got)
	}
	sent := up.sent()
	// Tried three times, the third counted though its answer was lost;
	// the fourth try is told so, and the next chunk follows.
	if strings.Join(sent[:5], ",") != "bytes 0-999/2500,bytes 0-999/2500,bytes 0-999/2500,bytes 0-999/2500,bytes 1000-1999/2500" {
		t.Errorf("sent %v; want the first chunk tried until the server said it had it, then the second", sent)
	}
	if n := len(sent) - 5; n != 8 {
		t.Errorf("the last chunk tried %d times, want 8", n)
	}
	up.mu.Lock()
	up.fail, up.puts = nil, nil
	up.mu.Unlock()
	if r := w.run("", "image", "upload", path, "--name", "nixos"); r.code != 0 {
		t.Fatalf("running the upload again: exit %d: %s", r.code, r.err)
	}
	if got := strings.Join(up.sent(), ","); got != "bytes 2000-2499/2500" {
		t.Errorf("run again, it sent %s; want only what the server lacked", got)
	}
	if !bytes.Equal(up.file("nixos"), data) {
		t.Error("the server got another file")
	}
}

// TestImageUploadJSONIsQuiet: with -o json, the image alone goes to
// stdout, and nothing to stderr.
func TestImageUploadJSONIsQuiet(t *testing.T) {
	w, _, path, _ := uploadWorld(t)
	r := w.run("", "image", "upload", path, "--name", "nixos", "-o", "json")
	if r.code != 0 || r.err != "" {
		t.Fatalf("upload -o json: exit %d, stderr %q", r.code, r.err)
	}
	var obj map[string]any
	if err := json.Unmarshal([]byte(r.out), &obj); err != nil || obj["kind"] != "CustomImage" {
		t.Errorf("stdout %q is not the image: %v", r.out, err)
	}
}

// TestImageUploadRefusesAnotherImage: an image of the name made of
// another file, or of none, is not uploaded to, and the command says so,
// exiting as a conflict; one of the same file made unchecked is another.
func TestImageUploadRefusesAnotherImage(t *testing.T) {
	w, up, path, _ := uploadWorld(t)
	if r := w.run("", "image", "upload", path, "--name", "nixos", "--no-verify"); r.code != 0 {
		t.Fatalf("upload --no-verify: exit %d: %s", r.code, r.err)
	}
	if got := w.must("", "get", "customimage", "nixos", "-o", "json"); strings.Contains(got, `"sha256"`) {
		t.Errorf("--no-verify sent a sha256: %s", got)
	}
	r := w.run("", "image", "upload", path, "--name", "nixos")
	if r.code != cli.ExitConflict || !strings.Contains(r.err, "sha256") {
		t.Errorf("uploading checked over an image made unchecked: exit %d: %s", r.code, r.err)
	}
	other := filepath.Join(t.TempDir(), "other.raw")
	if err := os.WriteFile(other, make([]byte, 512), 0o644); err != nil {
		t.Fatal(err)
	}
	before := len(up.sent())
	r = w.run("", "image", "upload", other, "--name", "nixos", "--no-verify")
	if r.code != cli.ExitConflict || !strings.Contains(r.err, "2500 bytes") {
		t.Errorf("uploading another file over an image: exit %d: %s", r.code, r.err)
	}
	if n := len(up.sent()); n != before {
		t.Error("a refused upload sent chunks")
	}
	if r := w.run("", "image", "upload", filepath.Join(t.TempDir(), "missing"), "--name", "x"); r.code == 0 {
		t.Error("uploading a file that is not there")
	}
	if r := w.run("", "image", "upload", path); r.code != cli.ExitUsage {
		t.Errorf("upload without --name: exit %d", r.code)
	}
}

// TestImageUploadTakesAServiceToken: an agent uploads with HANGAR_TOKEN,
// which the upload path is sent too.
func TestImageUploadTakesAServiceToken(t *testing.T) {
	w, up, path, _ := uploadWorld(t)
	secret := strings.TrimSpace(w.must("", "token", "create", "uploader", "--role", "project.editor"))
	r := w.runEnv("", map[string]string{"HANGAR_TOKEN": secret}, "image", "upload", path, "--name", "nixos")
	if r.code != 0 {
		t.Fatalf("upload with HANGAR_TOKEN: exit %d: %s", r.code, r.err)
	}
	up.mu.Lock()
	defer up.mu.Unlock()
	for _, a := range up.auth {
		if a != "Bearer "+secret {
			t.Errorf("an upload request carried %q, not the service token", a)
			break
		}
	}
	noSecret(t, secret, r.out, r.err)
}
