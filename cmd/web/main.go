package main

import (
	"cmp"
	"context"
	"errors"
	"flag"
	"fmt"
	"github.com/agim/lidza-deploy/internal/agent"
	"github.com/agim/lidza-deploy/internal/buildinfo"
	"github.com/agim/lidza-deploy/internal/control"
	"github.com/agim/lidza-deploy/internal/onboarding"
	"github.com/agim/lidza-deploy/internal/webapp"
	"github.com/agim/lidza-deploy/web"
	"github.com/agim/lidza/pkg/env"
	"log"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"
	"time"
)

func main() {
	version := flag.Bool("version", false, "print release version")
	verifyBundle := flag.String("verify-bundle", "", "verify a staged signed update bundle directory with the pinned keys and print its version")
	flag.Parse()
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
	if err := run(); err != nil {
		log.Fatal(err)
	}
}
func run() error {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	values, err := env.Values(".")
	if err != nil {
		return err
	}
	addr := values["LIDZA_ADDR"]
	if addr == "" {
		addr = "127.0.0.1:3000"
	}
	dir := values["CONTROL_DATA_DIR"]
	if dir == "" {
		dir = ".lidza-deploy"
	}
	dir, err = filepath.Abs(dir)
	if err != nil {
		return err
	}
	if err = os.Setenv("CONTROL_DATA_DIR", dir); err != nil {
		return err
	}
	var handler http.Handler
	var closeApp func(context.Context) error
	_, tokenErr := os.Stat(filepath.Join(dir, "setup-token"))
	_, doneErr := os.Stat(filepath.Join(dir, "setup-complete.json"))
	legacyConfigured := os.IsNotExist(tokenErr) && os.IsNotExist(doneErr) && values["PUBLIC_URL"] != "" && values["CONTROL_USER"] != ""
	// A panel that is not the active one serves only a notice and never
	// boots the application: no ticks, jobs or updates, no agent calls.
	// A standby copy still starts its database container so the move can
	// restore into it.
	if movedTo, moved := control.MovedTo(dir); moved {
		return serveNotice(ctx, addr, control.MovedHandler(cmp.Or(movedTo, values["PUBLIC_URL"])), "moved away")
	}
	if values[agent.ControlStandbyEnv] == "1" {
		if doneErr == nil {
			recoverCtx, cancel := context.WithTimeout(ctx, 90*time.Second)
			err = onboarding.RecoverManagedDatabase(recoverCtx, dir)
			cancel()
			if err != nil {
				return err
			}
		}
		return serveNotice(ctx, addr, control.StandbyHandler(), "standing by")
	}
	if doneErr == nil || legacyConfigured {
		recoverCtx, cancel := context.WithTimeout(ctx, 90*time.Second)
		err = onboarding.RecoverManagedDatabase(recoverCtx, dir)
		cancel()
		if err != nil {
			return err
		}
	}
	if legacyConfigured {
		booted, err := webapp.Boot(ctx)
		if err != nil {
			return err
		}
		handler = booted.Handler
		closeApp = booted.Close
	} else {
		if err = os.MkdirAll(dir, 0700); err != nil {
			return err
		}
		if err = os.Chdir(dir); err != nil {
			return err
		}
		setup, err := onboarding.New(onboarding.Options{Dir: dir, Origin: values["CONTROL_SETUP_ORIGIN"], Context: ctx, Boot: webapp.Boot, Frontend: web.Handler()})
		if err != nil {
			return err
		}
		handler = setup
		closeApp = setup.Close
		if _, err = os.Stat(filepath.Join(dir, "setup-token")); err == nil {
			log.Printf("First-run setup ready; one-time credential is in %s", filepath.Join(dir, "setup-token"))
			if values["CONTROL_SETUP_ORIGIN"] != "" {
				log.Printf("Setup address: %s", values["CONTROL_SETUP_ORIGIN"])
			}
		}
	}
	// The panel's own database container may come back on another port
	// after a Docker restart; the watch corrects DATABASE_URL and ends the
	// process cleanly so the service manager restarts it onto the new one.
	onboarding.WatchManagedDatabase(ctx, dir, stop)
	server := &http.Server{Addr: addr, Handler: handler, ReadHeaderTimeout: 5 * time.Second, ReadTimeout: 30 * time.Second, WriteTimeout: 3 * time.Minute, IdleTimeout: 60 * time.Second, MaxHeaderBytes: 16 << 10}
	failures := make(chan error, 1)
	go func() { failures <- server.ListenAndServe() }()
	log.Printf("Control panel listening on %s", addr)
	var serveErr error
	select {
	case <-ctx.Done():
	case serveErr = <-failures:
	}
	shutdown, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	_ = server.Shutdown(shutdown)
	// State changes are written in the background; write the last ones.
	control.FlushState(shutdown)
	_ = closeApp(shutdown)
	if errors.Is(serveErr, http.ErrServerClosed) {
		return nil
	}
	return serveErr
}

func serveNotice(ctx context.Context, addr string, handler http.Handler, mode string) error {
	server := &http.Server{Addr: addr, Handler: handler, ReadHeaderTimeout: 5 * time.Second, ReadTimeout: 30 * time.Second, WriteTimeout: 30 * time.Second, IdleTimeout: 60 * time.Second, MaxHeaderBytes: 16 << 10}
	failures := make(chan error, 1)
	go func() { failures <- server.ListenAndServe() }()
	log.Printf("Control panel %s; notice on %s", mode, addr)
	select {
	case <-ctx.Done():
	case err := <-failures:
		if !errors.Is(err, http.ErrServerClosed) {
			return err
		}
	}
	shutdown, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	return server.Shutdown(shutdown)
}
