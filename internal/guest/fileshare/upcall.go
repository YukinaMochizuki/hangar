package fileshare

import (
	"encoding/hex"
	"fmt"
	"net/netip"
	"strconv"
	"strings"
	"time"
)

// The caches nfsd asks userspace to fill, by their channels in
// /proc/net/rpc. Not auth.unix.gid: unread, it fails at once and a client's
// own groups count; read and unanswered, it holds every request up.
var upcallCaches = []string{"auth.unix.ip", "nfsd.fh", "nfsd.export"}

// entryTTL is how long nfsd keeps an answer. Every export change flushes
// the caches, so it bounds only how long an answer outlives a flush that
// failed.
const entryTTL = 30 * time.Minute

// fsidNum is FSID_NUM, the filesystem id type of an export with fsid=: four
// bytes, here 0, the pseudo-root.
const fsidNum = "1"

// answer is the reply to req, read from cache's channel, under the export e
// (nil: none) of the share at root; anything e does not grant gets a reply
// nfsd caches as a refusal. ok is false for a request it cannot read.
func answer(cache, req string, e *export, root string, now time.Time) (reply string, ok bool) {
	w := words(req)
	expiry := strconv.FormatInt(now.Add(entryTTL).Unix(), 10)
	switch cache {
	case "auth.unix.ip":
		// class address -> class address expiry [domain]
		if len(w) != 2 {
			return "", false
		}
		out := []string{quote(w[0]), quote(w[1]), expiry}
		if a, err := netip.ParseAddr(w[1]); err == nil && e != nil {
			if _, listed := e.readOnly[a.Unmap()]; listed {
				out = append(out, quote(domain(a.Unmap())))
			}
		}
		return strings.Join(out, " ") + "\n", true
	case "nfsd.fh":
		// domain fsidtype fsid -> domain fsidtype fsid expiry [path]
		if len(w) != 3 {
			return "", false
		}
		out := []string{quote(w[0]), quote(w[1]), `\x` + hex.EncodeToString([]byte(w[2])), expiry}
		if _, listed := e.clientOf(w[0]); listed && w[1] == fsidNum && w[2] == "\x00\x00\x00\x00" {
			out = append(out, quote(root))
		}
		return strings.Join(out, " ") + "\n", true
	case "nfsd.export":
		// domain path -> domain path expiry [flags anonuid anongid fsid]
		if len(w) != 2 {
			return "", false
		}
		out := []string{quote(w[0]), quote(w[1]), expiry}
		if ro, listed := e.clientOf(w[0]); listed && w[1] == root {
			out = append(out, strconv.Itoa(e.flags(ro)), fmt.Sprint(e.anonUID), fmt.Sprint(e.anonGID), "0")
		}
		return strings.Join(out, " ") + "\n", true
	}
	return "", false
}

// words splits a request as the kernel's qword_get reads one: words
// separated by spaces, each either \x and hex, or text with \ooo octal
// escapes.
func words(s string) []string {
	var out []string
	for _, f := range strings.Fields(s) {
		if strings.HasPrefix(f, `\x`) {
			if b, err := hex.DecodeString(f[2:]); err == nil {
				out = append(out, string(b))
				continue
			}
		}
		var b []byte
		for i := 0; i < len(f); i++ {
			if f[i] == '\\' && i+4 <= len(f) {
				if v, err := strconv.ParseUint(f[i+1:i+4], 8, 8); err == nil {
					b = append(b, byte(v))
					i += 3
					continue
				}
			}
			b = append(b, f[i])
		}
		out = append(out, string(b))
	}
	return out
}

// quote writes a word as the kernel's qword_add does, octal-escaping what
// would end or garble it.
func quote(s string) string {
	var b strings.Builder
	for i := 0; i < len(s); i++ {
		c := s[i]
		if c <= ' ' || c == '\\' || c >= 0x7f {
			fmt.Fprintf(&b, `\%03o`, c)
			continue
		}
		b.WriteByte(c)
	}
	return b.String()
}
