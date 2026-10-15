package front

import (
	"errors"
	"io/fs"
	"net/http"
	"os"
	"path"
	"strings"
	"time"
)

// consoleIndex is the console's page, which every path that is neither a
// file nor looks like one gets: the console routes in the browser.
const consoleIndex = "index.html"

// serveConsole serves the console's files from ConsoleDir. The directory is
// opened at each request, so a release that replaces it is served at once.
func (f *Front) serveConsole(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		w.Header().Set("Allow", "GET, HEAD")
		page(w, http.StatusMethodNotAllowed, pageMethod)
		return
	}
	if f.cfg.ConsoleDir == "" {
		page(w, http.StatusServiceUnavailable, pageNoConsole)
		return
	}
	// os.Root keeps every open inside the directory, symbolic links and
	// ".." included.
	root, err := os.OpenRoot(f.cfg.ConsoleDir)
	if err != nil {
		// Not there is a host whose release has no console yet.
		if !errors.Is(err, fs.ErrNotExist) {
			f.cfg.Log.Warn("the console's directory does not open", "dir", f.cfg.ConsoleDir, "error", err)
		}
		page(w, http.StatusServiceUnavailable, pageNoConsole)
		return
	}
	defer func() { _ = root.Close() }()

	name := strings.TrimPrefix(path.Clean("/"+r.URL.Path), "/")
	file, info, err := openFile(root, name)
	if errors.Is(err, fs.ErrNotExist) && path.Ext(name) == "" {
		name = consoleIndex
		file, info, err = openFile(root, name)
		if errors.Is(err, fs.ErrNotExist) {
			page(w, http.StatusServiceUnavailable, pageNoConsole)
			return
		}
	}
	if errors.Is(err, fs.ErrNotExist) {
		page(w, http.StatusNotFound, pageNotFound)
		return
	}
	if err != nil {
		f.cfg.Log.Warn("a console file does not open", "file", name, "error", err)
		page(w, http.StatusNotFound, pageNotFound)
		return
	}
	defer func() { _ = file.Close() }()

	_ = http.NewResponseController(w).SetWriteDeadline(time.Now().Add(f.cfg.Limits.ConsoleWriteTimeout))
	h := w.Header()
	h.Set("X-Content-Type-Options", "nosniff")
	switch {
	case name == consoleIndex:
		// Always asked again, so a new release's page, and through it its
		// new assets, is what loads.
		h.Set("Cache-Control", "no-cache")
	case strings.HasPrefix(name, "assets/"):
		// Vite names each asset after its content.
		h.Set("Cache-Control", "public, max-age=31536000, immutable")
	}
	http.ServeContent(w, r, name, info.ModTime(), file)
}

// openFile opens the regular file name in root; a directory, or anything
// else that is not a regular file, is not there.
func openFile(root *os.Root, name string) (*os.File, fs.FileInfo, error) {
	if name == "" || name == "." {
		return nil, nil, fs.ErrNotExist
	}
	file, err := root.Open(name)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) || errors.Is(err, fs.ErrPermission) {
			return nil, nil, fs.ErrNotExist
		}
		return nil, nil, err
	}
	info, err := file.Stat()
	if err != nil || !info.Mode().IsRegular() {
		_ = file.Close()
		return nil, nil, fs.ErrNotExist
	}
	return file, info, nil
}
