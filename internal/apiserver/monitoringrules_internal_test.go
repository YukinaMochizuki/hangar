package apiserver

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"sigs.k8s.io/yaml"
)

// rulesDir holds the host's alerting rules, which the overview's and the
// series' thresholds mirror.
const rulesDir = "../../deploy/host/monitoring/rules"

type alertRule struct {
	Alert  string            `json:"alert"`
	Expr   string            `json:"expr"`
	Labels map[string]string `json:"labels"`
}

// readRules returns a rules file's alerting rules by name, each expression
// with its whitespace collapsed, none inside parentheses, and false when the
// file is not there.
func readRules(t *testing.T, file string) (map[string]alertRule, bool) {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(rulesDir, file))
	if errors.Is(err, fs.ErrNotExist) {
		return nil, false
	}
	if err != nil {
		t.Fatal(err)
	}
	var f struct {
		Groups []struct {
			Rules []alertRule `json:"rules"`
		} `json:"groups"`
	}
	if err := yaml.Unmarshal(data, &f); err != nil {
		t.Fatalf("%s: %v", file, err)
	}
	out := map[string]alertRule{}
	for _, g := range f.Groups {
		for _, r := range g.Rules {
			if r.Alert != "" {
				r.Expr = strings.NewReplacer("( ", "(", " )", ")").Replace(strings.Join(strings.Fields(r.Expr), " "))
				out[r.Alert] = r
			}
		}
	}
	return out, true
}

func num(v float64) string { return strconv.FormatFloat(v, 'f', -1, 64) }

// TestThresholdsMatchTheRules: each threshold the overview and the series
// stand at is the one in the alerting rule it names, with that rule's
// severity, so a rule changed without them, or they without it, fails here.
func TestThresholdsMatchTheRules(t *testing.T) {
	five := 5 * time.Minute
	for file, want := range map[string][]struct {
		alert, severity string
		// whole is the rule's expression entire; else it contains each of
		// parts.
		whole string
		parts []string
	}{
		"host.yml": {
			{alert: "CertificateExpiresSoon", severity: "critical", parts: []string{"< " + num(certCriticalDays) + " * 86400"}},
			{alert: "CertificateRenewalLate", severity: "warning", parts: []string{num(certCriticalDays) + " * 86400 <=", "< " + num(certWarningDays) + " * 86400"}},
			{alert: "HostMemoryLow", severity: "critical", parts: []string{"< " + num(memoryLowBytes)}},
			{alert: "ZpoolAlmostFull", severity: "critical", parts: []string{"hangar_zpool_allocated_bytes / hangar_zpool_size_bytes > " + num(poolCriticalUsed)}},
			{alert: "ZpoolFilling", severity: "warning", parts: []string{"> " + num(poolWarningUsed) + " <= " + num(poolCriticalUsed)}},
			{alert: "ImagesQuotaAlmostFull", severity: "critical", whole: qImagesUsed + " > " + num(imagesCriticalUsed)},
			{alert: "MdArrayDegraded", severity: "critical", whole: qMdDegraded},
			{alert: "SmartWearHigh", severity: "warning", whole: qSmartWear + " > " + num(diskWearWarning)},
			{alert: "DiskTemperatureHigh", severity: "warning", whole: qSmartTemperature + " > " + num(diskTempWarning)},
			{alert: "ClockNotSynchronised", severity: "warning", parts: []string{"abs(node_timex_offset_seconds) > " + num(clockOffsetWarning)}},
			{alert: "UnitNotActive", severity: "critical", parts: []string{`node_systemd_unit_state{state="active"} == 0`}},
			{alert: "UnitRestartingOften", severity: "warning", parts: []string{"offset 1h > " + num(unitRestartsWarning)}},
		},
		"services.yml": {
			{alert: "ApiErrorsHigh", severity: "critical", whole: withWindow(apiErrorCallsRate, five) + " / " + withWindow(apiCallsRate, five) + " > " + num(apiErrorRateCritical)},
			{alert: "ApiLatencyHigh", severity: "warning", whole: withWindow(apiLatencyP99, five) + " > " + num(apiLatencyWarning)},
			{alert: "NodeNotReady", severity: "critical", whole: `hangar_apiserver_node_ready{status!="True"} == 1`},
			{alert: "NodeTunnelDown", severity: "critical", whole: `hangar_node_tunnel_connected == 0`},
			{alert: "MachinesFailedByThePlatform", severity: "critical",
				whole: `sum by (reason) (hangar_apiserver_machines{reason=~"` + strings.Join(platformReasons, "|") + `"}) > 0`},
		},
	} {
		t.Run(file, func(t *testing.T) {
			rules, ok := readRules(t, file)
			if !ok {
				t.Skipf("no %s on this branch", file)
			}
			for _, w := range want {
				r, ok := rules[w.alert]
				switch {
				case !ok:
					t.Errorf("%s has no rule %s", file, w.alert)
					continue
				case r.Labels["severity"] != w.severity:
					t.Errorf("%s is %q, the overview takes it as %s", w.alert, r.Labels["severity"], w.severity)
				case w.whole != "" && r.Expr != w.whole:
					t.Errorf("%s:\n  rule %s\n  here %s", w.alert, r.Expr, w.whole)
				}
				for _, p := range w.parts {
					if !strings.Contains(r.Expr, p) {
						t.Errorf("%s: %q has no %q", w.alert, r.Expr, p)
					}
				}
			}
		})
	}
}
