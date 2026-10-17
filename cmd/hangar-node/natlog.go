package main

import (
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/YukinaMochizuki/hangar/internal/node/natlog"
)

// defaultNATLog is where the agent logs the networks' IPv4 connections,
// root's alone.
const defaultNATLog = "/var/log/hangar/nat"

// natlogMain is hangar-node natlog: which machine had a public source port
// at a time, from the NAT log (docs/runbook/host-bootstrap.md).
func natlogMain(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("hangar-node natlog", flag.ContinueOnError)
	fs.SetOutput(stderr)
	dir := fs.String("dir", defaultNATLog, "the NAT log's directory, the agent's --nat-log-dir")
	port := fs.Uint("port", 0, "the public source port the report names (required)")
	at := fs.String("at", "", "the time the report names, RFC 3339, such as 2026-10-17T09:30:00Z (required)")
	ip := fs.String("ip", "", "the public source address the report names; empty for any")
	before := fs.Duration("before", 24*time.Hour, "how long before --at a connection may have started and still be open at --at")
	after := fs.Duration("after", 5*time.Minute, "how long after --at to look too, for a reporter's clock running behind")
	asJSON := fs.Bool("json", false, "print the records as the log holds them, one JSON line each")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	t, err := time.Parse(time.RFC3339, *at)
	if err == nil && (*port == 0 || *port > 65535) {
		err = errors.New("--port is a port, 1 to 65535")
	}
	if err != nil {
		_, _ = fmt.Fprintf(stderr, "hangar-node natlog: %v\n", err)
		fs.Usage()
		return 2
	}
	recs, err := natlog.Lookup(*dir, natlog.Query{Port: uint16(*port), IP: *ip, From: t.Add(-*before), To: t.Add(*after)})
	if err != nil {
		_, _ = fmt.Fprintf(stderr, "hangar-node natlog: %v\n", err)
		return 1
	}
	oldest, err := natlog.Oldest(*dir)
	if err != nil {
		_, _ = fmt.Fprintf(stderr, "hangar-node natlog: %v\n", err)
		return 1
	}
	if *asJSON {
		enc := json.NewEncoder(stdout)
		for _, r := range recs {
			_ = enc.Encode(r)
		}
		return 0
	}
	printNATLog(stdout, recs, t, t.Add(-*before), oldest)
	return 0
}

// printNATLog prints what Lookup found for a report at t: the connections
// from the port, the last to start by t marked as the likeliest, and what
// the log may be missing of the window from on: the gaps, the days removed
// before oldest, the oldest day it holds, and what it said of them.
func printNATLog(w io.Writer, recs []natlog.Record, t, from, oldest time.Time) {
	likeliest := -1
	for i, r := range recs {
		if r.Event == natlog.EventNew && !r.Time.After(t) {
			likeliest = i
		}
	}
	tw := tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)
	_, _ = fmt.Fprintln(tw, "\tSTARTED (UTC)\tPROTO\tPUBLIC\tDESTINATION\tPROJECT\tNETWORK\tMACHINE\tINNER")
	var gaps, retentions []natlog.Record
	n := 0
	for i, r := range recs {
		switch r.Event {
		case natlog.EventGap:
			gaps = append(gaps, r)
			continue
		case natlog.EventRetention:
			retentions = append(retentions, r)
			continue
		}
		n++
		mark := ""
		if i == likeliest {
			mark = "*"
		}
		_, _ = fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\t%s\t%s\t%s\t%s\n", mark, r.Time.UTC().Format(time.RFC3339Nano), r.Protocol,
			hostPort(r.PublicIP, r.PublicPort), hostPort(r.DstIP, r.DstPort), r.Project,
			fmt.Sprintf("%s (%s)", r.Network, r.NetworkUID), fmt.Sprintf("%s (%s)", r.Machine, r.MachineUID), hostPort(r.InnerIP, r.InnerPort))
	}
	_ = tw.Flush()
	if n == 0 {
		_, _ = fmt.Fprintln(w, "no connection from that port in the window")
	} else if likeliest >= 0 {
		_, _ = fmt.Fprintln(w, "* the last to start by --at: the one the report names, unless it had ended by then")
	}
	for _, g := range gaps {
		from := "the log's start"
		if g.From != nil {
			from = g.From.UTC().Format(time.RFC3339Nano)
		}
		_, _ = fmt.Fprintf(w, "warning: connections may be missing from %s to %s: %s\n", from, g.To.UTC().Format(time.RFC3339Nano), g.Reason)
	}
	switch {
	case oldest.IsZero():
		_, _ = fmt.Fprintln(w, "warning: the log holds no day at all")
	case from.Before(oldest):
		_, _ = fmt.Fprintf(w, "warning: the log holds nothing before %s, its oldest day: the window starts %s, so connections before then are not shown (days are removed after 30 days, or sooner when the log fills its dataset)\n",
			oldest.Format("2006-01-02"), from.UTC().Format(time.RFC3339))
	}
	for _, r := range retentions {
		_, _ = fmt.Fprintf(w, "note: at %s the log held %d day(s): %s\n", r.Time.UTC().Format(time.RFC3339), r.Days, r.Reason)
	}
}

func hostPort(ip string, port uint16) string {
	if port == 0 {
		return ip
	}
	if strings.Contains(ip, ":") {
		return fmt.Sprintf("[%s]:%d", ip, port)
	}
	return fmt.Sprintf("%s:%d", ip, port)
}

// natlogCommand runs hangar-node natlog if args ask for it.
func natlogCommand(args []string) (int, bool) {
	if len(args) == 0 || args[0] != "natlog" {
		return 0, false
	}
	return natlogMain(args[1:], os.Stdout, os.Stderr), true
}
