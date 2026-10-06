// Command dg-fixture serves DeployGuard's demo API. It exists only to
// demonstrate and test DeployGuard; see internal/fixture for the seeded cases.
//
//	dg-fixture --variant baseline  --addr 127.0.0.1:8080
//	dg-fixture --variant regressed --addr 127.0.0.1:8081
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/AddysEdge/deployguard/internal/fixture"
)

func main() {
	addr := flag.String("addr", "127.0.0.1:8080", "listen address (loopback by default)")
	variantName := flag.String("variant", "baseline", "baseline, clean or regressed")
	slowDelay := flag.Duration("slow-delay", fixture.DefaultSlowDelay, "R5 latency added by the regressed variant to /api/search")
	flag.Parse()

	log := slog.New(slog.NewTextHandler(os.Stderr, nil))
	variant, err := fixture.ParseVariant(*variantName)
	if err != nil {
		fmt.Fprintln(os.Stderr, "dg-fixture:", err)
		os.Exit(2)
	}

	srv := &http.Server{
		Addr:              *addr,
		Handler:           fixture.NewHandler(fixture.Options{Variant: variant, SlowDelay: *slowDelay}),
		ReadHeaderTimeout: 5 * time.Second,
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	go func() {
		<-ctx.Done()
		shutdown, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		srv.Shutdown(shutdown)
	}()

	log.Info("fixture listening", "addr", *addr, "variant", variant)
	if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
		log.Error("fixture stopped", "err", err)
		os.Exit(1)
	}
}
