package fake

import (
	"context"
	"errors"
	"fmt"
	"io"
	"slices"
	"strconv"
	"strings"
	"sync"
	"syscall"

	"github.com/YukinaMochizuki/hangar/internal/guest/console"
	"github.com/YukinaMochizuki/hangar/internal/node/driver"
)

// The fake guest's exec runs scripted programs in memory, named by the
// first argument:
//
//	cat                  copies stdin to stdout until stdin closes
//	echo ARG...          prints its arguments
//	exit CODE [OUT [ERR]] prints OUT to stdout and ERR to stderr, and exits
//	size                 prints the terminal's "ROWS COLS", reads a line of
//	                     stdin, and prints the size again
//	show [NAME...]       prints the value of each environment variable named,
//	                     then its working directory and its user, a line each
//	wait MARKER          runs until a signal or the end of the exec
//	running MARKER       prints 1 while a wait MARKER runs in the guest, else 0
//
// Any other program is not found, as a shell has it: 127. A signal ends
// any of them with 128 plus its number.

// execBuffer is how many outputs, and inputs, an exec holds unread, as a
// real one holds what flow control lets through.
const execBuffer = 16

// errGuestGone fails the execs of a VM that stopped or restarted.
var errGuestGone = errors.New("the guest went away")

// Execs returns how many exec processes run in the machine's guest.
func (h *Host) Execs(machineUID string) int {
	h.mu.Lock()
	defer h.mu.Unlock()
	return len(h.execs[machineUID])
}

// endExecsLocked ends every exec of a machine, as its guest going does.
// Callers hold h.mu.
func (h *Host) endExecsLocked(machineUID string) {
	for p := range h.execs[machineUID] {
		p.fail(errGuestGone)
	}
	delete(h.execs, machineUID)
	h.endConsoleLocked(machineUID)
}

func (d guest) Exec(ctx context.Context, machineUID string, spec driver.ExecSpec) (driver.ExecSession, error) {
	if len(spec.Process.Command) == 0 {
		return nil, errors.New("no program to run")
	}
	d.h.mu.Lock()
	defer d.h.mu.Unlock()
	if v, ok := d.h.vms[machineUID]; !ok || v.State != driver.VMRunning {
		return nil, errUnknownVM
	}
	if d.h.silent[machineUID] || d.h.hung[machineUID] {
		return nil, fmt.Errorf("the guest agent of machine %s does not answer", machineUID)
	}
	ctx, cancel := context.WithCancel(ctx)
	p := &process{
		h: d.h, uid: machineUID, spec: spec, cancel: cancel,
		out: make(chan driver.ExecOutput, execBuffer), input: make(chan any, execBuffer), done: make(chan struct{}),
	}
	if d.h.execs == nil {
		d.h.execs = map[string]map[*process]bool{}
	}
	if d.h.execs[machineUID] == nil {
		d.h.execs[machineUID] = map[*process]bool{}
	}
	d.h.execs[machineUID][p] = true
	go p.run(ctx)
	return p, nil
}

// process is a fake exec's process. Its program reads input and writes
// output through channels; the exec's end, or the guest's, ends it.
type process struct {
	h      *Host
	uid    string
	spec   driver.ExecSpec
	cancel context.CancelFunc

	out   chan driver.ExecOutput
	input chan any // []byte, stdinClosed, driver.WindowSize or syscall.Signal
	done  chan struct{}

	// killedBy is the signal that ended the program, if one did.
	killedBy syscall.Signal

	mu    sync.Mutex
	err   error // why the exec failed, if it did
	ended bool  // Recv has returned the exit or the failure
}

type stdinClosed struct{}

func (p *process) fail(err error) {
	p.mu.Lock()
	if p.err == nil {
		p.err = err
	}
	p.mu.Unlock()
	p.cancel()
}

func (p *process) run(ctx context.Context) {
	defer close(p.done)
	code := p.program(ctx)
	p.h.mu.Lock()
	delete(p.h.execs[p.uid], p)
	p.h.mu.Unlock()
	if ctx.Err() != nil {
		return
	}
	exit := console.ExitStatus{Code: int32(code), Signal: int32(p.killedBy)}
	select {
	case p.out <- driver.ExecOutput{Exit: &exit}:
	case <-ctx.Done():
	}
}

// program runs the program and returns its exit code; it returns early,
// with any code, once ctx ends.
func (p *process) program(ctx context.Context) int {
	args := p.spec.Process.Command
	write := func(stderr bool, s string) bool {
		o := driver.ExecOutput{Data: []byte(s), Stderr: stderr && !p.spec.TTY}
		select {
		case p.out <- o:
			return true
		case <-ctx.Done():
			return false
		}
	}
	// next returns the next input, or the signal that ended the program,
	// or nil once the exec has ended.
	next := func() any {
		select {
		case in := <-p.input:
			return in
		case <-ctx.Done():
			return nil
		}
	}
	signalled := func(in any) (int, bool) {
		if sig, ok := in.(syscall.Signal); ok {
			p.killedBy = sig
			return 128 + int(sig), true
		}
		return 0, in == nil
	}
	switch args[0] {
	case "cat":
		for {
			switch in := next().(type) {
			case []byte:
				if !write(false, string(in)) {
					return 0
				}
			case stdinClosed:
				return 0
			default:
				if code, end := signalled(in); end {
					return code
				}
			}
		}
	case "echo":
		write(false, strings.Join(args[1:], " ")+"\n")
		return 0
	case "exit":
		code, err := strconv.Atoi(get(args, 1))
		if err != nil {
			write(true, "exit: "+err.Error()+"\n")
			return 2
		}
		if out := get(args, 2); out != "" && !write(false, out) {
			return 0
		}
		if errOut := get(args, 3); errOut != "" {
			write(true, errOut)
		}
		return code
	case "size":
		if !p.spec.TTY {
			write(true, "size: not a terminal\n")
			return 1
		}
		size := p.spec.Size
		if !write(false, fmt.Sprintf("%d %d\n", size.Rows, size.Cols)) {
			return 0
		}
		for line := false; !line; {
			switch in := next().(type) {
			case driver.WindowSize:
				size = in
			case []byte:
				line = strings.Contains(string(in), "\n")
			case stdinClosed:
				return 1
			default:
				if code, end := signalled(in); end {
					return code
				}
			}
		}
		write(false, fmt.Sprintf("%d %d\n", size.Rows, size.Cols))
		return 0
	case "show":
		var b strings.Builder
		for _, name := range args[1:] {
			value := ""
			for _, kv := range p.spec.Process.Env {
				if v, ok := strings.CutPrefix(kv, name+"="); ok {
					value = v
				}
			}
			b.WriteString(value + "\n")
		}
		fmt.Fprintf(&b, "%s\n%s\n", p.spec.Process.WorkingDir, p.spec.Process.User)
		write(false, b.String())
		return 0
	case "wait":
		for {
			if code, end := signalled(next()); end {
				return code
			}
		}
	case "running":
		p.h.mu.Lock()
		n := 0
		for q := range p.h.execs[p.uid] {
			if slices.Equal(q.spec.Process.Command, []string{"wait", get(args, 1)}) {
				n = 1
			}
		}
		p.h.mu.Unlock()
		write(false, fmt.Sprintf("%d\n", n))
		return 0
	}
	write(true, args[0]+": not found\n")
	return 127
}

func get(args []string, i int) string {
	if i < len(args) {
		return args[i]
	}
	return ""
}

func (p *process) send(in any) error {
	select {
	case p.input <- in:
		return nil
	case <-p.done:
		return errors.New("the exec has ended")
	}
}

func (p *process) Stdin(b []byte) error             { return p.send(append([]byte(nil), b...)) }
func (p *process) CloseStdin() error                { return p.send(stdinClosed{}) }
func (p *process) Resize(s driver.WindowSize) error { return p.send(s) }
func (p *process) Signal(sig syscall.Signal) error  { return p.send(sig) }

func (p *process) Recv() (driver.ExecOutput, error) {
	p.mu.Lock()
	ended := p.ended
	p.mu.Unlock()
	if ended {
		return driver.ExecOutput{}, io.EOF
	}
	var o driver.ExecOutput
	select {
	case o = <-p.out:
	case <-p.done:
		select {
		case o = <-p.out:
		default:
			// The program ended by the exec's end, or the guest's.
			p.mu.Lock()
			defer p.mu.Unlock()
			p.ended = true
			if p.err != nil {
				return driver.ExecOutput{}, p.err
			}
			return driver.ExecOutput{}, context.Canceled
		}
	}
	if o.Exit != nil {
		p.mu.Lock()
		p.ended = true
		p.mu.Unlock()
		p.cancel()
	}
	return o, nil
}
