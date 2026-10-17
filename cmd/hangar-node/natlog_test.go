package main

import (
	"strings"
	"testing"
	"time"

	"github.com/YukinaMochizuki/hangar/internal/node/natlog"
)

// TestNatlogFindsTheMachine: the runbook's command, given a report's port
// and time, names the machine whose connection started last before it, and
// warns of a window the log may be missing connections in.
func TestNatlogFindsTheMachine(t *testing.T) {
	dir := t.TempDir()
	at := time.Date(2026, 10, 17, 9, 30, 0, 0, time.UTC)
	now := at.Add(-2 * time.Hour)
	l, err := natlog.Open(dir, natlog.Options{Now: func() time.Time { return now }})
	if err != nil {
		t.Fatal(err)
	}
	rec := func(when time.Time, port uint16, machine string) natlog.Record {
		return natlog.Record{
			Time: when, Event: natlog.EventNew, Protocol: "tcp", PublicIP: "192.0.2.1", PublicPort: port,
			DstIP: "198.51.100.7", DstPort: 587, Project: "p", Network: "default", NetworkUID: "n1",
			Machine: machine, MachineUID: machine + "-uid", InnerIP: "10.0.0.2", InnerPort: 51000,
		}
	}
	for _, r := range []natlog.Record{
		rec(at.Add(-time.Hour), 40123, "earlier"),
		rec(at.Add(-time.Minute), 40123, "spammer"),
		rec(at.Add(-30*time.Second), 40124, "neighbour"),
		rec(at.Add(time.Minute), 40123, "later"),
	} {
		if err := l.Write(r); err != nil {
			t.Fatal(err)
		}
	}
	from, to := at.Add(-10*time.Minute), at.Add(-9*time.Minute)
	if err := l.Write(natlog.Record{Time: to, Event: natlog.EventGap, From: &from, To: &to, Reason: "overflowed"}); err != nil {
		t.Fatal(err)
	}
	_ = l.Close("test")

	var out, errs strings.Builder
	if code := natlogMain([]string{"--dir", dir, "--port", "40123", "--at", at.Format(time.RFC3339)}, &out, &errs); code != 0 {
		t.Fatalf("exit %d: %s", code, errs.String())
	}
	var marked []string
	for _, line := range strings.Split(out.String(), "\n") {
		if strings.HasPrefix(line, "*") && strings.Contains(line, "tcp") {
			marked = append(marked, line)
		}
	}
	if len(marked) != 1 || !strings.Contains(marked[0], "spammer (spammer-uid)") || !strings.Contains(marked[0], "192.0.2.1:40123") {
		t.Errorf("marked %q, want the spammer's connection alone:\n%s", marked, out.String())
	}
	for _, want := range []string{"earlier", "later", "warning: connections may be missing", "overflowed"} {
		if !strings.Contains(out.String(), want) {
			t.Errorf("no %q in:\n%s", want, out.String())
		}
	}
	if strings.Contains(out.String(), "neighbour") {
		t.Errorf("another port's connection is listed:\n%s", out.String())
	}

	// The log holds the 17th alone; the window, 24 h before --at, starts
	// on the 16th.
	if !strings.Contains(out.String(), "warning: the log holds nothing before 2026-10-17") {
		t.Errorf("no warning that the window starts before the oldest day:\n%s", out.String())
	}

	for _, args := range [][]string{
		{"--dir", dir, "--at", at.Format(time.RFC3339)},
		{"--dir", dir, "--port", "40123"},
		{"--dir", dir, "--port", "70000", "--at", at.Format(time.RFC3339)},
	} {
		if code := natlogMain(args, &out, &errs); code != 2 {
			t.Errorf("%q: exit %d, want 2", args, code)
		}
	}
}

// TestNatlogSaysWhatWasRemoved: a window within the days the log holds
// gets no warning of days removed, and what the log said of the days it
// removed in the window is shown.
func TestNatlogSaysWhatWasRemoved(t *testing.T) {
	dir := t.TempDir()
	at := time.Date(2026, 10, 17, 9, 30, 0, 0, time.UTC)
	l, err := natlog.Open(dir, natlog.Options{Now: func() time.Time { return at }})
	if err != nil {
		t.Fatal(err)
	}
	for _, r := range []natlog.Record{
		{Time: at.Add(-2 * time.Hour), Event: natlog.EventNew, Protocol: "tcp", PublicIP: "192.0.2.1", PublicPort: 40123, Machine: "web"},
		{Time: at.Add(-time.Hour), Event: natlog.EventRetention, Days: 12, Reason: "the log's filesystem was 90% full"},
	} {
		if err := l.Write(r); err != nil {
			t.Fatal(err)
		}
	}
	_ = l.Close("test")
	var out, errs strings.Builder
	if code := natlogMain([]string{"--dir", dir, "--port", "40123", "--at", at.Format(time.RFC3339), "--before", "3h"}, &out, &errs); code != 0 {
		t.Fatalf("exit %d: %s", code, errs.String())
	}
	if strings.Contains(out.String(), "holds nothing before") {
		t.Errorf("a window within the days held is said to start before them:\n%s", out.String())
	}
	if !strings.Contains(out.String(), "note: at 2026-10-17T08:30:00Z the log held 12 day(s)") {
		t.Errorf("the retention record in the window is not shown:\n%s", out.String())
	}
}
