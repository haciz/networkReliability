package main

import (
	"flag"
	"log"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/prometheus/client_golang/prometheus/promhttp"
	"monitoring-app/internal/dns"
	"monitoring-app/internal/http_monitor"
	"monitoring-app/internal/tcp"
)

func main() {
	promPort       := flag.String("prom-port", "2112", "Prometheus metrics port")
	probeName      := flag.String("probe-name", "default", "Name of this probe appliance (appears on every metric label)")
	dnsTargetsFile := flag.String("dns-targets", "config/dns_targets.txt", "File with DNS targets to monitor")
	httpTargetsFile := flag.String("http-targets", "config/http_targets.txt", "File with HTTP targets to monitor")
	tcpTargetsFile := flag.String("tcp-targets", "config/tcp_targets.txt", "File with TCP targets to monitor")
	interval       := flag.Duration("interval", 10*time.Second, "Monitoring interval")
	flag.Parse()

	dnsMonitor, err := dns.NewMonitor(*dnsTargetsFile, *probeName, *interval)
	if err != nil {
		log.Fatalf("Failed to create DNS monitor: %v", err)
	}
	go dnsMonitor.Start()

	httpMonitor, err := http_monitor.NewMonitor(*httpTargetsFile, *probeName, *interval)
	if err != nil {
		log.Fatalf("Failed to create HTTP monitor: %v", err)
	}
	go httpMonitor.Start()

	tcpMonitor, err := tcp.NewMonitor(*tcpTargetsFile, *probeName, *interval)
	if err != nil {
		log.Fatalf("Failed to create TCP monitor: %v", err)
	}
	go tcpMonitor.Start()

	// Prometheus metrics endpoint
	http.Handle("/metrics", promhttp.Handler())
	go func() {
		log.Printf("Starting Prometheus metrics server on :%s", *promPort)
		if err := http.ListenAndServe(":"+*promPort, nil); err != nil {
			log.Fatalf("Failed to start metrics server: %v", err)
		}
	}()

	// SIGHUP reloads all target files atomically without restarting the process.
	// Send with: kill -HUP <pid>  or  docker kill --signal=HUP <container>
	sighup := make(chan os.Signal, 1)
	signal.Notify(sighup, syscall.SIGHUP)
	go func() {
		for range sighup {
			log.Println("SIGHUP received — reloading target files")
			dnsMonitor.Reload()
			httpMonitor.Reload()
			tcpMonitor.Reload()
		}
	}()

	sigChan := make(chan os.Signal, 1)
	signal.Notify(sigChan, syscall.SIGINT, syscall.SIGTERM)
	<-sigChan

	dnsMonitor.Stop()
	httpMonitor.Stop()
	tcpMonitor.Stop()
	log.Println("Monitoring stopped")
}
