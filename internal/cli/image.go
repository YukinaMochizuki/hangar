package cli

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
	"os"
	"time"

	"connectrpc.com/connect"
	"github.com/spf13/cobra"

	metav1 "github.com/YukinaMochizuki/hangar/gen/go/hangar/meta/v1"
	"github.com/YukinaMochizuki/hangar/gen/go/hangar/registry"
	storagev1alpha1 "github.com/YukinaMochizuki/hangar/gen/go/hangar/storage/v1alpha1"
	"github.com/YukinaMochizuki/hangar/internal/apimachinery/conditions"
	"github.com/YukinaMochizuki/hangar/internal/client"
	"github.com/YukinaMochizuki/hangar/internal/computeapi"
	"github.com/YukinaMochizuki/hangar/internal/uploadwire"
)

// Retries of a chunk that failed on the way, as a dropped connection or a
// node restarting: after a second, then twice as long, up to half a
// minute, at most uploadTries in all.
const (
	uploadBackoff    = time.Second
	uploadMaxBackoff = 30 * time.Second
	uploadTries      = 8
)

func (a *app) imageCmd() *cobra.Command {
	cmd := &cobra.Command{Use: "image", Short: "Work with the project's own disk images, CustomImages"}
	cmd.AddCommand(a.imageUploadCmd())
	return cmd
}

func (a *app) imageUploadCmd() *cobra.Command {
	var name, osName, version, user, output string
	var noVerify bool
	cmd := &cobra.Command{
		Use:   "upload FILE --name NAME",
		Short: "Upload a raw or qcow2 disk image file as a CustomImage",
		Long: `Upload a raw or qcow2 disk image file as a CustomImage of the project.

The CustomImage NAME is made with the file's size and its sha256, which
the node checks the whole file against; --no-verify sends none and skips
reading the file twice. One of the name already made by an upload of a
file of the same size and sha256, and still awaiting it, is uploaded to
again: running the same command after it was interrupted goes on from
where its node is. The file goes in chunks, each checked by the node and
tried again on failure. Once the whole file is in, the node converts it
in a VM of its own, and the image becomes Ready; follow it with
hangar get customimage NAME.

A progress line goes to stderr; with -o json nothing does, and the image
is printed as JSON once its file is in.`,
		Args: args(1, 1),
		RunE: func(cmd *cobra.Command, args []string) error {
			if name == "" {
				return usageErr("--name is required")
			}
			if output != "" && output != "json" {
				return usageErr("upload prints json or nothing, not %q", output)
			}
			return a.upload(cmd.Context(), args[0], name, osName, version, user, !noVerify, output == "json")
		},
	}
	cmd.Flags().StringVar(&name, "name", "", "the CustomImage's name")
	cmd.Flags().StringVar(&osName, "os", "", "what the image holds, such as nixos, for people choosing one")
	cmd.Flags().StringVar(&version, "version", "", "its version, such as 25.05")
	cmd.Flags().StringVar(&user, "default-user", "", "the user its cloud-init makes, whom hangar ssh logs in as")
	cmd.Flags().BoolVar(&noVerify, "no-verify", false, "send no sha256 of the whole file; each chunk is checked all the same")
	cmd.Flags().StringVarP(&output, "output", "o", "", "json to print the image as JSON, and no progress")
	return cmd
}

func (a *app) upload(ctx context.Context, path, name, osName, version, user string, verify, quiet bool) error {
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	defer func() { _ = f.Close() }()
	fi, err := f.Stat()
	if err != nil {
		return err
	}
	if !fi.Mode().IsRegular() || fi.Size() == 0 {
		return usageErr("%s is not a file with something in it", path)
	}
	size := fi.Size()
	progress := func(format string, args ...any) {
		if !quiet {
			_, _ = fmt.Fprintf(a.io.Err, "\r"+format, args...)
		}
	}
	sum := ""
	if verify {
		h := sha256.New()
		if _, err := io.Copy(h, f); err != nil {
			return fmt.Errorf("reading %s: %w", path, err)
		}
		sum = hex.EncodeToString(h.Sum(nil))
	}
	cl, c, err := a.client()
	if err != nil {
		return err
	}
	project, err := a.projectFor(registry.CustomImage, c)
	if err != nil {
		return err
	}
	ci, err := a.uploadTarget(ctx, cl, project, name, size, sum, osName, version, user)
	if err != nil {
		return err
	}
	creds, err := a.credentials(c)
	if err != nil {
		return err
	}
	hc, base := a.execHTTPClient(c)
	u := &uploader{hc: hc, url: base + uploadwire.Path(project, name), creds: creds, sleep: a.sleep}
	if !conditions.IsTrue(ci.GetStatus().GetConditions(), computeapi.CustomImageUploaded) {
		st, err := u.state(ctx)
		if err != nil {
			return err
		}
		chunk := st.ChunkSize
		if chunk <= 0 || chunk > uploadwire.ChunkSize {
			chunk = uploadwire.ChunkSize
		}
		buf := make([]byte, chunk)
		at := st.ReceivedBytes
		for at < size {
			progress("uploading %s: %s of %s (%d%%)", name, humanBytes(at), humanBytes(size), at*100/size)
			n := min(chunk, size-at)
			if _, err := f.ReadAt(buf[:n], at); err != nil {
				return fmt.Errorf("reading %s: %w", path, err)
			}
			if at, err = u.put(ctx, at, buf[:n], size); err != nil {
				if !quiet {
					_, _ = fmt.Fprintln(a.io.Err)
				}
				return err
			}
		}
		progress("uploading %s: %s of %s (100%%)\n", name, humanBytes(size), humanBytes(size))
	}
	if !quiet {
		_, _ = fmt.Fprintf(a.io.Out, "customimage/%s uploaded; its node converts it: hangar get customimage %s\n", name, name)
		return nil
	}
	obj, err := cl.Get(ctx, registry.CustomImage, client.Key{Project: project, Name: name})
	if err != nil {
		return err
	}
	js, err := marshalJSON(obj)
	if err != nil {
		return err
	}
	var out bytes.Buffer
	if err := json.Indent(&out, js, "", "  "); err != nil {
		return err
	}
	_, err = fmt.Fprintln(a.io.Out, out.String())
	return err
}

// uploadTarget makes the CustomImage name of the file, or returns the one
// of the name made for the same file, awaiting its upload or uploaded. Any
// other is a conflict.
func (a *app) uploadTarget(ctx context.Context, cl *client.Client, project, name string, size int64, sum, osName, version, user string) (*storagev1alpha1.CustomImage, error) {
	res, err := cl.Create(ctx, registry.CustomImage, &storagev1alpha1.CustomImage{
		Metadata: &metav1.ObjectMeta{Project: project, Name: name},
		Spec: &storagev1alpha1.CustomImageSpec{
			Source: &storagev1alpha1.CustomImageSource{Source: &storagev1alpha1.CustomImageSource_Upload{
				Upload: &storagev1alpha1.CustomImageUploadSource{Size: size, Sha256: sum},
			}},
			Os: osName, Version: version, DefaultUser: user,
		},
	})
	if err == nil {
		return res.Object.(*storagev1alpha1.CustomImage), nil
	}
	if connect.CodeOf(err) != connect.CodeAlreadyExists {
		return nil, err
	}
	obj, err := cl.Get(ctx, registry.CustomImage, client.Key{Project: project, Name: name})
	if err != nil {
		return nil, err
	}
	ci := obj.(*storagev1alpha1.CustomImage)
	up := ci.GetSpec().GetSource().GetUpload()
	conflict := func(why string) error {
		return &exitError{code: ExitConflict, err: fmt.Errorf("customimage/%s exists, and %s; delete it, or upload under another --name", name, why)}
	}
	switch {
	case up == nil:
		return nil, conflict("is captured from a disk, not uploaded")
	case up.GetSize() != size:
		return nil, conflict(fmt.Sprintf("is of a file of %d bytes, not %d", up.GetSize(), size))
	case up.GetSha256() != sum:
		return nil, conflict("is of a file of another sha256 (or one checked where this is not, or the other way)")
	case computeapi.CustomImageFailed(ci):
		c := conditions.Find(ci.GetStatus().GetConditions(), computeapi.CustomImageReady)
		return nil, conflict(fmt.Sprintf("failed (%s: %s)", c.GetReason(), c.GetMessage()))
	case ci.GetMetadata().GetDeleteTime() != nil:
		return nil, conflict("is being deleted")
	}
	return ci, nil
}

// uploader sends an upload's chunks to its path.
type uploader struct {
	hc    *http.Client
	url   string
	creds credentials
	sleep func(context.Context, time.Duration) error
}

func (u *uploader) do(ctx context.Context, method string, set http.Header, body []byte) (uploadwire.State, error) {
	req, err := http.NewRequestWithContext(ctx, method, u.url, bytes.NewReader(body))
	if err != nil {
		return uploadwire.State{}, err
	}
	if body == nil {
		req.Body, req.ContentLength = http.NoBody, 0
	}
	for k, v := range set {
		req.Header[k] = v
	}
	if err := u.creds.set(ctx, req.Header.Set); err != nil {
		return uploadwire.State{}, err
	}
	resp, err := u.hc.Do(req)
	if err != nil {
		return uploadwire.State{}, connect.NewError(connect.CodeUnavailable, err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return uploadwire.State{}, uploadwire.ReadError(resp)
	}
	var st uploadwire.State
	if err := json.NewDecoder(io.LimitReader(resp.Body, 4<<10)).Decode(&st); err != nil {
		return uploadwire.State{}, connect.NewError(connect.CodeUnavailable, fmt.Errorf("reading the answer: %w", err))
	}
	return st, nil
}

// state asks where the upload is, retrying as put does.
func (u *uploader) state(ctx context.Context) (uploadwire.State, error) {
	var st uploadwire.State
	err := u.retry(ctx, func() error {
		var err error
		st, err = u.do(ctx, http.MethodGet, nil, nil)
		return err
	})
	return st, err
}

// put sends the chunk at at of the file of size, and returns where the
// upload is then: past the chunk, or where the node says it is, as after a
// chunk an earlier run sent counted though its answer was lost.
func (u *uploader) put(ctx context.Context, at int64, chunk []byte, size int64) (int64, error) {
	sum := sha256.Sum256(chunk)
	set := http.Header{
		"Content-Range":        {uploadwire.ContentRange(at, int64(len(chunk)), size)},
		uploadwire.ChunkSHA256: {hex.EncodeToString(sum[:])},
		"Content-Type":         {"application/octet-stream"},
	}
	next := at
	err := u.retry(ctx, func() error {
		st, err := u.do(ctx, http.MethodPut, set, chunk)
		var off *uploadwire.OffsetError
		if errors.As(err, &off) {
			next = off.ReceivedBytes
			return nil
		}
		next = st.ReceivedBytes
		return err
	})
	return next, err
}

// retry runs try until it succeeds, fails for good, or has failed
// uploadTries times, waiting longer between each.
func (u *uploader) retry(ctx context.Context, try func() error) error {
	wait := uploadBackoff
	var err error
	for n := 1; ; n++ {
		if err = try(); err == nil || !retriable(err) || n == uploadTries {
			return err
		}
		if serr := u.sleep(ctx, wait); serr != nil {
			return errors.Join(err, serr)
		}
		wait = min(2*wait, uploadMaxBackoff)
	}
}

// retriable says a chunk may go through if tried again: the way to the
// node failed, a chunk was garbled on the way, or too many calls were
// open. What the server refuses for good, it says again.
func retriable(err error) bool {
	switch connect.CodeOf(err) {
	case connect.CodeUnavailable, connect.CodeResourceExhausted, connect.CodeAborted, connect.CodeInternal,
		connect.CodeUnknown, connect.CodeDeadlineExceeded:
		return !errors.Is(err, uploadwire.TooLarge)
	case connect.CodeInvalidArgument:
		// A chunk whose bytes were garbled on the way.
		return bytes.Contains([]byte(err.Error()), []byte("sha256 does not match"))
	}
	return false
}

// sleep waits d, or until ctx ends.
func (a *app) sleep(ctx context.Context, d time.Duration) error {
	if a.opts.UploadBackoff > 0 {
		d = d * a.opts.UploadBackoff / uploadBackoff
	}
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}

// humanBytes is n in binary units, such as 1.5 GiB.
func humanBytes(n int64) string {
	const unit = 1 << 10
	if n < unit {
		return fmt.Sprintf("%d B", n)
	}
	div, exp := int64(unit), 0
	for m := n / unit; m >= unit; m /= unit {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.1f %ciB", float64(n)/float64(div), "KMGTPE"[exp])
}
