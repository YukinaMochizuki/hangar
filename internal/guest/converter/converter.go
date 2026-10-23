// Package converter is what a converter VM's hangar-init serves the host:
// qemu-img info on a file a tenant uploaded, attached as a disk, and
// qemu-img convert of it onto a target disk, as raw. It runs only in the
// converter initramfs, which carries qemu-img; the host never parses the
// file (docs/decisions/custom-images.md). It decides nothing about the
// file: the host judges what Inspect reports.
package converter

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strings"
	"sync"

	"connectrpc.com/connect"

	guestv1 "github.com/YukinaMochizuki/hangar/gen/go/hangar/guest/v1"
	"github.com/YukinaMochizuki/hangar/gen/go/hangar/guest/v1/guestv1connect"
)

// Where a converter VM's disks are, and qemu-img.
const (
	Source  = "/dev/vdc"
	Target  = "/dev/vdd"
	QemuImg = "/usr/bin/qemu-img"
)

// Bounds on what qemu-img says, of which the answer holds a part.
const (
	maxInfo   = 1 << 20
	maxStderr = 4 << 10
)

// Server serves ImageConverterService, one qemu-img at a time.
type Server struct {
	// QemuImg, Source and Target are the program and the disks; empty
	// means the constants of the same names.
	QemuImg, Source, Target string

	mu sync.Mutex
}

var _ guestv1connect.ImageConverterServiceHandler = (*Server)(nil)

func (s *Server) paths() (qemuImg, source, target string) {
	qemuImg, source, target = s.QemuImg, s.Source, s.Target
	if qemuImg == "" {
		qemuImg = QemuImg
	}
	if source == "" {
		source = Source
	}
	if target == "" {
		target = Target
	}
	return qemuImg, source, target
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

func (s *Server) Inspect(ctx context.Context, _ *connect.Request[guestv1.InspectRequest]) (*connect.Response[guestv1.InspectResponse], error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	qemuImg, source, _ := s.paths()
	out, err := run(ctx, qemuImg, "info", "--output=json", source)
	if err != nil {
		return nil, err
	}
	var in info
	if err := json.Unmarshal(out, &in); err != nil {
		return nil, connect.NewError(connect.CodeInvalidArgument, fmt.Errorf("reading qemu-img info: %w", err))
	}
	return connect.NewResponse(&guestv1.InspectResponse{
		Format: in.Format, VirtualSize: in.VirtualSize,
		BackingFile: in.BackingFilename, DataFile: in.FormatSpecific.Data.DataFile,
	}), nil
}

func (s *Server) Convert(ctx context.Context, req *connect.Request[guestv1.ConvertRequest]) (*connect.Response[guestv1.ConvertResponse], error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	qemuImg, source, target := s.paths()
	format := req.Msg.GetFormat()
	if format != "raw" && format != "qcow2" {
		return nil, connect.NewError(connect.CodeInvalidArgument, fmt.Errorf("format %q is neither raw nor qcow2", format))
	}
	if _, err := os.Stat(target); err != nil {
		return nil, connect.NewError(connect.CodeFailedPrecondition, fmt.Errorf("no target disk: %w", err))
	}
	// The target is all zeros, so qemu-img writes none; the format is the
	// one given, never probed. The target is written past the page cache.
	if _, err := run(ctx, qemuImg, "convert", "-q", "-n", "--target-is-zero", "-t", "none", "-T", "writeback",
		"-f", format, "-O", "raw", source, target); err != nil {
		return nil, err
	}
	return connect.NewResponse(&guestv1.ConvertResponse{}), nil
}

// run runs qemu-img and returns what it wrote, at most maxInfo bytes. One
// that ran and failed is INVALID_ARGUMENT: it refused the file; with the
// tail of what it said.
func run(ctx context.Context, qemuImg string, args ...string) ([]byte, error) {
	var stdout bytes.Buffer
	stderr := &tail{max: maxStderr}
	cmd := exec.CommandContext(ctx, qemuImg, args...)
	cmd.Env = []string{"LC_ALL=C", "PATH=/usr/bin:/bin"}
	cmd.Stdout = &limited{w: &stdout, n: maxInfo}
	cmd.Stderr = stderr
	err := cmd.Run()
	var exit *exec.ExitError
	switch {
	case err == nil:
		return stdout.Bytes(), nil
	case ctx.Err() != nil:
		return nil, connect.NewError(connect.CodeDeadlineExceeded, fmt.Errorf("qemu-img %s: %w", args[0], ctx.Err()))
	case errors.As(err, &exit):
		return nil, connect.NewError(connect.CodeInvalidArgument, fmt.Errorf("qemu-img %s: %w: %s", args[0], err, strings.TrimSpace(stderr.String())))
	}
	return nil, connect.NewError(connect.CodeInternal, fmt.Errorf("qemu-img %s: %w", args[0], err))
}

// limited takes up to n bytes and drops the rest.
type limited struct {
	w io.Writer
	n int
}

func (l *limited) Write(p []byte) (int, error) {
	if l.n > 0 {
		k := min(len(p), l.n)
		l.n -= k
		_, _ = l.w.Write(p[:k])
	}
	return len(p), nil
}

// tail keeps the last max bytes written.
type tail struct {
	max int
	b   []byte
}

func (t *tail) Write(p []byte) (int, error) {
	t.b = append(t.b, p...)
	if len(t.b) > t.max {
		t.b = t.b[len(t.b)-t.max:]
	}
	return len(p), nil
}

func (t *tail) String() string { return string(t.b) }
