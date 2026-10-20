package fileshare

import (
	"fmt"
	"slices"
	"testing"
	"time"

	guestv1 "github.com/YukinaMochizuki/hangar/gen/go/hangar/guest/v1"
)

func mustExport(t *testing.T, e *guestv1.FileShareExport) *export {
	t.Helper()
	pe, err := parseExport(e)
	if err != nil {
		t.Fatal(err)
	}
	return pe
}

func clients(cs ...*guestv1.FileShareClient) []*guestv1.FileShareClient { return cs }

func rw(addr string) *guestv1.FileShareClient { return &guestv1.FileShareClient{Address: addr} }
func ro(addr string) *guestv1.FileShareClient {
	return &guestv1.FileShareClient{Address: addr, ReadOnly: true}
}

var (
	squashNone = guestv1.FileShareSquash_FILE_SHARE_SQUASH_NONE
	squashRoot = guestv1.FileShareSquash_FILE_SHARE_SQUASH_ROOT
	squashAll  = guestv1.FileShareSquash_FILE_SHARE_SQUASH_ALL
)

// TestTheUpcallsAnswerTheExport: a listed address is a client of its own,
// given the share's root as fsid 0, secure and sync, without subtree checks
// (flags 0x2400), plus read-only; anything else is told no.
func TestTheUpcallsAnswerTheExport(t *testing.T) {
	now := time.Unix(1_800_000_000, 0)
	exp := fmt.Sprint(now.Add(entryTTL).Unix())
	e := mustExport(t, &guestv1.FileShareExport{
		Clients: clients(rw("172.31.1.2"), ro("172.31.2.2"), rw("2001:db8::7")),
		Squash:  squashNone, AnonUid: 65534, AnonGid: 65534,
	})
	for _, tt := range []struct {
		cache, req, want string
	}{
		{"auth.unix.ip", "nfsd 172.31.1.2\n", "nfsd 172.31.1.2 " + exp + " c-172.31.1.2\n"},
		{"auth.unix.ip", "nfsd 172.31.9.2\n", "nfsd 172.31.9.2 " + exp + "\n"},
		// The kernel writes an IPv6 address in full.
		{"auth.unix.ip", "nfsd 2001:0db8:0000:0000:0000:0000:0000:0007\n", "nfsd 2001:0db8:0000:0000:0000:0000:0000:0007 " + exp + " c-2001:db8::7\n"},
		{"auth.unix.ip", "nfsd ::ffff:172.31.1.2\n", "nfsd ::ffff:172.31.1.2 " + exp + " c-172.31.1.2\n"},
		{"nfsd.fh", `c-172.31.1.2 1 \x00000000` + "\n", `c-172.31.1.2 1 \x00000000 ` + exp + " /share\n"},
		{"nfsd.fh", `c-172.31.1.2 1 \x01000000` + "\n", `c-172.31.1.2 1 \x01000000 ` + exp + "\n"},
		{"nfsd.fh", `c-172.31.1.2 6 \x00000000` + "\n", `c-172.31.1.2 6 \x00000000 ` + exp + "\n"},
		{"nfsd.fh", `c-172.31.9.2 1 \x00000000` + "\n", `c-172.31.9.2 1 \x00000000 ` + exp + "\n"},
		{"nfsd.export", "c-172.31.1.2 /share\n", "c-172.31.1.2 /share " + exp + " 9216 65534 65534 0\n"},
		{"nfsd.export", "c-172.31.2.2 /share\n", "c-172.31.2.2 /share " + exp + " 9217 65534 65534 0\n"},
		{"nfsd.export", "c-2001:db8::7 /share\n", "c-2001:db8::7 /share " + exp + " 9216 65534 65534 0\n"},
		{"nfsd.export", "c-172.31.1.2 /\n", "c-172.31.1.2 / " + exp + "\n"},
		{"nfsd.export", "c-172.31.9.2 /share\n", "c-172.31.9.2 /share " + exp + "\n"},
		{"nfsd.export", "* /share\n", `* /share ` + exp + "\n"},
	} {
		got, ok := answer(tt.cache, tt.req, e, Root, now)
		if !ok || got != tt.want {
			t.Errorf("%s %q:\n got %q (%t)\nwant %q", tt.cache, tt.req, got, ok, tt.want)
		}
	}

	// With no export yet, every client is refused.
	for cache, req := range map[string]string{"auth.unix.ip": "nfsd 172.31.1.2\n", "nfsd.export": "c-172.31.1.2 /share\n"} {
		if got, _ := answer(cache, req, nil, Root, now); got != req[:len(req)-1]+" "+exp+"\n" {
			t.Errorf("no export, %s: %q", cache, got)
		}
	}
}

// TestTheSquashModes: Root squashes root to the anonymous ids, All every
// user; None neither.
func TestTheSquashModes(t *testing.T) {
	now := time.Unix(1_800_000_000, 0)
	for _, tt := range []struct {
		squash guestv1.FileShareSquash
		want   string
	}{
		{squashNone, "9216 1234 2345 0"},
		{squashRoot, "9220 1234 2345 0"},
		{squashAll, "9228 1234 2345 0"},
	} {
		e := mustExport(t, &guestv1.FileShareExport{Clients: clients(rw("10.0.0.2")), Squash: tt.squash, AnonUid: 1234, AnonGid: 2345})
		got, _ := answer("nfsd.export", "c-10.0.0.2 /share\n", e, Root, now)
		if want := fmt.Sprintf("c-10.0.0.2 /share %d %s\n", now.Add(entryTTL).Unix(), tt.want); got != want {
			t.Errorf("%v: %q, want %q", tt.squash, got, want)
		}
	}
}

// TestOnlyThreeCachesAreAnswered: auth.unix.gid is neither opened nor
// answered, so a client's own groups count.
func TestOnlyThreeCachesAreAnswered(t *testing.T) {
	if want := []string{"auth.unix.ip", "nfsd.fh", "nfsd.export"}; !slices.Equal(upcallCaches, want) {
		t.Errorf("the caches answered are %q, want %q", upcallCaches, want)
	}
	if got, ok := answer("auth.unix.gid", "1000\n", nil, Root, time.Now()); ok {
		t.Errorf("auth.unix.gid answered %q", got)
	}
	for cache, req := range map[string]string{"auth.unix.ip": "nfsd\n", "nfsd.fh": "c-10.0.0.2 1\n", "nfsd.export": "c-10.0.0.2\n"} {
		if got, ok := answer(cache, req, nil, Root, time.Now()); ok {
			t.Errorf("%s: a short request %q was answered %q", cache, req, got)
		}
	}
}

func TestWordsAreTheKernels(t *testing.T) {
	for _, s := range []string{"plain", "with space", `back\slash`, "tab\tnew\nline", "\x00\x01\xff"} {
		if got := words(quote(s)); len(got) != 1 || got[0] != s {
			t.Errorf("%q quoted as %q reads back as %q", s, quote(s), got)
		}
	}
	if got := words(`a \x0a0b b\040c`); !slices.Equal(got, []string{"a", "\x0a\x0b", "b c"}) {
		t.Errorf("words: %q", got)
	}
}

// badExports are exports nfsd would not take, or that name no machine.
var badExports = map[string]*guestv1.FileShareExport{
	"none":                        nil,
	"no squash":                   {Clients: clients(rw("10.0.0.2"))},
	"an unknown squash":           {Squash: 9},
	"anonymous uid (uid_t)-1":     {Squash: squashAll, AnonUid: 4294967295, AnonGid: 1000},
	"anonymous gid (gid_t)-1":     {Squash: squashRoot, AnonUid: 1000, AnonGid: 4294967295},
	"a host name":                 {Squash: squashNone, Clients: clients(rw("web"))},
	"a prefix":                    {Squash: squashNone, Clients: clients(rw("10.0.0.0/24"))},
	"a zone":                      {Squash: squashNone, Clients: clients(rw("fe80::1%eth0"))},
	"0.0.0.0":                     {Squash: squashNone, Clients: clients(rw("0.0.0.0"))},
	"::":                          {Squash: squashNone, Clients: clients(rw("::"))},
	"loopback":                    {Squash: squashNone, Clients: clients(rw("127.0.0.1"))},
	"IPv6 loopback":               {Squash: squashNone, Clients: clients(rw("::1"))},
	"multicast":                   {Squash: squashNone, Clients: clients(rw("224.0.0.1"))},
	"IPv6 multicast":              {Squash: squashNone, Clients: clients(rw("ff02::1"))},
	"broadcast":                   {Squash: squashNone, Clients: clients(rw("255.255.255.255"))},
	"broadcast, mapped":           {Squash: squashNone, Clients: clients(rw("::ffff:255.255.255.255"))},
	"a client twice":              {Squash: squashNone, Clients: clients(rw("10.0.0.2"), ro("10.0.0.2"))},
	"a client twice, IPv4-mapped": {Squash: squashNone, Clients: clients(rw("::ffff:10.0.0.2"), ro("10.0.0.2"))},
	"a good client and a bad one": {Squash: squashNone, Clients: clients(rw("10.0.0.2"), rw("0.0.0.0"))},
}

func TestAnExportIsCheckedWhole(t *testing.T) {
	for name, e := range badExports {
		if _, err := parseExport(e); err == nil {
			t.Errorf("%s: taken", name)
		}
	}
	for name, e := range map[string]*guestv1.FileShareExport{
		"no clients":          {Squash: squashNone},
		"ids up to (uid_t)-2": {Squash: squashAll, AnonUid: 4294967294, AnonGid: 0, Clients: clients(rw("10.0.0.2"), ro("2001:db8::7"))},
	} {
		if _, err := parseExport(e); err != nil {
			t.Errorf("%s: %v", name, err)
		}
	}
}
