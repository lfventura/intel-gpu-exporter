package main

import (
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
)

// Card is an Intel GPU found in sysfs. PCI is the stable identifier used as the "card" label;
// the cardN node name can change between boots.
type Card struct {
	PCI    string // e.g. 0000:03:00.0
	Driver string // i915 or xe
	Node   string // e.g. card1
	Dir    string // /sys/class/drm/card1
	Dev    string // /sys/class/drm/card1/device
}

func discoverCards(sysRoot string, only map[string]bool) []Card {
	matches, _ := filepath.Glob(filepath.Join(sysRoot, "class/drm/card*"))
	var cards []Card
	for _, dir := range matches {
		node := filepath.Base(dir)
		if strings.Contains(node, "-") { // connectors such as card1-DP-1
			continue
		}
		dev := filepath.Join(dir, "device")
		driverLink, err := os.Readlink(filepath.Join(dev, "driver"))
		if err != nil {
			continue
		}
		driver := filepath.Base(driverLink)
		if driver != "i915" && driver != "xe" {
			continue
		}
		devLink, err := os.Readlink(dev)
		if err != nil {
			continue
		}
		pci := filepath.Base(devLink)
		if len(only) > 0 && !only[pci] {
			continue
		}
		cards = append(cards, Card{PCI: pci, Driver: driver, Node: node, Dir: dir, Dev: dev})
	}
	sort.Slice(cards, func(i, j int) bool { return cards[i].PCI < cards[j].PCI })
	return cards
}

func readInt(path string) (int64, bool) {
	b, err := os.ReadFile(path)
	if err != nil {
		return 0, false
	}
	v, err := strconv.ParseInt(strings.TrimSpace(string(b)), 10, 64)
	return v, err == nil
}

func readString(path string) string {
	b, err := os.ReadFile(path)
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(b))
}

// frequency returns the actual and requested frequency of the card's primary GT in MHz.
func (c Card) frequency() (actual, requested float64, ok bool) {
	actualPath, requestedPath := filepath.Join(c.Dir, "gt_act_freq_mhz"), filepath.Join(c.Dir, "gt_cur_freq_mhz")
	if c.Driver == "xe" {
		base := filepath.Join(c.Dev, "tile0/gt0/freq0")
		actualPath, requestedPath = filepath.Join(base, "act_freq"), filepath.Join(base, "cur_freq")
	}
	a, okA := readInt(actualPath)
	r, okR := readInt(requestedPath)
	return float64(a), float64(r), okA && okR
}

// idleResidencyMs sums the idle (RC6) residency of every GT of the card.
func (c Card) idleResidencyMs() (total int64, gts int) {
	pattern := filepath.Join(c.Dir, "gt/gt*/rc6_residency_ms")
	if c.Driver == "xe" {
		pattern = filepath.Join(c.Dev, "tile*/gt*/gtidle/idle_residency_ms")
	}
	files, _ := filepath.Glob(pattern)
	if len(files) == 0 && c.Driver == "i915" {
		files, _ = filepath.Glob(filepath.Join(c.Dir, "power/rc6_residency_ms"))
	}
	for _, f := range files {
		if v, ok := readInt(f); ok {
			total += v
			gts++
		}
	}
	return total, gts
}

// energySensor is one hwmon energy counter, in microjoules.
type energySensor struct {
	Name  string // energy1_input
	Label string // hwmon label when the driver sets one, e.g. card or pkg
	Value int64
}

func (c Card) energy() []energySensor {
	files, _ := filepath.Glob(filepath.Join(c.Dev, "hwmon/hwmon*/energy*_input"))
	var out []energySensor
	for _, f := range files {
		v, ok := readInt(f)
		if !ok {
			continue
		}
		out = append(out, energySensor{
			Name:  filepath.Base(f),
			Label: readString(strings.TrimSuffix(f, "_input") + "_label"),
			Value: v,
		})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}

// vramTotalBytes returns the VRAM size: the driver's own sysfs file when it has one, else the
// largest PCI BAR, which covers the whole VRAM when resizable BAR is on. BARs under 1 GiB are
// ignored because they are only a window into VRAM.
func (c Card) vramTotalBytes() (int64, bool) {
	if v, ok := readInt(filepath.Join(c.Dir, "lmem_total_bytes")); ok && v > 0 {
		return v, true
	}
	var largest int64
	for _, line := range strings.Split(readString(filepath.Join(c.Dev, "resource")), "\n") {
		fields := strings.Fields(line)
		if len(fields) < 2 {
			continue
		}
		start, err1 := strconv.ParseUint(strings.TrimPrefix(fields[0], "0x"), 16, 64)
		end, err2 := strconv.ParseUint(strings.TrimPrefix(fields[1], "0x"), 16, 64)
		if err1 != nil || err2 != nil || end <= start {
			continue
		}
		if size := int64(end - start + 1); size > largest {
			largest = size
		}
	}
	return largest, largest >= 1<<30
}

// interruptCount sums, over all CPUs, the interrupts of the lines in /proc/interrupts that belong
// to the card: lines naming its PCI address, else lines naming its driver when it is the only
// card with that driver.
func interruptCount(procRoot string, card Card, driverIsUnique bool) (int64, bool) {
	data, err := os.ReadFile(filepath.Join(procRoot, "interrupts"))
	if err != nil {
		return 0, false
	}
	lines := strings.Split(string(data), "\n")
	if len(lines) < 2 {
		return 0, false
	}
	cpus := len(strings.Fields(lines[0]))
	count := func(match func(string) bool) (int64, bool) {
		var total int64
		found := false
		for _, line := range lines[1:] {
			fields := strings.Fields(line)
			if len(fields) < cpus+2 || !match(line) {
				continue
			}
			found = true
			for _, f := range fields[1 : cpus+1] {
				n, _ := strconv.ParseInt(f, 10, 64)
				total += n
			}
		}
		return total, found
	}
	if total, ok := count(func(l string) bool { return strings.Contains(l, card.PCI) }); ok {
		return total, true
	}
	if !driverIsUnique {
		return 0, false
	}
	return count(func(l string) bool {
		fields := strings.Fields(l)
		return len(fields) > 0 && fields[len(fields)-1] == card.Driver
	})
}
