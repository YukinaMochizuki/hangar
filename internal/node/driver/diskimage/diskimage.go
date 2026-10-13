// Package diskimage is the host's DiskImages driver: it downloads a disk
// image over HTTPS into a directory of its own, checks its sha256 as it
// comes and its size against a bound, asks qemu-img what it holds, and has
// qemu-img write it raw onto a block device. qemu-img reads only images a
// platform administrator chose, so it runs as root without a sandbox; only
// raw and qcow2 are taken, naming no backing or data file. It keeps nothing
// once the image is closed: a node that made a base needs the file no more.
// See docs/decisions/vm-images.md.
package diskimage

import (
	"bytes"
	"cmp"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"hash"
	"io"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"regexp"
	"strings"

	"golang.org/x/sys/unix"

	"github.com/YukinaMochizuki/hangar/internal/node/driver"
	"github.com/YukinaMochizuki/hangar/internal/node/driver/registry"
)

// Bounds on an image unless Config says otherwise: what is downloaded, and
// the disk it holds, which its base takes on the pool.
const (
	DefaultMaxFileSize = 16 << 30
	DefaultMaxDiskSize = 256 << 30
)

// Config configures the driver.
type Config struct {
	// Dir is where images are downloaded to, root's alone; the driver owns
	// all of it. Put it on the image dataset, whose quota bounds it.
	Dir string
	// MaxFileSize and MaxDiskSize bound the file downloaded and the disk it
	// holds; past them an image is ErrImageTooLarge. Zero means the
	// defaults.
	MaxFileSize, MaxDiskSize int64
	// Transport reaches the servers. Nil means a default one connecting
	// within Timeouts; either way an answer or a body making no progress
	// within Timeouts fails the fetch.
	Transport http.RoundTripper
	Timeouts  registry.Timeouts
	// QemuImg is the qemu-img program. Empty means qemu-img on PATH.
	QemuImg string
}

// Images is the driver.
type Images struct {
	cfg    Config
	client *http.Client
}

// New makes the directory, root's alone, and clears what an interrupted
// fetch left in it.
func New(cfg Config) (*Images, error) {
	cfg.MaxFileSize = cmp.Or(cfg.MaxFileSize, DefaultMaxFileSize)
	cfg.MaxDiskSize = cmp.Or(cfg.MaxDiskSize, DefaultMaxDiskSize)
	cfg.QemuImg = cmp.Or(cfg.QemuImg, "qemu-img")
	if err := os.MkdirAll(cfg.Dir, 0o700); err != nil {
		return nil, err
	}
	if err := os.Chmod(cfg.Dir, 0o700); err != nil {
		return nil, err
	}
	left, err := os.ReadDir(cfg.Dir)
	if err != nil {
		return nil, err
	}
	for _, e := range left {
		if err := os.RemoveAll(cfg.Dir + "/" + e.Name()); err != nil {
			return nil, err
		}
	}
	tr := registry.Bounded(cfg.Transport, cfg.Timeouts, "image server")
	return &Images{cfg: cfg, client: &http.Client{
		Transport: tr,
		// A redirect may lead only to another https URL, as mirrors do.
		CheckRedirect: func(req *http.Request, via []*http.Request) error {
			if req.URL.Scheme != "https" {
				return fmt.Errorf("refusing a redirect to %s", req.URL.Redacted())
			}
			if len(via) >= 10 {
				return errors.New("too many redirects")
			}
			return nil
		},
	}}, nil
}

var digestRE = regexp.MustCompile(`^sha256:([0-9a-f]{64})$`)

func (d *Images) Fetch(ctx context.Context, rawURL, digest string) (driver.FetchedImage, error) {
	u, err := url.Parse(rawURL)
	if err != nil || u.Scheme != "https" || u.Host == "" {
		return nil, fmt.Errorf("%q is not an https URL: %w", rawURL, driver.ErrInvalidImage)
	}
	m := digestRE.FindStringSubmatch(digest)
	if m == nil {
		return nil, fmt.Errorf("%q is not a sha256 digest", digest)
	}
	f, err := os.CreateTemp(d.cfg.Dir, "image-")
	if err != nil {
		return nil, err
	}
	img := &fetched{d: d, path: f.Name()}
	err = d.download(ctx, u, m[1], f)
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if err == nil {
		err = img.inspect(ctx)
	}
	if err != nil {
		_ = img.Close()
		return nil, err
	}
	return img, nil
}

// download writes what u serves to f, failing unless it is at most the
// bound and its sha256 is want, in hex.
func (d *Images) download(ctx context.Context, u *url.URL, want string, f *os.File) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u.String(), nil)
	if err != nil {
		return err
	}
	resp, err := d.client.Do(req)
	if err != nil {
		return fmt.Errorf("fetching %s: %w", u.Redacted(), err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("fetching %s: %s", u.Redacted(), resp.Status)
	}
	if resp.ContentLength > d.cfg.MaxFileSize {
		return fmt.Errorf("%s is %d bytes, more than %d: %w", u.Redacted(), resp.ContentLength, d.cfg.MaxFileSize, driver.ErrImageTooLarge)
	}
	if resp.ContentLength > 0 {
		var st unix.Statfs_t
		if err := unix.Fstatfs(int(f.Fd()), &st); err != nil {
			return err
		}
		if free := int64(st.Bavail) * st.Bsize; free < resp.ContentLength {
			return fmt.Errorf("%s is %d bytes, %d are free: %w", u.Redacted(), resp.ContentLength, free, driver.ErrNoSpace)
		}
	}
	h := sha256.New()
	n, err := io.Copy(io.MultiWriter(f, h), io.LimitReader(resp.Body, d.cfg.MaxFileSize+1))
	switch {
	case err != nil && isNoSpace(err):
		return fmt.Errorf("downloading %s: %w: %w", u.Redacted(), err, driver.ErrNoSpace)
	case err != nil:
		return fmt.Errorf("downloading %s: %w", u.Redacted(), err)
	case n > d.cfg.MaxFileSize:
		return fmt.Errorf("%s is more than %d bytes: %w", u.Redacted(), d.cfg.MaxFileSize, driver.ErrImageTooLarge)
	}
	if got := sum(h); got != want {
		return fmt.Errorf("%s has sha256 %s, not %s: %w", u.Redacted(), got, want, driver.ErrDigestMismatch)
	}
	return nil
}

func sum(h hash.Hash) string { return hex.EncodeToString(h.Sum(nil)) }

func isNoSpace(err error) bool { return errors.Is(err, unix.ENOSPC) || errors.Is(err, unix.EDQUOT) }

// fetched is a downloaded image, checked, until closed.
type fetched struct {
	d      *Images
	path   string
	format string
	size   int64
}

// info is what qemu-img info --output=json says, as far as it is read.
type info struct {
	Format          string `json:"format"`
	VirtualSize     int64  `json:"virtual-size"`
	BackingFilename string `json:"backing-filename"`
	FormatSpecific  struct {
		Data struct {
			DataFile string `json:"data-file"`
		} `json:"data"`
	} `json:"format-specific"`
}

// inspect asks qemu-img what the file holds and refuses what the host does
// not take: a format other than raw or qcow2, or one that would have
// qemu-img read another file.
func (i *fetched) inspect(ctx context.Context) error {
	out, err := i.d.qemuImg(ctx, "info", "--output=json", i.path)
	if err != nil && ctx.Err() != nil {
		return fmt.Errorf("%w: %w", err, ctx.Err())
	}
	if exit := (*exec.ExitError)(nil); errors.As(err, &exit) {
		// qemu-img ran and refused the file; one that could not run says
		// nothing of it.
		return fmt.Errorf("%w: %w", err, driver.ErrInvalidImage)
	}
	if err != nil {
		return err
	}
	var in info
	if err := json.Unmarshal(out, &in); err != nil {
		return fmt.Errorf("reading qemu-img info: %w: %w", err, driver.ErrInvalidImage)
	}
	switch {
	case in.Format != "raw" && in.Format != "qcow2":
		return fmt.Errorf("the image is %q, not raw or qcow2: %w", in.Format, driver.ErrInvalidImage)
	case in.BackingFilename != "" || in.FormatSpecific.Data.DataFile != "":
		return fmt.Errorf("the image names another file: %w", driver.ErrInvalidImage)
	case in.VirtualSize <= 0:
		return fmt.Errorf("the image holds a disk of %d bytes: %w", in.VirtualSize, driver.ErrInvalidImage)
	case in.VirtualSize > i.d.cfg.MaxDiskSize:
		return fmt.Errorf("the image holds a disk of %d bytes, more than %d: %w", in.VirtualSize, i.d.cfg.MaxDiskSize, driver.ErrImageTooLarge)
	}
	i.format, i.size = in.Format, in.VirtualSize
	return nil
}

func (i *fetched) Size() int64 { return i.size }

func (i *fetched) WriteTo(ctx context.Context, dev string) error {
	// The device is all zeros, so qemu-img writes none; the format is the
	// one inspected, never probed again.
	_, err := i.d.qemuImg(ctx, "convert", "-q", "-n", "--target-is-zero", "-f", i.format, "-O", "raw", i.path, dev)
	return err
}

func (i *fetched) Close() error {
	if err := os.Remove(i.path); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	return nil
}

func (d *Images) qemuImg(ctx context.Context, args ...string) ([]byte, error) {
	var stdout, stderr bytes.Buffer
	cmd := exec.CommandContext(ctx, d.cfg.QemuImg, args...)
	cmd.Env = append(os.Environ(), "LC_ALL=C")
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	if err := cmd.Run(); err != nil {
		return nil, fmt.Errorf("qemu-img %s: %w: %s", args[0], err, strings.TrimSpace(stderr.String()))
	}
	return stdout.Bytes(), nil
}
