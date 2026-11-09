package agent

import (
	"testing"

	"connectrpc.com/connect"

	guestv1 "github.com/YukinaMochizuki/hangar/gen/go/hangar/guest/v1"
)

const meminfo = `MemTotal:         493532 kB
MemFree:          301212 kB
MemAvailable:     402100 kB
Buffers:            2048 kB
Cached:           104000 kB
HugePages_Total:       0
Hugepagesize:       2048 kB
`

func TestParseMeminfo(t *testing.T) {
	m, err := ParseMeminfo([]byte(meminfo))
	if err != nil {
		t.Fatal(err)
	}
	if want := (Memory{Total: 493532 << 10, Available: 402100 << 10}); m != want {
		t.Errorf("parsed %+v, want %+v", m, want)
	}
	for name, in := range map[string]string{
		"no MemAvailable":         "MemTotal: 1024 kB\nMemFree: 512 kB\n",
		"no MemTotal":             "MemAvailable: 1024 kB\n",
		"not kB":                  "MemTotal: 1024 MB\nMemAvailable: 512 kB\n",
		"not a number":            "MemTotal: lots kB\nMemAvailable: 512 kB\n",
		"negative":                "MemTotal: -1 kB\nMemAvailable: 512 kB\n",
		"overflowing bytes":       "MemTotal: 18014398509481984 kB\nMemAvailable: 512 kB\n",
		"more available than all": "MemTotal: 512 kB\nMemAvailable: 1024 kB\n",
		"empty":                   "",
	} {
		if m, err := ParseMeminfo([]byte(in)); err == nil {
			t.Errorf("%s: parsed %+v, want an error", name, m)
		}
	}
}

// TestMemoryUsage: a guest that cannot say its memory says so, which the
// node takes as one from before it could.
func TestMemoryUsage(t *testing.T) {
	req := connect.NewRequest(&guestv1.MemoryUsageRequest{})
	if _, err := (&Server{}).MemoryUsage(t.Context(), req); connect.CodeOf(err) != connect.CodeUnimplemented {
		t.Errorf("without StatMemory: %v, want Unimplemented", err)
	}
	s := &Server{StatMemory: func() (Memory, error) { return Memory{Total: 1 << 30, Available: 1 << 29}, nil }}
	res, err := s.MemoryUsage(t.Context(), req)
	if err != nil {
		t.Fatal(err)
	}
	if res.Msg.GetTotal() != 1<<30 || res.Msg.GetAvailable() != 1<<29 {
		t.Errorf("answered %v", res.Msg)
	}
}
