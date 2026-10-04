package main

import (
	"context"
	"errors"
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
	if os.IsNotExist(tokenErr) && os.IsNotExist(doneErr) && values["PUBLIC_URL"] != "" && values["CONTROL_USER"] != "" {
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
	_ = closeApp(shutdown)
	if errors.Is(serveErr, http.ErrServerClosed) {
		return nil
	}
	return serveErr
}
