package main

import (
	"log"
	"net/http"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promhttp"
)

func getEnv(key, defaultValue string) string {
	if value := os.Getenv(key); value != "" {
		return value
	}
	return defaultValue
}

func getEnvBool(key string) bool {
	value := os.Getenv(key)
	return value == "true" || value == "1"
}

func getEnvInt(key string, defaultValue int) int {
	value, err := strconv.Atoi(getEnv(key, strconv.Itoa(defaultValue)))
	if err != nil {
		log.Fatalf("Invalid %s: %v", key, err)
	}
	return value
}

func main() {
	debug := getEnvBool("DEBUG")

	// Setup signal handling
	sigChan := make(chan os.Signal, 1)
	signal.Notify(sigChan, os.Interrupt, syscall.SIGTERM)

	metrics := newMetrics(prometheus.DefaultRegisterer)

	port := getEnv("PORT", "8080")
	period := getEnvInt("REFRESH_PERIOD_MS", 10000)
	fullScan := getEnvInt("FULL_SCAN_INTERVAL_S", 60)

	if os.Getenv("DEVICE") != "" {
		log.Printf("DEVICE is no longer used: every Intel GPU is monitored; use CARDS to pick PCI addresses")
	}
	only := map[string]bool{}
	for _, pci := range strings.Split(os.Getenv("CARDS"), ",") {
		if pci = strings.TrimSpace(pci); pci != "" {
			only[pci] = true
		}
	}

	cards := discoverCards(getEnv("SYSFS_ROOT", "/sys"), only)
	if len(cards) == 0 {
		log.Fatal("No Intel GPU (i915 or xe) found in sysfs")
	}
	for _, card := range cards {
		log.Printf("Found %s (%s, %s)", card.PCI, card.Driver, card.Node)
	}

	http.Handle("/metrics", promhttp.Handler())
	go func() {
		addr := ":" + port
		log.Printf("Starting HTTP server on %s", addr)
		if err := http.ListenAndServe(addr, nil); err != nil {
			log.Fatal(err)
		}
	}()

	scanner := newFdinfoScanner(getEnv("PROC_ROOT", "/proc"), time.Duration(fullScan)*time.Second)
	col := newCollector(cards, scanner, metrics)

	ticker := time.NewTicker(time.Duration(period) * time.Millisecond)
	defer ticker.Stop()
	col.collect()
	log.Printf("Collecting every %d ms, full /proc scan every %d s", period, fullScan)
	for {
		select {
		case <-ticker.C:
			start := time.Now()
			col.collect()
			if debug {
				log.Printf("Collected %d DRM clients in %v", len(col.prevClients), time.Since(start))
			}
		case sig := <-sigChan:
			log.Printf("Received signal: %v", sig)
			log.Println("Finished")
			return
		}
	}
}
