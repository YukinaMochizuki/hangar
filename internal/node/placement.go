package node

import (
	"bufio"
	"bytes"
	"cmp"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"

	"google.golang.org/protobuf/proto"

	computev1alpha1 "github.com/YukinaMochizuki/hangar/gen/go/hangar/compute/v1alpha1"
	"github.com/YukinaMochizuki/hangar/internal/computeapi"
)

// configuredPlacement is conf without the fields a Node may not report,
// each logged once: a bad description is no reason to stop a node.
func configuredPlacement(conf *computev1alpha1.Placement, log *slog.Logger) *computev1alpha1.Placement {
	p := &computev1alpha1.Placement{}
	if conf != nil {
		p = proto.Clone(conf).(*computev1alpha1.Placement)
	}
	for _, f := range computeapi.PlacementFields(p) {
		if err := computeapi.CheckPlacementValue(f.Name, *f.Value); err != nil {
			log.Warn("the configured placement is reported without "+f.Name, "error", err)
			*f.Value = ""
		}
	}
	return p
}

// placement is what the node reports of its host: the hardware read from
// it, hw, under what the configuration sets, conf. Nil when neither says
// anything.
func placement(hw, conf *computev1alpha1.Placement) *computev1alpha1.Placement {
	p := &computev1alpha1.Placement{
		Region:        conf.GetRegion(),
		Site:          conf.GetSite(),
		CpuModel:      cmp.Or(conf.GetCpuModel(), hw.GetCpuModel()),
		CpuGeneration: cmp.Or(conf.GetCpuGeneration(), hw.GetCpuGeneration()),
		Storage:       cmp.Or(conf.GetStorage(), hw.GetStorage()),
		Chassis:       conf.GetChassis(),
	}
	if proto.Size(p) == 0 {
		return nil
	}
	return p
}

// hardware reads the host's CPU and disks, each independently: one whose
// read fails, or reads what a Node may not report, stays as last reported.
func (a *Agent) hardware(last *computev1alpha1.Placement) *computev1alpha1.Placement {
	hw := &computev1alpha1.Placement{}
	c, err := a.cfg.CPU()
	if err == nil {
		err = cmp.Or(computeapi.CheckPlacementValue("cpu_model", c.GetCpuModel()),
			computeapi.CheckPlacementValue("cpu_generation", c.GetCpuGeneration()))
	}
	a.cpuRead.note(a.cfg.Log, "reading the host's CPU", err)
	if err != nil {
		c = last
	}
	hw.CpuModel, hw.CpuGeneration = c.GetCpuModel(), c.GetCpuGeneration()
	hw.Storage, err = a.cfg.Disks()
	if err == nil {
		err = computeapi.CheckPlacementValue("storage", hw.Storage)
	}
	a.disksRead.note(a.cfg.Log, "reading the host's disks", err)
	if err != nil {
		hw.Storage = last.GetStorage()
	}
	return hw
}

// readState is whether a read of the host failed last time, so a read that
// keeps failing is logged when it starts failing and when it reads again,
// not at every heartbeat.
type readState struct{ failing bool }

func (r *readState) note(log *slog.Logger, what string, err error) {
	switch {
	case err != nil && !r.failing:
		log.Warn(what+" failed; reporting what was last read until it works", "error", err)
	case err == nil && r.failing:
		log.Info(what + " works again")
	}
	r.failing = err != nil
}

// hostCPU reads this host's CPU model and generation.
func hostCPU() (*computev1alpha1.Placement, error) {
	cpuinfo, err := os.ReadFile("/proc/cpuinfo")
	if err != nil {
		return nil, err
	}
	c := readCPU(cpuinfo)
	return &computev1alpha1.Placement{CpuModel: c.name, CpuGeneration: c.generation()}, nil
}

// hostDisks describes this host's disks.
func hostDisks() (string, error) { return storageKinds("/sys/block") }

// cpu is the first processor in /proc/cpuinfo.
type cpu struct {
	vendor        string
	family, model int
	// name is the model name without its core count: how many cores the
	// host has is its capacity, which a placement never says.
	name string
}

// coreCount is the core count AMD ends its model names with: "16-Core
// Processor", or "64-Cores" on a Threadripper.
var coreCount = regexp.MustCompile(`\s+\d+-Cores?(\s+Processor)?$`)

// readCPU reads the first processor's entry from the contents of
// /proc/cpuinfo. What the entry lacks, as an arm64 host's lacks a model
// name, stays zero.
func readCPU(cpuinfo []byte) cpu {
	var c cpu
	s := bufio.NewScanner(bytes.NewReader(cpuinfo))
	for s.Scan() {
		line := s.Text()
		if strings.TrimSpace(line) == "" {
			if c != (cpu{}) {
				break
			}
			continue
		}
		key, value, ok := strings.Cut(line, ":")
		if !ok {
			continue
		}
		value = strings.TrimSpace(value)
		switch strings.TrimSpace(key) {
		case "vendor_id":
			c.vendor = value
		case "cpu family":
			c.family, _ = strconv.Atoi(value)
		case "model":
			c.model, _ = strconv.Atoi(value)
		case "model name":
			c.name = coreCount.ReplaceAllString(strings.Join(strings.Fields(value), " "), "")
		}
	}
	return c
}

// epycGenerations names AMD EPYC generations by CPUID family and the range
// of models it gives them. Desktop parts share some of these ranges, so only
// a CPU its model name calls EPYC is looked up.
var epycGenerations = []struct {
	family, fromModel, toModel int
	name                       string
}{
	{0x17, 0x00, 0x0f, "Zen (Naples)"},
	{0x17, 0x30, 0x3f, "Zen 2 (Rome)"},
	{0x19, 0x00, 0x0f, "Zen 3 (Milan)"},
	{0x19, 0x10, 0x1f, "Zen 4 (Genoa)"},
	{0x1a, 0x00, 0x0f, "Zen 5 (Turin)"},
}

// generation names the CPU's microarchitecture and code name; empty for one
// the table does not know, whose generation only the configuration gives.
func (c cpu) generation() string {
	if c.vendor != "AuthenticAMD" || !strings.Contains(c.name, "EPYC") {
		return ""
	}
	for _, g := range epycGenerations {
		if c.family == g.family && c.model >= g.fromModel && c.model <= g.toModel {
			return g.name
		}
	}
	return ""
}

// storageKinds counts the physical disks under sysBlock, /sys/block on a
// host, by kind, such as "2 × NVMe SSD"; what counts as a disk is in
// docs/decisions/machine-placement.md.
func storageKinds(sysBlock string) (string, error) {
	entries, err := os.ReadDir(sysBlock)
	if err != nil {
		return "", err
	}
	seen := map[string]bool{}
	count := map[string]int{}
	for _, e := range entries {
		dir := filepath.Join(sysBlock, e.Name())
		dev, err := filepath.EvalSymlinks(filepath.Join(dir, "device"))
		if errors.Is(err, os.ErrNotExist) {
			continue
		}
		if err != nil {
			return "", err
		}
		if readFlag(filepath.Join(dir, "removable")) || seen[dev] {
			continue
		}
		seen[dev] = true
		count[diskKind(e.Name(), readFlag(filepath.Join(dir, "queue", "rotational")))]++
	}
	kinds := make([]string, 0, len(count))
	for k := range count {
		kinds = append(kinds, k)
	}
	slices.SortFunc(kinds, func(a, b string) int { return cmp.Or(count[b]-count[a], strings.Compare(a, b)) })
	parts := make([]string, len(kinds))
	for i, k := range kinds {
		parts[i] = fmt.Sprintf("%d × %s", count[k], k)
	}
	return strings.Join(parts, ", "), nil
}

// diskKind is what a disk named name in /sys/block is.
func diskKind(name string, rotational bool) string {
	switch {
	case strings.HasPrefix(name, "nvme"):
		return "NVMe SSD"
	case strings.HasPrefix(name, "vd"), strings.HasPrefix(name, "xvd"):
		return "virtual disk"
	case rotational:
		return "HDD"
	}
	return "SSD"
}

// readFlag reads a sysfs file holding 0 or 1; a missing one is 0.
func readFlag(path string) bool {
	b, err := os.ReadFile(path)
	return err == nil && strings.TrimSpace(string(b)) == "1"
}
