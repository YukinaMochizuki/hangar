package fake

import (
	"context"
	"fmt"
	"time"

	"github.com/YukinaMochizuki/hangar/internal/guest/console"
	"github.com/YukinaMochizuki/hangar/internal/node/driver"
	"github.com/YukinaMochizuki/hangar/internal/node/driver/consolelog"
)

// MaxLog bounds what a fake console log holds, in payload bytes; past it
// the oldest records go, as a real log's rotated file does.
const MaxLog = 1 << 20

// consoleLog is a machine's console log. Its records are numbered from
// the first ever written, so a follower knows where it is when old ones
// go.
type consoleLog struct {
	records []console.Record
	// first is the number of records[0]; boot, the last boot numbered.
	first int
	bytes int
	boot  uint64
	// headBoot is the boot records[0] is of, as each file of a real log
	// starts with its boot.
	headBoot uint64
}

// logOf returns the machine's log, made if need be. Callers hold h.mu.
func (h *Host) logOf(machineUID string) *consoleLog {
	l, ok := h.logs[machineUID]
	if !ok {
		l = &consoleLog{}
		h.logs[machineUID] = l
	}
	return l
}

// writeLog appends a record to the machine's log and wakes its
// followers. Callers hold h.mu.
func (h *Host) writeLog(machineUID string, rec console.Record) {
	l := h.logOf(machineUID)
	if rec.Stream == console.Boot {
		l.boot, _ = console.BootOf(rec)
	}
	if len(l.records) == 0 {
		l.headBoot = l.boot
	}
	l.records = append(l.records, rec)
	l.bytes += len(rec.Payload)
	for l.bytes > MaxLog && len(l.records) > 1 {
		if l.records[0].Stream == console.Boot {
			l.headBoot, _ = console.BootOf(l.records[0])
		}
		l.bytes -= len(l.records[0].Payload)
		l.records = l.records[1:]
		l.first++
	}
	close(h.logChanged)
	h.logChanged = make(chan struct{})
}

// boot marks a new boot of the machine's VM in its log. Callers hold h.mu.
func (h *Host) boot(machineUID string) {
	h.writeLog(machineUID, console.BootRecord(h.logOf(machineUID).boot+1, time.Now()))
	h.writeLog(machineUID, console.Record{Stream: console.Init, Time: time.Now(), Payload: []byte("hangar-init: starting the container\n")})
}

// Print writes data on the console of the machine's running VM, as its
// container's stream s.
func (h *Host) Print(machineUID string, s console.Stream, data string) error {
	h.mu.Lock()
	defer h.mu.Unlock()
	if v, ok := h.vms[machineUID]; !ok || v.State != driver.VMRunning {
		return fmt.Errorf("no running VM for machine %s", machineUID)
	}
	h.writeLog(machineUID, console.Record{Stream: s, Time: time.Now(), Payload: []byte(data)})
	return nil
}

// WriteRecord writes rec on the machine's console as it is, as a guest
// forging records can.
func (h *Host) WriteRecord(machineUID string, rec console.Record) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if rec.Stream == console.Boot { // the host drops it, as the launcher does
		return
	}
	h.writeLog(machineUID, rec)
}

// LogReaders returns how many reads of console logs are under way.
func (h *Host) LogReaders() int {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.logReaders
}

func (d hypervisor) ReadLog(ctx context.Context, machineUID string, opts driver.LogOptions, send func([]driver.LogRecord) error) error {
	d.h.mu.Lock()
	d.h.logReaders++
	d.h.mu.Unlock()
	defer func() {
		d.h.mu.Lock()
		d.h.logReaders--
		d.h.mu.Unlock()
	}()
	sel := consolelog.New(opts)
	batch := consolelog.NewBatch(send)
	// next is the number of the next record to read of log.
	var log *consoleLog
	next := 0
	// read takes what log holds from next, or from the start of a log
	// made since.
	read := func() ([]console.Record, <-chan struct{}) {
		d.h.mu.Lock()
		defer d.h.mu.Unlock()
		l := d.h.logs[machineUID]
		if l == nil {
			return nil, d.h.logChanged
		}
		var recs []console.Record
		if l != log || next < l.first {
			log, next = l, l.first
			recs = append(recs, console.BootRecord(l.headBoot, time.Time{}))
		}
		recs = append(recs, l.records[next-l.first:]...)
		next = l.first + len(l.records)
		return recs, d.h.logChanged
	}
	recs, changed := read()
	for _, rec := range recs {
		if r, ok := sel.Add(rec); ok {
			if err := batch.Add(r); err != nil {
				return err
			}
		}
	}
	for _, r := range sel.EndBacklog() {
		if err := batch.Add(r); err != nil {
			return err
		}
	}
	if err := batch.Flush(); err != nil {
		return err
	}
	if !opts.Follow {
		return nil
	}
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-changed:
		}
		recs, changed = read()
		for _, rec := range recs {
			if r, ok := sel.Add(rec); ok {
				if err := batch.Add(r); err != nil {
					return err
				}
			}
		}
		if err := batch.Flush(); err != nil {
			return err
		}
	}
}
