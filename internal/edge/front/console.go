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

// consoleCSP is the console's Content-Security-Policy. The console is a
// built bundle of files from its own origin, with no inline script or
// style, and it calls only its own origin, the API's paths there included.
// No other site may frame it, so none can make a click on it look like a
// click on theirs.
const consoleCSP = "default-src 'self'; script-src 'self'; style-src 'self'; " +
	"img-src 'self' data:; font-src 'self'; connect-src 'self'; object-src 'none'; " +
	"base-uri 'none'; form-action 'self'; frame-ancestors 'none'"

// serveConsole serves the console's files from ConsoleDir. The directory is
// opened at each request, so a release that replaces it is served at once.
func (f *Front) serveConsole(w http.ResponseWriter, r *http.Request) {
	h := w.Header()
	h.Set("Content-Security-Policy", consoleCSP)
	// The apiserver believes the session cookie only on a request whose
	// Origin is the console's, and a browser sends a form's real Origin only
	// if the page's referrer policy lets it (docs/decisions/authentication.md).
	// same-origin still sends no referrer to any other site.
	h.Set("Referrer-Policy", "same-origin")
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
