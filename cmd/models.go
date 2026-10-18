package main

import "github.com/prometheus/client_golang/prometheus"

type Metrics struct {
	info                  *prometheus.GaugeVec
	enginesBusy           *prometheus.GaugeVec
	frequencyActual       *prometheus.GaugeVec
	frequencyRequested    *prometheus.GaugeVec
	interrupts            *prometheus.GaugeVec
	period                *prometheus.GaugeVec
	powerGPU              *prometheus.GaugeVec
	powerPackage          *prometheus.GaugeVec
	rc6                   *prometheus.GaugeVec
	memoryTotal           *prometheus.GaugeVec
	memoryUsed            *prometheus.GaugeVec
	clientMemoryTotal     *prometheus.GaugeVec
	clientMemoryShared    *prometheus.GaugeVec
	clientMemoryResident  *prometheus.GaugeVec
	clientMemoryPurgeable *prometheus.GaugeVec
	clientMemoryActive    *prometheus.GaugeVec
	clientEngineBusy      *prometheus.GaugeVec
}
