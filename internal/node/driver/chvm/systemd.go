package chvm

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"

	sd "github.com/coreos/go-systemd/v22/dbus"
	"github.com/godbus/dbus/v5"

	"github.com/YukinaMochizuki/hangar/internal/node/driver"
)

// systemd is what the driver asks of systemd, for its own units only.
type systemd interface {
	// start starts a transient service and waits for its job.
	start(ctx context.Context, name string, props []sd.Property) error
	// stop stops a unit and waits for its job; stopping what is not
	// loaded is not an error.
	stop(ctx context.Context, name string) error
	// resetFailed forgets a unit's failure, so its name can be used again.
	resetFailed(ctx context.Context, name string) error
	// list lists the units loaded whose names start with unitPrefix.
	list(ctx context.Context) ([]unitState, error)
	close()
}

// unitState is a unit as systemd has it.
type unitState struct {
	Name string
	// Active is its ActiveState, such as active or activating.
	Active string
	// Started is when its main process last started.
	Started time.Time
}

// Backoff of the restart policy: a VM is started again after a second,
// then longer each time, up to a minute.
const (
	restartFirst = time.Second
	restartSteps = 6
	restartMax   = time.Minute
)

func usec(d time.Duration) uint64 { return uint64(d / time.Microsecond) }

// unitProps are the properties of a VM's unit, which systemd gives
// stopTimeout to stop. Explicit stops never restart a unit, whatever its
// Restart=.
func unitProps(spec driver.VMSpec, argv []string, restart string, stopTimeout time.Duration) []sd.Property {
	prop := func(name string, v any) sd.Property { return sd.Property{Name: name, Value: dbus.MakeVariant(v)} }
	return []sd.Property{
		sd.PropDescription(fmt.Sprintf("Hangar VM of machine %s (%s)", spec.Machine, spec.MachineUID)),
		// systemd takes the flag go-systemd calls uncleanIsFailure as
		// ExecStart='s "-": true would make every exit a success, and
		// Restart=on-failure would never restart.
		sd.PropExecStart(argv, false),
		sd.PropType("exec"),
		prop("Restart", restart),
		prop("RestartUSec", usec(restartFirst)),
		prop("RestartSteps", uint32(restartSteps)),
		prop("RestartMaxDelayUSec", usec(restartMax)),
		// Never give up restarting, as Docker does not.
		prop("StartLimitIntervalUSec", uint64(0)),
		// SIGTERM to the launcher alone, which stops the guest and Cloud
		// Hypervisor; the rest of the unit is killed once it has exited.
		prop("KillMode", "mixed"),
		prop("TimeoutStopUSec", usec(stopTimeout)),
		// At host shutdown the VM stops before the host's network does
		// (networkd, and nftables.service flushing the ruleset, which
		// holds the networks' SNAT), so a container has its network for
		// its grace. Its disks and directory, and the networks'
		// namespaces, are mounts, which the default dependencies already
		// keep until it has stopped.
		prop("After", []string{"network.target"}),
		// An exited VM's record is its exit file: the unit goes.
		prop("CollectMode", "inactive-or-failed"),
	}
}

// dbusSystemd is systemd over D-Bus. It follows the driver's units and
// calls onChange when one changes state, and when it had to connect again,
// having maybe missed a change.
type dbusSystemd struct {
	onChange func()
	// life is the connections' context, ended by close: godbus closes a
	// connection once the context it was dialled with ends, so a
	// request's or a dial's own would end it with them.
	life context.Context
	end  context.CancelFunc
	mu   sync.Mutex
	conn *sd.Conn
	// connDone ends the forwarding of conn's updates.
	connDone chan struct{}
	// connects counts the connections made, for the testbed.
	connects int
	once     sync.Once
}

// dialTimeout bounds connecting to systemd again from the watch.
const dialTimeout = 10 * time.Second

func dialSystemd(ctx context.Context, onChange func()) (*dbusSystemd, error) {
	life, end := context.WithCancel(context.Background())
	s := &dbusSystemd{onChange: onChange, life: life, end: end}
	if _, err := s.connect(ctx); err != nil {
		end()
		return nil, err
	}
	go s.watch()
	return s, nil
}

// dialWithin dials with the context life, which the connection keeps, but
// gives up when bound ends first; a connection made after that is closed.
func dialWithin[C interface{ Close() }](bound, life context.Context, dial func(context.Context) (C, error)) (C, error) {
	type result struct {
		c   C
		err error
	}
	got := make(chan result, 1)
	go func() {
		c, err := dial(life)
		got <- result{c, err}
	}()
	select {
	case r := <-got:
		return r.c, r.err
	case <-bound.Done():
		go func() {
			if r := <-got; r.err == nil {
				r.c.Close()
			}
		}()
		var none C
		return none, bound.Err()
	}
}

// connect connects, or connects again once the connection is lost; ctx
// bounds only the dial.
func (s *dbusSystemd) connect(ctx context.Context) (*sd.Conn, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.life.Err() != nil {
		return nil, errors.New("the driver is closed")
	}
	if s.conn != nil && s.conn.Connected() {
		return s.conn, nil
	}
	if s.conn != nil {
		s.conn.Close()
		close(s.connDone)
		s.conn = nil
	}
	c, err := dialWithin(ctx, s.life, sd.NewWithContext)
	if err != nil {
		return nil, err
	}
	s.connects++
	if err := c.Subscribe(); err != nil {
		c.Close()
		return nil, err
	}
	updates := make(chan *sd.PropertiesUpdate, 256)
	errs := make(chan error, 16)
	c.SetPropertiesSubscriber(updates, errs)
	connDone := make(chan struct{})
	go func() {
		for {
			select {
			case u := <-updates:
				if _, ok := uidOfUnit(u.UnitName); ok && changedState(u.Changed) {
					s.onChange()
				}
			case <-errs:
				// An update was dropped: it may have been one of ours.
				s.onChange()
			case <-connDone:
				return
			case <-s.life.Done():
				return
			}
		}
	}()
	s.conn, s.connDone = c, connDone
	return c, nil
}

func changedState(props map[string]dbus.Variant) bool {
	_, active := props["ActiveState"]
	_, sub := props["SubState"]
	return active || sub
}

// watch connects again when the connection is lost.
func (s *dbusSystemd) watch() {
	t := time.NewTicker(5 * time.Second)
	defer t.Stop()
	for {
		select {
		case <-s.life.Done():
			return
		case <-t.C:
		}
		s.mu.Lock()
		lost := s.conn == nil || !s.conn.Connected()
		s.mu.Unlock()
		if lost {
			ctx, cancel := context.WithTimeout(s.life, dialTimeout)
			if _, err := s.connect(ctx); err == nil {
				s.onChange()
			}
			cancel()
		}
	}
}

func (s *dbusSystemd) close() {
	s.once.Do(func() {
		s.end()
		s.mu.Lock()
		defer s.mu.Unlock()
		if s.conn != nil {
			s.conn.Close()
		}
	})
}

// job waits for a job's result.
func job(ctx context.Context, name string, results <-chan string) error {
	select {
	case r := <-results:
		if r != "done" {
			return fmt.Errorf("the job for %s ended %s; journalctl -u %s says why", name, r, name)
		}
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (s *dbusSystemd) start(ctx context.Context, name string, props []sd.Property) error {
	c, err := s.connect(ctx)
	if err != nil {
		return err
	}
	results := make(chan string, 1)
	if _, err := c.StartTransientUnitContext(ctx, name, "fail", props, results); err != nil {
		return err
	}
	return job(ctx, name, results)
}

// notLoaded reports systemd's error for a unit it does not have.
func notLoaded(err error) bool {
	var e dbus.Error
	return errors.As(err, &e) && (e.Name == "org.freedesktop.systemd1.NoSuchUnit" ||
		e.Name == "org.freedesktop.systemd1.LoadFailed") ||
		err != nil && strings.Contains(err.Error(), "not loaded")
}

func (s *dbusSystemd) stop(ctx context.Context, name string) error {
	c, err := s.connect(ctx)
	if err != nil {
		return err
	}
	results := make(chan string, 1)
	if _, err := c.StopUnitContext(ctx, name, "replace", results); err != nil {
		if notLoaded(err) {
			return nil
		}
		return err
	}
	return job(ctx, name, results)
}

func (s *dbusSystemd) resetFailed(ctx context.Context, name string) error {
	c, err := s.connect(ctx)
	if err != nil {
		return err
	}
	if err := c.ResetFailedUnitContext(ctx, name); err != nil && !notLoaded(err) {
		return err
	}
	return nil
}

func (s *dbusSystemd) list(ctx context.Context) ([]unitState, error) {
	c, err := s.connect(ctx)
	if err != nil {
		return nil, err
	}
	// By pattern: asking for a unit by name would load it.
	units, err := c.ListUnitsByPatternsContext(ctx, nil, []string{unitPrefix + "*.service"})
	if err != nil {
		return nil, err
	}
	var out []unitState
	for _, u := range units {
		if _, ok := uidOfUnit(u.Name); !ok || u.LoadState == "not-found" {
			continue
		}
		st := unitState{Name: u.Name, Active: u.ActiveState}
		if running(st) {
			p, err := c.GetServicePropertyContext(ctx, u.Name, "ExecMainStartTimestamp")
			if err != nil {
				return nil, err
			}
			if us, ok := p.Value.Value().(uint64); ok && us > 0 {
				st.Started = time.UnixMicro(int64(us))
			}
		}
		out = append(out, st)
	}
	return out, nil
}
