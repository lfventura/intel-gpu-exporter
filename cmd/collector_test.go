package main

import (
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/testutil"
)

// fakeHost builds a /proc and /sys tree with one xe card (like an Arc Pro B50) and one i915 card
// (like an Arc A310), so the collector runs against the same files it reads on a real host.
type fakeHost struct {
	t    *testing.T
	proc string
	sys  string
}

func newFakeHost(t *testing.T) *fakeHost {
	root := t.TempDir()
	h := &fakeHost{t: t, proc: filepath.Join(root, "proc"), sys: filepath.Join(root, "sys")}
	h.card("card1", "0000:03:00.0", "xe", 16<<30)
	h.card("card0", "0000:14:00.0", "i915", 4<<30)
	h.write("devices/0000:03:00.0/tile0/gt0/freq0/act_freq", "2267")
	h.write("devices/0000:03:00.0/tile0/gt0/freq0/cur_freq", "2600")
	h.write("devices/0000:03:00.0/hwmon/hwmon3/energy1_input", "0")
	h.write("devices/0000:03:00.0/hwmon/hwmon3/energy1_label", "card")
	h.write("devices/0000:03:00.0/hwmon/hwmon3/energy2_input", "1000000000")
	h.write("devices/0000:03:00.0/hwmon/hwmon3/energy2_label", "pkg")
	h.write("devices/0000:14:00.0/hwmon/hwmon2/energy1_input", "500000000")
	h.write("class/drm/card0/gt_act_freq_mhz", "1350")
	h.write("class/drm/card0/gt_cur_freq_mhz", "2450")
	h.idle(0, 0)
	h.interrupts(0, 0)
	return h
}

// interrupts writes /proc/interrupts with two CPUs: the B50 line names its PCI address, the A310
// line only its driver, as older kernels print.
func (h *fakeHost) interrupts(b50, a310 int64) {
	content := fmt.Sprintf(`           CPU0       CPU1
  0:         40          0   IO-APIC   2-edge      timer
170:   %10d   %10d  IR-PCI-MSI-0000:03:00.0    0-edge      xe
171:   %10d   %10d  IR-PCI-MSI-0000:14:00.0    0-edge      i915
LOC:   1000   1000   Local timer interrupts
`, b50/2, b50-b50/2, a310/2, a310-a310/2)
	if err := os.MkdirAll(h.proc, 0o755); err != nil {
		h.t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(h.proc, "interrupts"), []byte(content), 0o644); err != nil {
		h.t.Fatal(err)
	}
}

func (h *fakeHost) write(rel, content string) {
	path := filepath.Join(h.sys, rel)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		h.t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content+"\n"), 0o644); err != nil {
		h.t.Fatal(err)
	}
}

func (h *fakeHost) card(node, pci, driver string, bar int64) {
	dev := filepath.Join(h.sys, "devices", pci)
	drv := filepath.Join(h.sys, "bus/pci/drivers", driver)
	for _, dir := range []string{dev, drv, filepath.Join(h.sys, "class/drm")} {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			h.t.Fatal(err)
		}
	}
	cardDir := filepath.Join(h.sys, "class/drm", node)
	if err := os.MkdirAll(cardDir, 0o755); err != nil {
		h.t.Fatal(err)
	}
	if err := os.Symlink(dev, filepath.Join(cardDir, "device")); err != nil {
		h.t.Fatal(err)
	}
	if err := os.Symlink(drv, filepath.Join(dev, "driver")); err != nil {
		h.t.Fatal(err)
	}
	h.write("devices/"+pci+"/resource", fmt.Sprintf(
		"0x00000000f0000000 0x00000000f0ffffff 0x0000000000040200\n0x0000004000000000 0x%016x 0x000000000014220c\n0x0000000000000000 0x0000000000000000 0x0000000000000000",
		0x0000004000000000+bar-1))
	if err := os.MkdirAll(filepath.Join(h.sys, "class/drm", node+"-DP-1"), 0o755); err != nil {
		h.t.Fatal(err)
	}
}

func (h *fakeHost) idle(gt0, gt1 int64) {
	h.write("devices/0000:03:00.0/tile0/gt0/gtidle/idle_residency_ms", fmt.Sprint(gt0))
	h.write("devices/0000:03:00.0/tile0/gt1/gtidle/idle_residency_ms", fmt.Sprint(gt1))
	h.write("class/drm/card0/gt/gt0/rc6_residency_ms", fmt.Sprint(gt0*2))
}

func (h *fakeHost) energy(b50, a310 int64) {
	h.write("devices/0000:03:00.0/hwmon/hwmon3/energy2_input", fmt.Sprint(b50))
	h.write("devices/0000:14:00.0/hwmon/hwmon2/energy1_input", fmt.Sprint(a310))
}

// process creates a process with one open DRM node per fdinfo body.
func (h *fakeHost) process(pid int, comm string, fdinfos ...string) {
	base := filepath.Join(h.proc, fmt.Sprint(pid))
	for _, dir := range []string{"fd", "fdinfo"} {
		if err := os.MkdirAll(filepath.Join(base, dir), 0o755); err != nil {
			h.t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(base, "comm"), []byte(comm+"\n"), 0o644); err != nil {
		h.t.Fatal(err)
	}
	_ = os.Symlink("/dev/null", filepath.Join(base, "fd", "0"))
	for i, info := range fdinfos {
		fd := fmt.Sprint(10 + i)
		link := filepath.Join(base, "fd", fd)
		_ = os.Remove(link)
		if err := os.Symlink("/dev/dri/renderD129", link); err != nil {
			h.t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(base, "fdinfo", fd), []byte(info), 0o644); err != nil {
			h.t.Fatal(err)
		}
	}
}

func xeFdinfo(id int, vramKiB, gttKiB int64, ccs, total uint64) string {
	return fmt.Sprintf(`pos:	0
flags:	0100002
drm-driver:	xe
drm-client-id:	%d
drm-pdev:	0000:03:00.0
drm-total-system:	0
drm-resident-system:	0
drm-purgeable-system:	0
drm-total-gtt:	%d KiB
drm-resident-gtt:	%d KiB
drm-total-vram0:	%d KiB
drm-shared-vram0:	0
drm-resident-vram0:	%d KiB
drm-active-vram0:	0
drm-total-stolen:	0
drm-cycles-rcs:	0
drm-total-cycles-rcs:	%d
drm-cycles-bcs:	0
drm-total-cycles-bcs:	%d
drm-cycles-ccs:	%d
drm-total-cycles-ccs:	%d
drm-cycles-vcs:	0
drm-total-cycles-vcs:	%d
drm-cycles-vecs:	0
drm-total-cycles-vecs:	%d
drm-engine-capacity-vcs:	2
drm-engine-capacity-vecs:	2
`, id, gttKiB, gttKiB, vramKiB, vramKiB, total, total, ccs, total, total, total)
}

func i915Fdinfo(id int, localKiB int64, videoNs uint64) string {
	return fmt.Sprintf(`pos:	0
drm-driver:	i915
drm-client-id:	%d
drm-pdev:	0000:14:00.0
drm-total-system0:	83 MiB
drm-resident-system0:	83 MiB
drm-total-local0:	%d KiB
drm-resident-local0:	%d KiB
drm-total-stolen-local0:	0
drm-engine-render:	0 ns
drm-engine-copy:	0 ns
drm-engine-video:	%d ns
drm-engine-capacity-video:	2
drm-engine-video-enhance:	0 ns
`, id, localKiB, localKiB, videoNs)
}

func value(t *testing.T, vec *prometheus.GaugeVec, labels ...string) float64 {
	t.Helper()
	g, err := vec.GetMetricWithLabelValues(labels...)
	if err != nil {
		t.Fatal(err)
	}
	return testutil.ToFloat64(g)
}

func near(t *testing.T, name string, got, want float64) {
	t.Helper()
	if got < want-0.01*want-0.01 || got > want+0.01*want+0.01 {
		t.Errorf("%s = %.3f, want %.3f", name, got, want)
	}
}

func TestDiscoverCards(t *testing.T) {
	h := newFakeHost(t)
	cards := discoverCards(h.sys, nil)
	if len(cards) != 2 || cards[0].PCI != "0000:03:00.0" || cards[0].Driver != "xe" || cards[0].Node != "card1" ||
		cards[1].PCI != "0000:14:00.0" || cards[1].Driver != "i915" {
		t.Fatalf("unexpected cards: %+v", cards)
	}
	if only := discoverCards(h.sys, map[string]bool{"0000:14:00.0": true}); len(only) != 1 || only[0].Driver != "i915" {
		t.Fatalf("CARDS filter: %+v", only)
	}
	if total, ok := cards[0].vramTotalBytes(); !ok || total != 16<<30 {
		t.Fatalf("vram total = %d, %v", total, ok)
	}
}

func TestCollector(t *testing.T) {
	h := newFakeHost(t)
	// Ollama holds two clients on the B50, whisper one; ffmpeg uses the A310
	h.process(100, "llama-server", xeFdinfo(1, 5000*1024, 3000*1024, 0, 1_000_000), xeFdinfo(2, 411*1024, 0, 0, 1_000_000))
	h.process(200, "simple-server", xeFdinfo(3, 1988*1024, 2*1024, 0, 1_000_000))
	h.process(300, "ffmpeg", i915Fdinfo(4, 1011*1024, 0))

	reg := prometheus.NewRegistry()
	m := newMetrics(reg)
	col := newCollector(discoverCards(h.sys, nil), newFdinfoScanner(h.proc, time.Hour), m)
	col.collect()

	// Second sample: Ollama keeps Compute busy 70% of the GPU cycles, ffmpeg uses 1.2 video engines,
	// the B50 drew 38 J/s and its two GTs were idle 25% of the time
	time.Sleep(200 * time.Millisecond)
	elapsed := time.Since(col.prevAt)
	h.process(100, "llama-server", xeFdinfo(1, 5000*1024, 3000*1024, 700_000, 2_000_000), xeFdinfo(2, 411*1024, 0, 0, 2_000_000))
	h.process(200, "simple-server", xeFdinfo(3, 1988*1024, 2*1024, 0, 2_000_000))
	h.process(300, "ffmpeg", i915Fdinfo(4, 1011*1024, uint64(1.2*float64(elapsed.Nanoseconds()))))
	h.energy(1_000_000_000+int64(38e6*elapsed.Seconds()), 500_000_000+int64(22e6*elapsed.Seconds()))
	h.idle(elapsed.Milliseconds()/4, elapsed.Milliseconds()/4)
	h.interrupts(int64(5000*elapsed.Seconds()), int64(800*elapsed.Seconds()))
	col.collect()

	b50, a310 := "0000:03:00.0", "0000:14:00.0"
	near(t, "ollama vram", value(t, m.clientMemoryResident, b50, "100", "llama-server", "vram"), 5411*1024*1024)
	near(t, "ollama system", value(t, m.clientMemoryResident, b50, "100", "llama-server", "system"), 3000*1024*1024)
	near(t, "ffmpeg vram", value(t, m.clientMemoryResident, a310, "300", "ffmpeg", "vram"), 1011*1024*1024)
	near(t, "b50 vram used", value(t, m.memoryUsed, b50, "vram"), (5411+1988)*1024*1024)
	near(t, "b50 vram total", value(t, m.memoryTotal, b50, "vram"), 16<<30)
	near(t, "a310 vram total", value(t, m.memoryTotal, a310, "vram"), 4<<30)
	near(t, "ollama compute", value(t, m.clientEngineBusy, b50, "100", "llama-server", "Compute"), 70)
	near(t, "whisper compute", value(t, m.clientEngineBusy, b50, "200", "simple-server", "Compute"), 0)
	near(t, "b50 compute", value(t, m.enginesBusy, b50, "Compute"), 70)
	near(t, "ffmpeg video", value(t, m.clientEngineBusy, a310, "300", "ffmpeg", "Video"), 120)
	near(t, "b50 power", value(t, m.powerGPU, b50), 38)
	near(t, "b50 package", value(t, m.powerPackage, b50), 38)
	near(t, "a310 power", value(t, m.powerGPU, a310), 22)
	near(t, "b50 rc6", value(t, m.rc6, b50), 25)
	near(t, "b50 freq", value(t, m.frequencyActual, b50), 2267)
	near(t, "a310 video", value(t, m.enginesBusy, a310, "Video"), 60)
	near(t, "a310 freq", value(t, m.frequencyActual, a310), 1350)
	near(t, "a310 requested", value(t, m.frequencyRequested, a310), 2450)
	near(t, "a310 rc6", value(t, m.rc6, a310), 50)
	near(t, "b50 interrupts", value(t, m.interrupts, b50), 5000)
	near(t, "a310 interrupts", value(t, m.interrupts, a310), 800)

	// A process that exits disappears from the client series
	if err := os.RemoveAll(filepath.Join(h.proc, "200")); err != nil {
		t.Fatal(err)
	}
	col.collect()
	if n := testutil.CollectAndCount(m.clientMemoryResident, "igpu_client_memory_resident_bytes"); n != 4 {
		t.Errorf("resident series after whisper exited = %d, want 4 (vram and system of ollama and ffmpeg)", n)
	}
}

func TestScannerFindsNewProcessesBetweenFullScans(t *testing.T) {
	h := newFakeHost(t)
	h.process(100, "llama-server", xeFdinfo(1, 5000*1024, 0, 0, 1_000_000))
	h.process(400, "bash")
	s := newFdinfoScanner(h.proc, time.Hour)
	if n := len(s.scan()); n != 1 {
		t.Fatalf("first (full) scan found %d clients, want 1", n)
	}

	// A transcode starts after the full scan: it must show up on the next sample, not an hour later
	h.process(500, "ffmpeg", i915Fdinfo(7, 1011*1024, 0))
	clients := s.scan()
	if _, ok := clients[clientKey{"0000:14:00.0", "7"}]; !ok || len(clients) != 2 {
		t.Fatalf("new ffmpeg not found between full scans: %d clients", len(clients))
	}

	// A process known without a GPU node is only re-read by the full scan
	h.process(400, "bash", xeFdinfo(9, 1024, 0, 0, 1_000_000))
	if _, ok := s.scan()[clientKey{"0000:03:00.0", "9"}]; ok {
		t.Fatal("known non-GPU process was re-read before the full scan")
	}
}

func TestBusyPercent(t *testing.T) {
	if p, ok := busyPercent(engineCounter{10, 100}, engineCounter{60, 200}, time.Second); !ok || p != 50 {
		t.Errorf("xe = %v %v", p, ok)
	}
	if p, ok := busyPercent(engineCounter{0, 0}, engineCounter{500_000_000, 0}, time.Second); !ok || p != 50 {
		t.Errorf("i915 = %v %v", p, ok)
	}
	if _, ok := busyPercent(engineCounter{10, 100}, engineCounter{5, 200}, time.Second); ok {
		t.Error("counter going backwards must be ignored")
	}
}
