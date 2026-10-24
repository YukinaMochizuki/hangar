package node

import (
	"context"
	"crypto/sha256"
	"encoding"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"hash"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"sync"

	"golang.org/x/sys/unix"

	"github.com/YukinaMochizuki/hangar/internal/node/driver"
)

// stager keeps the files tenants upload for CustomImages until they are
// converted: dir/<image uid>, written chunk by chunk at the offset the
// node has reached, each chunk counted only once its sha256 matches, and
// the whole file's sha256 computed as the chunks come. The count and the
// hash's state are kept beside it, in dir/<image uid>.state, so an upload
// goes on, its hash right, after the agent restarts. The host only writes
// the file and hashes it; it never reads what the file holds
// (docs/decisions/custom-images.md). The file is laid out as
// driver.UploadedFile says: the tenant's bytes at driver.UploadOffset,
// after zeros, and once whole, zeros after them up to
// driver.UploadedLength, so the converter VM's Cloud Hypervisor, which
// reads a disk's first and last bytes for a format's header, never sees
// the tenant's there. Every count is of the tenant's bytes.
type stager struct {
	dir string
	// counted is called with an image's uid each time a chunk of it
	// counts, whole once the file is all in.
	counted func(uid string, whole bool)

	mu sync.Mutex
	// expected are the uploads the node takes chunks of, by uid: the file's
	// size.
	expected map[string]int64
	// writing are the uploads a chunk is being written to, each with what
	// ends that write.
	writing map[string]context.CancelCauseFunc
}

// stagedState is what is kept of an upload beside its file.
type stagedState struct {
	// Size is the file's, as its image declares it.
	Size int64
	// Received counts the bytes from the file's start written, synced and
	// checked.
	Received int64
	// Hash is the state of the file's sha256 at Received
	// (encoding.BinaryMarshaler).
	Hash []byte
}

// Errors of a chunk the stager refuses.
var (
	// errNotExpected is a chunk of an upload the node takes none of.
	errNotExpected = errors.New("the node is not taking an upload of this image")
	// errChunkBusy is a chunk while another of the same upload is written.
	errChunkBusy = errors.New("another chunk of this upload is being written; send one at a time")
	// errChunkDigest is a chunk whose sha256 does not match its bytes.
	errChunkDigest = errors.New("the chunk's sha256 does not match its bytes; it is dropped")
	// errChunkShape is a chunk that is empty, runs past the file's size,
	// or holds more or fewer bytes than it says.
	errChunkShape = errors.New("the chunk is malformed")
)

// offsetError is a chunk that does not start where the upload has reached.
type offsetError struct{ received int64 }

func (e *offsetError) Error() string {
	return fmt.Sprintf("the chunk does not start at %d, the bytes the node holds", e.received)
}

func newStager(dir string, counted func(uid string, whole bool)) (*stager, error) {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, err
	}
	if err := os.Chmod(dir, 0o700); err != nil {
		return nil, err
	}
	return &stager{dir: dir, counted: counted, expected: map[string]int64{}, writing: map[string]context.CancelCauseFunc{}}, nil
}

func (s *stager) path(uid string) string      { return filepath.Join(s.dir, uid) }
func (s *stager) statePath(uid string) string { return filepath.Join(s.dir, uid+".state") }

// expect has the node take chunks of uid's upload, of a file of size bytes.
func (s *stager) expect(uid string, size int64) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.expected[uid] = size
}

// state is what the node holds of uid's upload, from its kept state: none
// at all for one not started.
func (s *stager) state(uid string, size int64) (stagedState, error) {
	b, err := os.ReadFile(s.statePath(uid))
	if errors.Is(err, fs.ErrNotExist) {
		h, _ := sha256.New().(encoding.BinaryMarshaler).MarshalBinary()
		return stagedState{Size: size, Hash: h}, nil
	}
	if err != nil {
		return stagedState{}, err
	}
	var st stagedState
	if err := json.Unmarshal(b, &st); err != nil {
		return stagedState{}, fmt.Errorf("%s: %w", s.statePath(uid), err)
	}
	if st.Size != size || st.Received < 0 || st.Received > size {
		return stagedState{}, fmt.Errorf("%s holds %d of %d bytes, not of the %d the image declares", s.statePath(uid), st.Received, st.Size, size)
	}
	return st, nil
}

// received is how many bytes of uid's upload the node holds, checked; an
// upload not expected is errNotExpected.
func (s *stager) received(uid string) (int64, error) {
	s.mu.Lock()
	size, ok := s.expected[uid]
	s.mu.Unlock()
	if !ok {
		return 0, errNotExpected
	}
	st, err := s.state(uid, size)
	return st.Received, err
}

// whole is uid's file as staged, once it is all in.
func (s *stager) whole(uid string) (driver.UploadedFile, error) {
	b, err := os.ReadFile(s.statePath(uid))
	if err != nil {
		return driver.UploadedFile{}, err
	}
	var st stagedState
	if err := json.Unmarshal(b, &st); err != nil {
		return driver.UploadedFile{}, fmt.Errorf("%s: %w", s.statePath(uid), err)
	}
	if st.Received != st.Size {
		return driver.UploadedFile{}, fmt.Errorf("%d of the file's %d bytes are in", st.Received, st.Size)
	}
	return driver.UploadedFile{Path: s.path(uid), Offset: driver.UploadOffset, Size: st.Size}, nil
}

// sum is the sha256 of uid's whole file, in hex, once it is all in.
func (s *stager) sum(uid string, size int64) (string, bool, error) {
	st, err := s.state(uid, size)
	if err != nil || st.Received != size {
		return "", false, err
	}
	h, err := restoreHash(st.Hash)
	if err != nil {
		return "", false, err
	}
	return hex.EncodeToString(h.Sum(nil)), true, nil
}

func restoreHash(state []byte) (hash.Hash, error) {
	h := sha256.New()
	if err := h.(encoding.BinaryUnmarshaler).UnmarshalBinary(state); err != nil {
		return nil, fmt.Errorf("the upload's kept hash: %w", err)
	}
	return h, nil
}

// write writes a chunk of uid's upload, length bytes from body, at
// offset, which must be what the node holds. It counts only once its
// sha256, in hex, matches and it is synced; otherwise what the node holds
// stays as it was. It returns what the node then holds.
func (s *stager) write(ctx context.Context, uid string, offset, length int64, sha string, body io.Reader) (int64, error) {
	size, ctx, done, err := s.begin(ctx, uid)
	if err != nil {
		return 0, err
	}
	defer done()
	st, err := s.state(uid, size)
	if err != nil {
		return 0, err
	}
	switch {
	case offset != st.Received:
		return st.Received, &offsetError{st.Received}
	case length <= 0 || length > size-offset:
		return st.Received, fmt.Errorf("%w: %d bytes at %d of a file of %d", errChunkShape, length, offset, size)
	}
	whole, err := restoreHash(st.Hash)
	if err != nil {
		return st.Received, err
	}
	f, err := os.OpenFile(s.path(uid), os.O_WRONLY|os.O_CREATE, 0o600)
	if err != nil {
		return st.Received, err
	}
	defer func() { _ = f.Close() }()
	part := sha256.New()
	n, err := io.Copy(io.MultiWriter(io.NewOffsetWriter(f, driver.UploadOffset+offset), part, whole), io.LimitReader(ctxReader{ctx, body}, length))
	switch {
	case err != nil && (errors.Is(err, unix.ENOSPC) || errors.Is(err, unix.EDQUOT)):
		return st.Received, fmt.Errorf("%w: %w", errNoRoom, err)
	case err != nil:
		return st.Received, err
	case n < length:
		return st.Received, fmt.Errorf("%w: it ended after %d of its %d bytes", errChunkShape, n, length)
	}
	// Only a clean end counts: a stream cut there, as the apiserver cuts a
	// chunk it refuses, fails here before the handler's context ends.
	switch extra, err := body.Read(make([]byte, 1)); {
	case extra > 0:
		return st.Received, fmt.Errorf("%w: it holds more than its %d bytes", errChunkShape, length)
	case err != nil && !errors.Is(err, io.EOF):
		return st.Received, err
	}
	if got := hex.EncodeToString(part.Sum(nil)); got != strings.ToLower(sha) {
		return st.Received, errChunkDigest
	}
	if offset+length == size {
		// Whole: zeros after it, whatever earlier writes left there.
		if err := f.Truncate(driver.UploadOffset + size); err != nil {
			return st.Received, err
		}
		if err := f.Truncate(driver.UploadedLength(size)); err != nil {
			return st.Received, err
		}
	}
	if err := f.Sync(); err != nil {
		return st.Received, err
	}
	hb, err := whole.(encoding.BinaryMarshaler).MarshalBinary()
	if err != nil {
		return st.Received, err
	}
	next := stagedState{Size: size, Received: offset + length, Hash: hb}
	b, err := json.Marshal(next)
	if err != nil {
		return st.Received, err
	}
	if err := s.keep(ctx, uid, b); err != nil {
		return st.Received, err
	}
	s.counted(uid, next.Received == size)
	return next.Received, nil
}

// keep writes uid's state unless stop has ended the write: under mu, so
// a remove after stop finds whatever was kept, and leaves nothing behind.
func (s *stager) keep(ctx context.Context, uid string, state []byte) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if ctx.Err() != nil {
		return context.Cause(ctx)
	}
	return writeFileAtomic(s.statePath(uid), state)
}

// errNoRoom is a chunk the images dataset has no room for.
var errNoRoom = errors.New("no room for the upload on its node")

// begin takes uid's upload for one chunk, and returns its size, a context
// remove ends, and what gives it back.
func (s *stager) begin(ctx context.Context, uid string) (int64, context.Context, func(), error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	size, ok := s.expected[uid]
	if !ok {
		return 0, nil, nil, errNotExpected
	}
	if _, busy := s.writing[uid]; busy {
		return 0, nil, nil, errChunkBusy
	}
	ctx, cancel := context.WithCancelCause(ctx)
	s.writing[uid] = cancel
	return size, ctx, func() {
		cancel(nil)
		s.mu.Lock()
		defer s.mu.Unlock()
		delete(s.writing, uid)
	}, nil
}

// stop has the node take no more chunks of uid's upload, and ends the
// one being written, if any.
func (s *stager) stop(uid string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.expected, uid)
	if cancel, ok := s.writing[uid]; ok {
		cancel(errNotExpected)
	}
}

// expecting lists the uids of the uploads the node takes chunks of.
func (s *stager) expecting() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]string, 0, len(s.expected))
	for uid := range s.expected {
		out = append(out, uid)
	}
	return out
}

// remove stops uid's upload and removes its file and state.
func (s *stager) remove(uid string) error {
	s.stop(uid)
	var errs []error
	for _, p := range []string{s.statePath(uid), s.path(uid)} {
		if err := os.Remove(p); err != nil && !errors.Is(err, fs.ErrNotExist) {
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}

// staged lists the uids of the uploads whose file or state is kept.
func (s *stager) staged() ([]string, error) {
	entries, err := os.ReadDir(s.dir)
	if err != nil {
		return nil, err
	}
	seen := map[string]bool{}
	var out []string
	for _, e := range entries {
		uid := strings.TrimSuffix(e.Name(), ".state")
		if strings.HasPrefix(uid, ".") || seen[uid] {
			continue
		}
		seen[uid] = true
		out = append(out, uid)
	}
	return out, nil
}

// ctxReader ends a read once ctx does.
type ctxReader struct {
	ctx context.Context
	r   io.Reader
}

func (c ctxReader) Read(p []byte) (int, error) {
	if err := c.ctx.Err(); err != nil {
		return 0, context.Cause(c.ctx)
	}
	return c.r.Read(p)
}

// writeFileAtomic writes a file whole, synced, or not at all.
func writeFileAtomic(path string, data []byte) error {
	f, err := os.CreateTemp(filepath.Dir(path), "."+filepath.Base(path)+".*")
	if err != nil {
		return err
	}
	_, err = f.Write(data)
	if err == nil {
		err = f.Sync()
	}
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if err == nil {
		err = os.Rename(f.Name(), path)
	}
	if err != nil {
		_ = os.Remove(f.Name())
	}
	return err
}
