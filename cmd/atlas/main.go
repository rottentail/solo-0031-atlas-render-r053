package main

import (
	"atlas-render/internal/engine"
	"atlas-render/internal/httpapi"
	"atlas-render/internal/model"
	"atlas-render/internal/persist"
	"atlas-render/internal/service"
	"context"
	"errors"
	"log"
	"net/http"
	"os"
	"os/signal"
	"strconv"
	"syscall"
	"time"
)

func main() {
	if e := run(); e != nil {
		log.Print(e)
		os.Exit(1)
	}
}
func loadConfig() (persist.Config, error) {
	cfg := persist.Config{
		MarginBytes:  persist.DefaultMarginBytes,
		MarginInodes: persist.DefaultMarginInodes,
	}
	if v := os.Getenv("ATLAS_DISK_MARGIN_BYTES"); v != "" {
		n, e := strconv.ParseUint(v, 10, 64)
		if e != nil {
			return cfg, errors.New("ATLAS_DISK_MARGIN_BYTES must be a non-negative integer")
		}
		cfg.MarginBytes = n
	}
	if v := os.Getenv("ATLAS_DISK_MARGIN_INODES"); v != "" {
		n, e := strconv.ParseUint(v, 10, 64)
		if e != nil {
			return cfg, errors.New("ATLAS_DISK_MARGIN_INODES must be a non-negative integer")
		}
		cfg.MarginInodes = n
	}
	cfg.FaultFull = os.Getenv("ATLAS_FAULT_FULL") == "1" || os.Getenv("ATLAS_FAULT_FULL") == "true"
	return cfg, nil
}
func run() error {
	addr := os.Getenv("ATLAS_ADDR")
	if addr == "" {
		addr = "127.0.0.1:8763"
	}
	path := os.Getenv("ATLAS_STATE")
	if path == "" {
		path = "var/atlas-state.json"
	}
	cfg, e := loadConfig()
	if e != nil {
		return e
	}
	st, e := persist.Open(path, cfg)
	if e != nil {
		return e
	}
	defer st.Close()
	worker := engine.New(st)
	if e = worker.Recover(); e != nil {
		// A full state volume must not crash the process: keep serving reads
		// and diagnostics in degraded mode until space is restored. A corrupt
		// or invalid state is still fatal and never overwritten.
		var de *model.Error
		if errors.As(e, &de) && de.Code == "disk_full" {
			log.Printf("startup recovery deferred: %v", e)
		} else {
			return e
		}
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	done := make(chan struct{})
	go func() { defer close(done); worker.Run(ctx) }()
	srv := &http.Server{Addr: addr, Handler: (httpapi.API{Service: &service.Service{Store: st}, Worker: worker}).Handler(), ReadHeaderTimeout: 5 * time.Second, ReadTimeout: 20 * time.Second, WriteTimeout: 60 * time.Second, IdleTimeout: 60 * time.Second, MaxHeaderBytes: 16384}
	errc := make(chan error, 1)
	go func() { log.Printf("Atlas Render listening on %s", addr); errc <- srv.ListenAndServe() }()
	select {
	case e = <-errc:
		stop()
	case <-ctx.Done():
	}
	shutdown, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	_ = srv.Shutdown(shutdown)
	<-done
	if e == http.ErrServerClosed {
		return nil
	}
	return e
}
