package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"github.com/agim/lidza-deploy/internal/agent"
	"github.com/agim/lidza-deploy/internal/buildinfo"
	"log"
	"net/http"
	"os/signal"
	"path/filepath"
	"syscall"
	"time"
)

func main() {
	path := flag.String("config", "/etc/lidza-agent/config.json", "configuration file")
	version := flag.Bool("version", false, "print release version")
	applySSH := flag.Bool("apply-ssh-keys", false, "apply queued managed SSH access (root helper only)")
	scannerFirewall := flag.String("scanner-firewall", "", "prepare, enable or disable Caddy scanner rejection and 90-day Fail2ban bans")
	scannerOutput := flag.String("scanner-firewall-output", "", "directory for reviewable scanner firewall configuration (prepare mode)")
	scannerAllow := flag.String("scanner-firewall-allow", "", "comma-separated operator/monitoring IPs or CIDRs exempt from scanner bans")
	directTraffic := flag.Bool("direct-public-traffic", false, "confirm public traffic reaches Caddy directly without a CDN/load balancer")
	flag.Parse()
	if *version {
		fmt.Println(buildinfo.Version)
		return
	}
	if *scannerFirewall != "" {
		if err := agent.ConfigureScannerFirewall(context.Background(), *scannerFirewall, *scannerOutput, *scannerAllow, *directTraffic); err != nil {
			log.Fatal(err)
		}
		return
	}
	if *applySSH {
		if err := agent.ApplySSHAccess(); err != nil {
			log.Fatal(err)
		}
		return
	}
	cfg, err := agent.LoadConfig(*path)
	if err != nil {
		log.Fatal(err)
	}
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	m, err := agent.NewManager(ctx, cfg, &agent.Docker{Root: filepath.Join(cfg.DataDir, "builds")})
	if err != nil {
		log.Fatal(err)
	}
	defer m.Close()
	m.StartMaintenance()
	api := &http.Server{Addr: cfg.Listen, Handler: agent.Handler(m), ReadHeaderTimeout: 5 * time.Second, ReadTimeout: 15 * time.Second, WriteTimeout: 35 * time.Second, IdleTimeout: 60 * time.Second}
	proxy := &http.Server{Addr: cfg.ProxyListen, Handler: agent.Proxy(m), ReadHeaderTimeout: 5 * time.Second, IdleTimeout: 60 * time.Second}
	errs := make(chan error, 2)
	go func() { errs <- api.ListenAndServe() }()
	go func() { errs <- proxy.ListenAndServe() }()
	log.Printf("agent API %s; application ingress %s", cfg.Listen, cfg.ProxyListen)
	select {
	case <-ctx.Done():
	case err := <-errs:
		if !errors.Is(err, http.ErrServerClosed) {
			log.Print(err)
		}
	}
	stop()
	shutdown, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	_ = api.Shutdown(shutdown)
	_ = proxy.Shutdown(shutdown)
}
