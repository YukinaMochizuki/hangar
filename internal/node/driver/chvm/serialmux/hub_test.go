package serialmux

import (
	"bufio"
	"bytes"
	"fmt"
	"io"
	"log/slog"
	"net"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/charmbracelet/x/vt"
	"google.golang.org/protobuf/encoding/protodelim"

	nodev1 "github.com/YukinaMochizuki/hangar/gen/go/hangar/node/v1"
	"github.com/YukinaMochizuki/hangar/internal/testutil"
)

// recorder is a writer tests read back: the log, or what reached the guest.
type recorder struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (r *recorder) Write(p []byte) (int, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.b.Write(p)
}

func (r *recorder) String() string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.b.String()
}

// testHub is a hub whose guest is the test: what it typed is guest.
func testHub(t *testing.T) (*Hub, *recorder, *recorder) {
	t.Helper()
	log, guest := &recorder{}, &recorder{}
	h := New(log, slog.New(slog.DiscardHandler))
	h.Connected(guest, false)
	t.Cleanup(h.Close)
	return h, log, guest
}

// attached is a client of a hub, as the node is.
type attached struct {
	t    *testing.T
	conn net.Conn
	r    *bufio.Reader
}

func attach(t *testing.T, h *Hub, start *nodev1.ConsoleStart) *attached {
	t.Helper()
	client, served := net.Pipe()
	go h.Serve(served)
	a := &attached{t: t, conn: client, r: bufio.NewReader(client)}
	t.Cleanup(func() { _ = client.Close() })
	a.send(&nodev1.ConsoleRequest{Message: &nodev1.ConsoleRequest_Start{Start: start}})
	return a
}

func (a *attached) send(req *nodev1.ConsoleRequest) {
	a.t.Helper()
	if _, err := protodelim.MarshalTo(a.conn, req); err != nil {
		a.t.Fatal(err)
	}
}

func (a *attached) typeIn(s string) {
	a.t.Helper()
	a.send(&nodev1.ConsoleRequest{Message: &nodev1.ConsoleRequest_Input{Input: []byte(s)}})
}

// next reads the client's next message.
func (a *attached) next() *nodev1.ConsoleResponse {
	a.t.Helper()
	_ = a.conn.SetReadDeadline(time.Now().Add(10 * time.Second))
	res := &nodev1.ConsoleResponse{}
	if err := (protodelim.UnmarshalOptions{MaxSize: MaxMessage}).UnmarshalFrom(a.r, res); err != nil {
		a.t.Fatalf("reading the console: %v", err)
	}
	return res
}

// state reads the client's next message, which must be its state.
func (a *attached) state() *nodev1.ConsoleState {
	a.t.Helper()
	res := a.next()
	if res.GetState() == nil {
		a.t.Fatalf("got %v, not a state", res)
	}
	return res.GetState()
}

// output reads the client's next message, which must be output.
func (a *attached) output() string {
	a.t.Helper()
	res := a.next()
	if _, ok := res.GetMessage().(*nodev1.ConsoleResponse_Output); !ok {
		a.t.Fatalf("got %v, not output", res)
	}
	return string(res.GetOutput())
}

// TestAnAttachIsDrawnThenSentWhatComes: a client is told its state, then
// drawn the screen as the guest left it, then sent what the guest writes
// after, each byte once; the log has it all.
func TestAnAttachIsDrawnThenSentWhatComes(t *testing.T) {
	h, log, _ := testHub(t)
	h.Output([]byte("before\r\n"))
	a := attach(t, h, &nodev1.ConsoleStart{})
	if st := a.state(); !st.GetInput() || st.GetSize().GetColumns() != DefaultColumns || st.GetSize().GetRows() != DefaultRows {
		t.Errorf("the first client's state %v, want the input at 80x24", st)
	}
	if drawn := a.output(); !bytes.Contains([]byte(drawn), []byte("before")) {
		t.Errorf("the drawing %q lacks what came before", drawn)
	}
	h.Output([]byte("after\r\n"))
	if got := a.output(); got != "after\r\n" {
		t.Errorf("then sent %q", got)
	}
	if got := log.String(); got != "before\r\nafter\r\n" {
		t.Errorf("the log holds %q", got)
	}
}

// TestAnAttachMissesAndRepeatsNothing: clients attach while the guest
// writes a count over one line as fast as it can: each is drawn the line at
// some count, and then sent every count after it, each once, in order.
func TestAnAttachMissesAndRepeatsNothing(t *testing.T) {
	const counts, clients = 600, 8
	h, _, _ := testHub(t)
	count := func(n int) string { return fmt.Sprintf("\r%08d", n) }
	written := make(chan struct{})
	go func() {
		defer close(written)
		for n := 1; n <= counts; n++ {
			h.Output([]byte(count(n)))
		}
	}()
	var wg sync.WaitGroup
	for i := range clients {
		a := attach(t, h, &nodev1.ConsoleStart{ReadOnly: true})
		wg.Go(func() {
			a.state()
			drawn := vt.NewEmulator(DefaultColumns, DefaultRows)
			go func() { _, _ = io.Copy(io.Discard, drawn) }()
			_, _ = drawn.Write([]byte(a.output()))
			line, _, _ := text(drawn)
			from := 0
			if s := strings.TrimSpace(line[0]); s != "" {
				if _, err := fmt.Sscanf(s, "%d", &from); err != nil {
					t.Errorf("client %d is drawn %q", i, line[0])
					return
				}
			}
			var want, got strings.Builder
			for n := from + 1; n <= counts; n++ {
				want.WriteString(count(n))
			}
			for got.Len() < want.Len() {
				got.WriteString(a.output())
			}
			if got.String() != want.String() {
				t.Errorf("client %d, drawn at %d, is then sent %.60q..., want %.60q...", i, from, got.String(), want.String())
			}
		})
	}
	<-written
	wg.Wait()
}

// TestOneClientTypes: the first client holds the input, a second watches,
// and what it types reaches no one; a read-only client never holds it;
// forcing takes it, telling the one that held it, whose typing then reaches
// no one; and once the holder goes, no one holds it.
func TestOneClientTypes(t *testing.T) {
	h, _, guest := testHub(t)
	first := attach(t, h, &nodev1.ConsoleStart{})
	first.state()
	first.output()
	second := attach(t, h, &nodev1.ConsoleStart{})
	if second.state().GetInput() {
		t.Error("a second client holds the input")
	}
	second.output()
	watcher := attach(t, h, &nodev1.ConsoleStart{ReadOnly: true, Force: true})
	if watcher.state().GetInput() {
		t.Error("a read-only client holds the input")
	}
	watcher.output()

	first.typeIn("ls\r")
	second.typeIn("rm -rf /\r")
	watcher.typeIn("reboot\r")
	testutil.Eventually(t, "the holder's typing at the guest", func() (bool, string) {
		return guest.String() == "ls\r", guest.String()
	})

	forcer := attach(t, h, &nodev1.ConsoleStart{Force: true})
	if !forcer.state().GetInput() {
		t.Error("forcing did not take the input")
	}
	forcer.output()
	if first.state().GetInput() {
		t.Error("the client the input was taken from is told it holds it")
	}
	first.typeIn("whoami\r")
	forcer.typeIn("id\r")
	testutil.Eventually(t, "only the new holder's typing at the guest", func() (bool, string) {
		return guest.String() == "ls\rid\r", guest.String()
	})

	_ = forcer.conn.Close()
	testutil.Eventually(t, "no one holding the input", func() (bool, string) {
		h.mu.Lock()
		defer h.mu.Unlock()
		return h.input == nil && len(h.subs) == 3, ""
	})
}

// TestTheHolderResizes: a resize from the client holding the input sets
// the screen's size, every client is told it and drawn the screen again;
// one from a watcher is not taken; a new connection to Cloud Hypervisor, as
// after a reboot, brings the default size back.
func TestTheHolderResizes(t *testing.T) {
	h, _, _ := testHub(t)
	holder := attach(t, h, &nodev1.ConsoleStart{})
	holder.state()
	holder.output()
	watcher := attach(t, h, &nodev1.ConsoleStart{ReadOnly: true})
	watcher.state()
	watcher.output()

	watcher.send(&nodev1.ConsoleRequest{Message: &nodev1.ConsoleRequest_Resize{Resize: &nodev1.ConsoleSize{Columns: 10, Rows: 5}}})
	holder.send(&nodev1.ConsoleRequest{Message: &nodev1.ConsoleRequest_Resize{Resize: &nodev1.ConsoleSize{Columns: 200, Rows: 50}}})
	for name, a := range map[string]*attached{"holder": holder, "watcher": watcher} {
		if st := a.state(); st.GetSize().GetColumns() != 200 || st.GetSize().GetRows() != 50 {
			t.Errorf("the %s is told the size %v, want 200x50", name, st.GetSize())
		}
		a.output()
	}

	h.Connected(&recorder{}, true)
	for name, a := range map[string]*attached{"holder": holder, "watcher": watcher} {
		if st := a.state(); st.GetSize().GetColumns() != DefaultColumns || st.GetSize().GetRows() != DefaultRows {
			t.Errorf("after a reboot the %s is told the size %v, want 80x24", name, st.GetSize())
		}
		a.output()
	}
}

// TestASlowClientIsLetGo: a client that stops reading is let go once it has
// fallen behind; the guest's output is never held up for it, and the other
// clients are sent all of it.
func TestASlowClientIsLetGo(t *testing.T) {
	h, _, _ := testHub(t)
	stuck := attach(t, h, &nodev1.ConsoleStart{})
	reading := attach(t, h, &nodev1.ConsoleStart{ReadOnly: true})
	// Attached once told so: what comes after is sent, not drawn.
	reading.state()
	reading.output()
	var read atomic.Int64
	done := make(chan struct{})
	go func() {
		defer close(done)
		for read.Load() < 2*queued {
			read.Add(int64(len(reading.output())))
		}
	}()
	// In batches the reading client keeps up with, which the stuck one,
	// never reading, falls behind by.
	for sent := 0; sent < 2*queued; sent += queued / 4 {
		for range queued / 4 {
			h.Output([]byte("x"))
		}
		testutil.Eventually(t, "the reading client sent the batch", func() (bool, string) {
			return read.Load() == int64(sent+queued/4), fmt.Sprintf("%d of %d", read.Load(), sent+queued/4)
		})
	}
	<-done
	testutil.Eventually(t, "the stuck client let go", func() (bool, string) {
		_ = stuck.conn.SetReadDeadline(time.Now().Add(time.Second))
		_, err := io.Copy(io.Discard, stuck.conn)
		return err == nil, "" // EOF: the hub closed it
	})
}
