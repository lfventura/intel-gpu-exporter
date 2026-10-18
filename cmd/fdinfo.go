package main

import (
	"bufio"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

// Engine names follow what intel_gpu_top reports, so i915 and xe cards share the same label values.
var xeEngines = map[string]string{
	"rcs":  "Render/3D",
	"bcs":  "Blitter",
	"ccs":  "Compute",
	"vcs":  "Video",
	"vecs": "VideoEnhance",
}

var i915Engines = map[string]string{
	"render":        "Render/3D",
	"copy":          "Blitter",
	"compute":       "Compute",
	"video":         "Video",
	"video-enhance": "VideoEnhance",
}

// drmClient is one DRM client (one open of a GPU node), read from /proc/<pid>/fdinfo/<fd>.
type drmClient struct {
	PCI      string
	ID       string
	PID      string
	Name     string
	Driver   string
	Memory   map[string]map[string]int64 // kind (total, resident, ...) -> region (vram, system, stolen) -> bytes
	Busy     map[string]engineCounter    // engine label -> counter
	Capacity map[string]int64            // engine label -> number of engines of that class
	At       time.Time
}

// engineCounter is xe's busy cycles over total GPU cycles, or i915's busy nanoseconds (Total = 0).
type engineCounter struct {
	Busy  uint64
	Total uint64
}

type clientKey struct{ PCI, ID string }

// regionOf maps the driver's memory region names to vram, system or stolen.
func regionOf(name string) string {
	switch {
	case strings.HasPrefix(name, "stolen"):
		return "stolen"
	case strings.HasPrefix(name, "vram"), strings.HasPrefix(name, "local"):
		return "vram"
	default: // system, system0, gtt
		return "system"
	}
}

// parseSize reads values such as "5541888 KiB" or "1234" into bytes.
func parseSize(v string) int64 {
	fields := strings.Fields(v)
	if len(fields) == 0 {
		return 0
	}
	n, err := strconv.ParseInt(fields[0], 10, 64)
	if err != nil {
		return 0
	}
	if len(fields) > 1 {
		switch fields[1] {
		case "KiB":
			n *= 1 << 10
		case "MiB":
			n *= 1 << 20
		case "GiB":
			n *= 1 << 30
		}
	}
	return n
}

func parseCounter(v string) uint64 {
	fields := strings.Fields(v)
	if len(fields) == 0 {
		return 0
	}
	n, _ := strconv.ParseUint(fields[0], 10, 64)
	return n
}

func parseFdinfo(path string) (*drmClient, bool) {
	f, err := os.Open(path)
	if err != nil {
		return nil, false
	}
	defer f.Close()

	c := &drmClient{
		Memory:   map[string]map[string]int64{},
		Busy:     map[string]engineCounter{},
		Capacity: map[string]int64{},
	}
	totals := map[string]uint64{}
	scanner := bufio.NewScanner(f)
	for scanner.Scan() {
		key, value, found := strings.Cut(scanner.Text(), ":")
		if !found || !strings.HasPrefix(key, "drm-") {
			continue
		}
		value = strings.TrimSpace(value)
		switch {
		case key == "drm-pdev":
			c.PCI = value
		case key == "drm-client-id":
			c.ID = value
		case key == "drm-driver":
			c.Driver = value
		case strings.HasPrefix(key, "drm-engine-capacity-"):
			c.Capacity[strings.TrimPrefix(key, "drm-engine-capacity-")], _ = strconv.ParseInt(value, 10, 64)
		case strings.HasPrefix(key, "drm-total-cycles-"):
			totals[strings.TrimPrefix(key, "drm-total-cycles-")] = parseCounter(value)
		case strings.HasPrefix(key, "drm-cycles-"):
			name := strings.TrimPrefix(key, "drm-cycles-")
			c.Busy[name] = engineCounter{Busy: parseCounter(value)}
		case strings.HasPrefix(key, "drm-engine-"):
			c.Busy[strings.TrimPrefix(key, "drm-engine-")] = engineCounter{Busy: parseCounter(value)}
		default:
			for _, kind := range []string{"total", "shared", "resident", "purgeable", "active"} {
				prefix := "drm-" + kind + "-"
				if strings.HasPrefix(key, prefix) {
					region := regionOf(strings.TrimPrefix(key, prefix))
					if c.Memory[kind] == nil {
						c.Memory[kind] = map[string]int64{}
					}
					c.Memory[kind][region] += parseSize(value)
				}
			}
		}
	}
	if c.PCI == "" || c.ID == "" {
		return nil, false
	}

	// Rename engines to the shared labels; xe counters also need their total cycles.
	names := i915Engines
	if c.Driver == "xe" {
		names = xeEngines
	}
	busy := map[string]engineCounter{}
	capacity := map[string]int64{}
	for raw, counter := range c.Busy {
		label, ok := names[raw]
		if !ok {
			continue
		}
		if c.Driver == "xe" {
			counter.Total = totals[raw]
		}
		busy[label] = counter
		capacity[label] = 1
		if n := c.Capacity[raw]; n > 0 {
			capacity[label] = n
		}
	}
	c.Busy, c.Capacity = busy, capacity
	c.At = time.Now()
	return c, true
}

// fdinfoScanner finds DRM clients in /proc. A full scan of every process runs every fullScanEvery;
// in between only the processes that already had a GPU node open are read again.
type fdinfoScanner struct {
	procRoot      string
	fullScanEvery time.Duration
	lastFullScan  time.Time
	gpuPIDs       map[string]bool
	selfPID       string
}

func newFdinfoScanner(procRoot string, fullScanEvery time.Duration) *fdinfoScanner {
	return &fdinfoScanner{
		procRoot:      procRoot,
		fullScanEvery: fullScanEvery,
		gpuPIDs:       map[string]bool{},
		selfPID:       strconv.Itoa(os.Getpid()),
	}
}

func (s *fdinfoScanner) scan() map[clientKey]*drmClient {
	var pids []string
	if time.Since(s.lastFullScan) >= s.fullScanEvery {
		entries, _ := os.ReadDir(s.procRoot)
		for _, e := range entries {
			if name := e.Name(); name[0] >= '0' && name[0] <= '9' {
				pids = append(pids, name)
			}
		}
		s.lastFullScan = time.Now()
		s.gpuPIDs = map[string]bool{}
	} else {
		for pid := range s.gpuPIDs {
			pids = append(pids, pid)
		}
	}

	clients := map[clientKey]*drmClient{}
	for _, pid := range pids {
		if pid == s.selfPID {
			continue
		}
		fdDir := filepath.Join(s.procRoot, pid, "fd")
		fds, err := os.ReadDir(fdDir)
		if err != nil {
			delete(s.gpuPIDs, pid)
			continue
		}
		found := false
		for _, fd := range fds {
			target, err := os.Readlink(filepath.Join(fdDir, fd.Name()))
			if err != nil || !strings.HasPrefix(target, "/dev/dri/") {
				continue
			}
			found = true
			c, ok := parseFdinfo(filepath.Join(s.procRoot, pid, "fdinfo", fd.Name()))
			if !ok {
				continue
			}
			key := clientKey{c.PCI, c.ID}
			if _, dup := clients[key]; dup { // dup'ed fds share one client
				continue
			}
			c.PID = pid
			c.Name = readString(filepath.Join(s.procRoot, pid, "comm"))
			clients[key] = c
		}
		if found {
			s.gpuPIDs[pid] = true
		} else {
			delete(s.gpuPIDs, pid)
		}
	}
	return clients
}

// busyPercent is the share of time a client kept an engine busy between two samples.
func busyPercent(prev, cur engineCounter, elapsed time.Duration) (float64, bool) {
	if cur.Busy < prev.Busy {
		return 0, false
	}
	if cur.Total > 0 || prev.Total > 0 { // xe: cycles over GPU cycles
		if cur.Total <= prev.Total {
			return 0, false
		}
		return 100 * float64(cur.Busy-prev.Busy) / float64(cur.Total-prev.Total), true
	}
	if elapsed <= 0 { // i915: busy nanoseconds over wall time
		return 0, false
	}
	return 100 * float64(cur.Busy-prev.Busy) / float64(elapsed.Nanoseconds()), true
}
