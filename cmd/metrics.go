package main

import "github.com/prometheus/client_golang/prometheus"

func gaugeVec(name, help string, labels ...string) *prometheus.GaugeVec {
	return prometheus.NewGaugeVec(prometheus.GaugeOpts{Name: name, Help: help}, labels)
}

func newMetrics(reg prometheus.Registerer) *Metrics {
	client := []string{"card", "pid", "name", "region"}
	m := &Metrics{
		info:                  gaugeVec("igpu_info", "Intel GPU found, always 1", "card", "driver", "node"),
		enginesBusy:           gaugeVec("igpu_engines_busy_percent", "Engine busy utilisation %", "card", "engine"),
		frequencyActual:       gaugeVec("igpu_frequency_actual", "Frequency actual MHz", "card"),
		frequencyRequested:    gaugeVec("igpu_frequency_requested", "Frequency requested MHz", "card"),
		interrupts:            gaugeVec("igpu_interrupts", "Interrupts/s", "card"),
		period:                gaugeVec("igpu_period", "Period ms", "card"),
		powerGPU:              gaugeVec("igpu_power_gpu", "GPU power W", "card"),
		powerPackage:          gaugeVec("igpu_power_package", "Package power W", "card"),
		rc6:                   gaugeVec("igpu_rc6", "RC6 (idle) residency %", "card"),
		memoryTotal:           gaugeVec("igpu_memory_total_bytes", "Memory size in bytes", "card", "region"),
		memoryUsed:            gaugeVec("igpu_memory_used_bytes", "Memory resident in bytes, summed over all clients", "card", "region"),
		clientMemoryTotal:     gaugeVec("igpu_client_memory_total_bytes", "Client memory total bytes", client...),
		clientMemoryShared:    gaugeVec("igpu_client_memory_shared_bytes", "Client memory shared bytes", client...),
		clientMemoryResident:  gaugeVec("igpu_client_memory_resident_bytes", "Client memory resident bytes", client...),
		clientMemoryPurgeable: gaugeVec("igpu_client_memory_purgeable_bytes", "Client memory purgeable bytes", client...),
		clientMemoryActive:    gaugeVec("igpu_client_memory_active_bytes", "Client memory active bytes", client...),
		clientEngineBusy:      gaugeVec("igpu_client_engine_busy_percent", "Client engine busy utilisation %", "card", "pid", "name", "engine"),
	}

	reg.MustRegister(
		m.info,
		m.enginesBusy,
		m.frequencyActual,
		m.frequencyRequested,
		m.interrupts,
		m.period,
		m.powerGPU,
		m.powerPackage,
		m.rc6,
		m.memoryTotal,
		m.memoryUsed,
		m.clientMemoryTotal,
		m.clientMemoryShared,
		m.clientMemoryResident,
		m.clientMemoryPurgeable,
		m.clientMemoryActive,
		m.clientEngineBusy,
	)
	return m
}

// seriesSet sets a batch of series on a vector and deletes the ones from the previous batch
// that are gone (for example a process that exited), without a window where all are missing.
type seriesSet struct {
	vec  *prometheus.GaugeVec
	prev map[string]prometheus.Labels
	cur  map[string]prometheus.Labels
}

func newSeriesSet(vec *prometheus.GaugeVec) *seriesSet {
	return &seriesSet{vec: vec, prev: map[string]prometheus.Labels{}}
}

func (s *seriesSet) begin() { s.cur = map[string]prometheus.Labels{} }

func (s *seriesSet) set(labels prometheus.Labels, value float64) {
	s.vec.With(labels).Set(value)
	s.cur[labelKey(labels)] = labels
}

func (s *seriesSet) end() {
	for key, labels := range s.prev {
		if _, ok := s.cur[key]; !ok {
			s.vec.Delete(labels)
		}
	}
	s.prev = s.cur
}

func labelKey(labels prometheus.Labels) string {
	key := ""
	for _, name := range []string{"card", "pid", "name", "region", "engine"} {
		key += labels[name] + "\x00"
	}
	return key
}
