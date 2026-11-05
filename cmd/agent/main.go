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
	"os"
	"os/signal"
	"path/filepath"
	"syscall"
	"time"
)

func main() {
	path := flag.String("config", "/etc/lidza-agent/config.json", "configuration file")
	checkConfig := flag.Bool("check-config", false, "validate the configuration and exit without starting the agent")
	version := flag.Bool("version", false, "print release version")
	verifyBundle := flag.String("verify-bundle", "", "verify a staged signed update bundle directory with the pinned keys and print its version; no manager")
	nativeDump := flag.Bool("native-pg-dump", false, "isolated native PostgreSQL dump helper; bounded stdin, no manager")
	nativeSQLite := flag.Bool("native-sqlite-snapshot", false, "isolated native SQLite snapshot helper; bounded stdin, no manager")
	applySSH := flag.Bool("apply-ssh-keys", false, "apply queued managed SSH access (root helper only)")
	scannerFirewall := flag.String("scanner-firewall", "", "prepare, enable or disable Caddy scanner rejection and 90-day Fail2ban bans")
	scannerOutput := flag.String("scanner-firewall-output", "", "directory for reviewable scanner firewall configuration (prepare mode)")
	scannerAllow := flag.String("scanner-firewall-allow", "", "comma-separated operator/monitoring IPs or CIDRs exempt from scanner bans")
	directTraffic := flag.Bool("direct-public-traffic", false, "confirm public traffic reaches Caddy directly without a CDN/load balancer")
	gatewayMode := flag.String("gateway-transport", "", "prepare reviewable protected gateway host configuration")
	gatewayOutput := flag.String("gateway-output", "", "new absolute directory for private gateway credentials and reviewable configuration")
	flag.Parse()
	nativeModes := 0
	for _, enabled := range []bool{*nativeDump, *nativeSQLite} {
		if enabled {
			nativeModes++
		}
	}
	if nativeModes > 1 {
		log.Fatal("native helper modes are mutually exclusive")
	}
	if *nativeDump {
		ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
		defer stop()
		if err := agent.RunNativePGDump(ctx, os.Stdin, os.Stdout); err != nil {
			log.Fatal(err)
		}
		return
	}
	if *nativeSQLite {
		ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
		defer stop()
		if err := agent.RunNativeSQLiteSnapshot(ctx, os.Stdin, os.Stdout); err != nil {
			log.Fatal(err)
		}
		return
	}
	if *version {
		fmt.Println(buildinfo.Version)
		return
	}
	if *verifyBundle != "" {
		bundleVersion, err := agent.VerifyStagedBundle(*verifyBundle)
		if err != nil {
			log.Fatal(err)
		}
		fmt.Println(bundleVersion)
		return
	}
	if *scannerFirewall != "" {
		if err := agent.ConfigureScannerFirewall(context.Background(), *scannerFirewall, *scannerOutput, *scannerAllow, *directTraffic); err != nil {
			log.Fatal(err)
		}
		return
	}
	if *gatewayMode != "" {
		if *gatewayMode != "prepare" {
			log.Fatal("gateway-transport supports prepare only")
		}
		if err := agent.PrepareGateway(*gatewayOutput); err != nil {
			log.Fatal(err)
		}
		fmt.Println("Prepared private gateway credentials and reviewable configuration; no services changed.")
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
	if *checkConfig {
		return
	}
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	var m *agent.Manager
	if cfg.IngressOnly {
		m, err = agent.NewIngressReader(ctx, cfg)
	} else {
		m, err = agent.NewManager(ctx, cfg, &agent.Docker{Root: filepath.Join(cfg.DataDir, "builds")})
	}
	if err != nil {
		log.Fatal(err)
	}
	defer m.Close()
	if !cfg.NativeOnly && !cfg.IngressOnly {
		m.StartMaintenance()
	}
	api := &http.Server{Addr: cfg.Listen, Handler: agent.Handler(m), ReadHeaderTimeout: 5 * time.Second, ReadTimeout: 15 * time.Second, WriteTimeout: 35 * time.Second, IdleTimeout: 60 * time.Second}
	var proxy *http.Server
	if !cfg.NativeOnly {
		proxy = &http.Server{Addr: cfg.ProxyListen, Handler: agent.Proxy(m), ReadHeaderTimeout: 5 * time.Second, IdleTimeout: 60 * time.Second}
	}
	gateway := m.GatewayServer()
	errs := make(chan error, 3)
	go func() { errs <- api.ListenAndServe() }()
	if proxy != nil {
		go func() { errs <- proxy.ListenAndServe() }()
	}
	if gateway != nil {
		go func() { errs <- gateway.ListenAndServeTLS("", "") }()
	}
	if cfg.NativeOnly {
		log.Printf("native observer API %s; application ingress disabled", cfg.Listen)
	} else {
		log.Printf("agent API %s; application ingress %s", cfg.Listen, cfg.ProxyListen)
	}
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
	if proxy != nil {
		_ = proxy.Shutdown(shutdown)
	}
	if gateway != nil {
		_ = gateway.Shutdown(shutdown)
	}
}
