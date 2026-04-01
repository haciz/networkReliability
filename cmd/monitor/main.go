package main

import (
	"flag"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/prometheus/client_golang/prometheus/promhttp"
	"monitoring-app/internal/dns"
	"monitoring-app/internal/http_monitor"
	"monitoring-app/internal/icmp"
	"monitoring-app/internal/tcp"
)

func main() {
	promPort        := flag.String("prom-port", "2112", "Prometheus metrics port")
	probeName       := flag.String("probe-name", "default", "Name of this probe appliance (appears on every metric label)")
	dnsTargetsFile  := flag.String("dns-targets", "config/dns_targets.yaml", "File with DNS targets to monitor")
	httpTargetsFile := flag.String("http-targets", "config/http_targets.yaml", "File with HTTP targets to monitor")
	tcpTargetsFile  := flag.String("tcp-targets", "config/tcp_targets.yaml", "File with TCP targets to monitor")
	icmpTargetsFile := flag.String("icmp-targets", "config/icmp_targets.yaml", "File with ICMP ping targets to monitor")
	interval        := flag.Duration("interval", 10*time.Second, "Monitoring interval")
	logFormat       := flag.String("log-format", "json", "Log format: json or text")
	flag.Parse()

	// Configure structured logging. JSON for production; text for local dev.
	if *logFormat == "text" {
		slog.SetDefault(slog.New(slog.NewTextHandler(os.Stderr, nil)))
	} else {
		slog.SetDefault(slog.New(slog.NewJSONHandler(os.Stderr, nil)))
	}

	dnsMonitor, err := dns.NewMonitor(*dnsTargetsFile, *probeName, *interval)
	if err != nil {
		slog.Error("failed to create DNS monitor", "error", err)
		os.Exit(1)
	}
	go dnsMonitor.Start()

	httpMonitor, err := http_monitor.NewMonitor(*httpTargetsFile, *probeName, *interval)
	if err != nil {
		slog.Error("failed to create HTTP monitor", "error", err)
		os.Exit(1)
	}
	go httpMonitor.Start()

	tcpMonitor, err := tcp.NewMonitor(*tcpTargetsFile, *probeName, *interval)
	if err != nil {
		slog.Error("failed to create TCP monitor", "error", err)
		os.Exit(1)
	}
	go tcpMonitor.Start()

	icmpMonitor, err := icmp.NewMonitor(*icmpTargetsFile, *probeName, *interval)
	if err != nil {
		slog.Error("failed to create ICMP monitor", "error", err)
		os.Exit(1)
	}
	go icmpMonitor.Start()

	// Prometheus metrics endpoint
	http.Handle("/metrics", promhttp.Handler())
	go func() {
		slog.Info("starting Prometheus metrics server", "port", *promPort)
		if err := http.ListenAndServe(":"+*promPort, nil); err != nil {
			slog.Error("metrics server failed", "error", err)
			os.Exit(1)
		}
	}()

	// SIGHUP reloads all target files atomically without restarting the process.
	// Send with: kill -HUP <pid>  or  docker kill --signal=HUP <container>
	sighup := make(chan os.Signal, 1)
	signal.Notify(sighup, syscall.SIGHUP)
	go func() {
		for range sighup {
			slog.Info("SIGHUP received, reloading target files")
			dnsMonitor.Reload()
			httpMonitor.Reload()
			tcpMonitor.Reload()
			icmpMonitor.Reload()
		}
	}()

	sigChan := make(chan os.Signal, 1)
	signal.Notify(sigChan, syscall.SIGINT, syscall.SIGTERM)
	<-sigChan

	slog.Info("shutdown signal received, draining in-flight probes")
	stopDone := make(chan struct{})
	go func() {
		defer close(stopDone)
		dnsMonitor.Stop()
		httpMonitor.Stop()
		tcpMonitor.Stop()
		icmpMonitor.Stop()
	}()

	select {
	case <-stopDone:
		slog.Info("all monitors stopped cleanly")
	case <-time.After(10 * time.Second):
		slog.Warn("shutdown timed out after 10s, forcing exit")
	}
}
