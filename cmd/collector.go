package main

import (
	"time"

	"github.com/prometheus/client_golang/prometheus"
)

// collector reads every metric from files the kernel already keeps, the same way for i915 and xe
// cards: engine and memory use per client from fdinfo, frequency and RC6 from sysfs, power from
// hwmon and interrupts from /proc/interrupts. It needs no PMU, which xe lacks before kernel 6.15.
type collector struct {
	cards   []Card
	scanner *fdinfoScanner
	m       *Metrics

	prevClients    map[clientKey]*drmClient
	prevEnergy     map[string][]energySensor
	prevIdle       map[string]int64
	prevInterrupts map[string]int64
	prevAt         time.Time

	clientMemory map[string]*seriesSet // kind -> series
	clientBusy   *seriesSet
	memoryUsed   *seriesSet
}

func newCollector(cards []Card, scanner *fdinfoScanner, m *Metrics) *collector {
	c := &collector{
		cards:          cards,
		scanner:        scanner,
		m:              m,
		prevEnergy:     map[string][]energySensor{},
		prevIdle:       map[string]int64{},
		prevInterrupts: map[string]int64{},
		clientMemory: map[string]*seriesSet{
			"total":     newSeriesSet(m.clientMemoryTotal),
			"shared":    newSeriesSet(m.clientMemoryShared),
			"resident":  newSeriesSet(m.clientMemoryResident),
			"purgeable": newSeriesSet(m.clientMemoryPurgeable),
			"active":    newSeriesSet(m.clientMemoryActive),
		},
		clientBusy: newSeriesSet(m.clientEngineBusy),
		memoryUsed: newSeriesSet(m.memoryUsed),
	}
	for _, card := range cards {
		m.info.With(prometheus.Labels{"card": card.PCI, "driver": card.Driver, "node": card.Node}).Set(1)
	}
	return c
}

type appKey struct{ card, pid, name string }

func (c *collector) collect() {
	now := time.Now()
	elapsed := now.Sub(c.prevAt)
	first := c.prevAt.IsZero()
	clients := c.scanner.scan()
	drivers := map[string]string{}
	for _, card := range c.cards {
		drivers[card.PCI] = card.Driver
	}

	// Per-process sums: a process can hold several clients (Vulkan, OpenVINO and ffmpeg open more than one)
	memory := map[string]map[appKey]map[string]int64{}
	busy := map[appKey]map[string]float64{}
	engineTotals := map[string]map[string]float64{}
	used := map[string]map[string]int64{}
	for key, cl := range clients {
		if _, known := drivers[cl.PCI]; !known {
			continue
		}
		app := appKey{cl.PCI, cl.PID, cl.Name}
		for kind, regions := range cl.Memory {
			if memory[kind] == nil {
				memory[kind] = map[appKey]map[string]int64{}
			}
			if memory[kind][app] == nil {
				memory[kind][app] = map[string]int64{}
			}
			for region, bytes := range regions {
				if region == "stolen" && bytes == 0 { // stolen memory is only used by integrated GPUs
					continue
				}
				memory[kind][app][region] += bytes
				if kind == "resident" {
					if used[cl.PCI] == nil {
						used[cl.PCI] = map[string]int64{}
					}
					used[cl.PCI][region] += bytes
				}
			}
		}

		prev, ok := c.prevClients[key]
		if busy[app] == nil {
			busy[app] = map[string]float64{}
		}
		for engine, counter := range cl.Busy {
			busy[app][engine] += 0 // keep idle engines as 0 instead of missing
			if !ok {
				continue
			}
			pct, valid := busyPercent(prev.Busy[engine], counter, cl.At.Sub(prev.At))
			if !valid {
				continue
			}
			busy[app][engine] += pct
			if engineTotals[cl.PCI] == nil {
				engineTotals[cl.PCI] = map[string]float64{}
			}
			engineTotals[cl.PCI][engine] += pct / float64(cl.Capacity[engine])
		}
	}

	for kind, set := range c.clientMemory {
		set.begin()
		for app, regions := range memory[kind] {
			for region, bytes := range regions {
				set.set(prometheus.Labels{"card": app.card, "pid": app.pid, "name": app.name, "region": region}, float64(bytes))
			}
		}
		set.end()
	}
	c.clientBusy.begin()
	for app, engines := range busy {
		for engine, pct := range engines {
			c.clientBusy.set(prometheus.Labels{"card": app.card, "pid": app.pid, "name": app.name, "engine": engine}, pct)
		}
	}
	c.clientBusy.end()
	c.memoryUsed.begin()
	for _, card := range c.cards {
		for _, region := range []string{"vram", "system"} {
			c.memoryUsed.set(prometheus.Labels{"card": card.PCI, "region": region}, float64(used[card.PCI][region]))
		}
	}
	c.memoryUsed.end()

	for _, card := range c.cards {
		labels := prometheus.Labels{"card": card.PCI}
		if total, ok := card.vramTotalBytes(); ok {
			c.m.memoryTotal.With(prometheus.Labels{"card": card.PCI, "region": "vram"}).Set(float64(total))
		}
		c.collectPower(card, labels, elapsed, first)
		c.collectInterrupts(card, labels, elapsed, first)

		for _, engine := range xeEngines {
			pct := engineTotals[card.PCI][engine]
			if pct > 100 {
				pct = 100
			}
			if !first {
				c.m.enginesBusy.With(prometheus.Labels{"card": card.PCI, "engine": engine}).Set(pct)
			}
		}
		if actual, requested, ok := card.frequency(); ok {
			c.m.frequencyActual.With(labels).Set(actual)
			c.m.frequencyRequested.With(labels).Set(requested)
		}
		if idle, gts := card.idleResidencyMs(); gts > 0 {
			if prev, ok := c.prevIdle[card.PCI]; ok && !first && elapsed > 0 && idle >= prev {
				c.m.rc6.With(labels).Set(100 * float64(idle-prev) / float64(gts) / float64(elapsed.Milliseconds()))
			}
			c.prevIdle[card.PCI] = idle
		}
		if !first {
			c.m.period.With(labels).Set(float64(elapsed.Milliseconds()))
		}
	}

	c.prevClients = clients
	c.prevAt = now
}

// collectInterrupts turns the card's interrupt count into a rate per second.
func (c *collector) collectInterrupts(card Card, labels prometheus.Labels, elapsed time.Duration, first bool) {
	unique := true
	for _, other := range c.cards {
		if other.PCI != card.PCI && other.Driver == card.Driver {
			unique = false
		}
	}
	count, ok := interruptCount(c.scanner.procRoot, card, unique)
	if !ok {
		return
	}
	if prev, seen := c.prevInterrupts[card.PCI]; seen && !first && elapsed > 0 && count >= prev {
		c.m.interrupts.With(labels).Set(float64(count-prev) / elapsed.Seconds())
	}
	c.prevInterrupts[card.PCI] = count
}

// collectPower turns hwmon energy counters into watts. The GPU value comes from the sensor
// labelled "card" when it counts, else from the first one that counts (the B50 reads 0 on
// energy1 and counts on energy2); "pkg" also feeds the package value.
func (c *collector) collectPower(card Card, labels prometheus.Labels, elapsed time.Duration, first bool) {
	sensors := card.energy()
	prev := c.prevEnergy[card.PCI]
	c.prevEnergy[card.PCI] = sensors
	if first || len(sensors) == 0 || len(prev) != len(sensors) || elapsed <= 0 {
		return
	}
	watts := map[string]float64{}
	gpu, gpuFound := 0.0, false
	for i, s := range sensors {
		if s.Name != prev[i].Name || s.Value < prev[i].Value {
			continue
		}
		w := float64(s.Value-prev[i].Value) / 1e6 / elapsed.Seconds()
		watts[s.Label] = w
		if s.Label == "card" && w > 0 {
			gpu, gpuFound = w, true
		}
	}
	if !gpuFound {
		for i, s := range sensors {
			if s.Name == prev[i].Name && s.Value > prev[i].Value {
				gpu, gpuFound = float64(s.Value-prev[i].Value)/1e6/elapsed.Seconds(), true
				break
			}
		}
	}
	c.m.powerGPU.With(labels).Set(gpu)
	if w, ok := watts["pkg"]; ok {
		c.m.powerPackage.With(labels).Set(w)
	} else {
		c.m.powerPackage.With(labels).Set(gpu)
	}
}
