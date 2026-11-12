// Command bowler is a webhook delivery service.
//
//	bowler serve     run the service
//	bowler demo      run it with sample receivers, to see it work
//	bowler version   print the version
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/sungwoonpk/bowler/internal/api"
	"github.com/sungwoonpk/bowler/internal/deliver"
	"github.com/sungwoonpk/bowler/internal/dispatcher"
	"github.com/sungwoonpk/bowler/internal/retention"
	"github.com/sungwoonpk/bowler/internal/secretbox"
	"github.com/sungwoonpk/bowler/internal/ssrf"
	"github.com/sungwoonpk/bowler/internal/store"
)

// version is set at build time with -ldflags "-X main.version=...".
var version = "dev"

func main() {
	if len(os.Args) < 2 {
		usage()
		os.Exit(2)
	}
	switch os.Args[1] {
	case "serve":
		cfg, err := parseServe(os.Args[2:], osGetenv, os.Stderr)
		if err != nil {
			fmt.Fprintln(os.Stderr, "bowler:", err)
			os.Exit(2)
		}
		ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
		defer stop()
		if err := serve(ctx, cfg); err != nil {
			fmt.Fprintln(os.Stderr, "bowler:", err)
			os.Exit(1)
		}
	case "demo":
		ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
		defer stop()
		if err := runDemo(ctx, os.Args[2:], os.Stdout); err != nil {
			fmt.Fprintln(os.Stderr, "bowler:", err)
			os.Exit(1)
		}
	case "healthcheck":
		os.Exit(healthcheck(os.Args[2:], os.Getenv, os.Stderr))
	case "version", "--version", "-v":
		fmt.Println("bowler", version)
	case "help", "--help", "-h":
		usage()
	default:
		fmt.Fprintf(os.Stderr, "bowler: unknown command %q\n\n", os.Args[1])
		usage()
		os.Exit(2)
	}
}

func usage() {
	fmt.Fprint(os.Stderr, `Bowler delivers webhooks: it signs them, retries them, and shows what happened to each one.

Usage:
  bowler serve [flags]    run the service (set BOWLER_API_KEY first)
  bowler demo             run it with sample receivers that fail in different ways
  bowler healthcheck      exit 0 if a running instance is ready (for container health checks)
  bowler version

Run "bowler serve -h" for the settings.
`)
}

// serve runs the service until ctx ends, then shuts down in order: stop taking new messages, let deliveries in
// progress finish, and only then close the database.
func serve(ctx context.Context, cfg config) error {
	log := newLogger(cfg, os.Stderr)
	policy := ssrf.Policy{AllowPrivate: cfg.AllowPrivate, AllowHTTP: cfg.AllowHTTP}
	if cfg.AllowPrivate {
		log.Warn("private and loopback addresses are allowed. This is for development, never for a service that takes URLs from strangers")
	}
	if cfg.AllowHTTP {
		log.Warn("plain http endpoints are allowed, so webhook bodies may cross the network unencrypted")
	}

	var box *secretbox.Box
	if cfg.EncryptionKey != nil {
		var previous [][]byte
		if cfg.EncryptionKeyPrevious != nil {
			previous = append(previous, cfg.EncryptionKeyPrevious)
		}
		var err error
		if box, err = secretbox.New(cfg.EncryptionKey, previous...); err != nil {
			return err
		}
	}
	var opts []store.Option
	if box != nil {
		opts = append(opts, store.WithSecretBox(box))
	}
	db, err := store.Open(cfg.DB, opts...)
	if err != nil {
		return err
	}
	defer func() {
		// Closing writes the last of the journal into the database file, so a failure here is worth knowing about.
		if err := db.Close(); err != nil {
			log.Error("closing the database failed", "error", err)
		}
	}()

	// Seal what is still plain, or sealed under a key being rotated out. This also opens every secret, so a wrong key or a
	// damaged value stops the program here and not later, when an endpoint is first used.
	resealed, err := db.ResealSecrets(ctx, 200)
	switch {
	case errors.Is(err, secretbox.ErrNoKey):
		return fmt.Errorf("this database holds encrypted signing secrets, so BOWLER_ENCRYPTION_KEY must be set to the key they were sealed with (%w)", err)
	case errors.Is(err, secretbox.ErrUnknownKey):
		return fmt.Errorf("a signing secret is sealed under a key that is not available. Set BOWLER_ENCRYPTION_KEY to the key it was sealed with, or while rotating, BOWLER_ENCRYPTION_KEY_PREVIOUS to the old one (%w)", err)
	case err != nil:
		return fmt.Errorf("signing secrets: %w", err)
	}
	if resealed > 0 {
		log.Info("signing secrets were sealed under the current key", "endpoints", resealed)
		// The old text stays in unused parts of the file and in the journal until it is rewritten.
		if err := db.Compact(ctx); err != nil {
			return fmt.Errorf("removing the old secrets from the file: %w", err)
		}
		log.Info("the database file was compacted so no plain secret stays in it")
	}
	if box == nil {
		log.Warn("BOWLER_ENCRYPTION_KEY is not set, so signing secrets are stored unencrypted. Make a key with: openssl rand -base64 32")
	}

	sender := deliver.New(deliver.Config{SSRF: policy, Timeout: cfg.Timeout, UserAgent: "Bowler/" + version})
	defer sender.Close()

	disp, err := dispatcher.New(db, sender, dispatcher.Config{
		Workers: cfg.Workers, AttemptTimeout: cfg.Timeout, LeaseFor: cfg.Lease,
		DrainTimeout: cfg.Drain, Log: log,
	})
	if err != nil {
		return err
	}
	var janitor *retention.Janitor
	var retentionStats func() retention.Stats
	if cfg.Retention > 0 || cfg.AuditRetention > 0 {
		janitor, err = retention.New(retention.Config{Store: db, Keep: cfg.Retention, KeepAudit: cfg.AuditRetention, Log: log})
		if err != nil {
			return err
		}
		retentionStats = janitor.Stats
	}
	apiServer, err := api.New(api.Config{
		Store: db, APIKey: cfg.APIKey, SSRF: policy, Notify: disp.Notify, Stats: disp.Stats, Retention: retentionStats,
		MaxPayloadBytes: cfg.MaxPayload, Version: version, Log: log,
	})
	if err != nil {
		return err
	}

	httpServer := &http.Server{
		Addr:              cfg.Addr,
		Handler:           apiServer.Handler(),
		ReadHeaderTimeout: 10 * time.Second,
		ReadTimeout:       30 * time.Second,
		WriteTimeout:      30 * time.Second,
		IdleTimeout:       2 * time.Minute,
		MaxHeaderBytes:    64 << 10,
	}

	dispatcherDone := make(chan struct{})
	go func() {
		defer close(dispatcherDone)
		_ = disp.Run(ctx)
	}()

	janitorDone := make(chan struct{})
	go func() {
		defer close(janitorDone)
		if janitor != nil {
			janitor.Run(ctx)
		}
	}()

	serverErr := make(chan error, 1)
	go func() {
		log.Info("listening", "addr", cfg.Addr, "db", cfg.DB, "version", version)
		serverErr <- httpServer.ListenAndServe()
	}()

	select {
	case err := <-serverErr:
		if !errors.Is(err, http.ErrServerClosed) {
			return fmt.Errorf("the HTTP server stopped: %w", err)
		}
	case <-ctx.Done():
	}

	log.Info("shutting down")
	shutdown, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := httpServer.Shutdown(shutdown); err != nil {
		log.Warn("the HTTP server did not stop cleanly", "error", err)
	}
	<-dispatcherDone // waits for deliveries in progress, up to the drain time
	<-janitorDone
	log.Info("stopped")
	return nil
}

// healthcheck asks a running instance whether it is ready and returns the exit code to use. Container images with no
// shell or curl in them cannot run a health check any other way, so the program does it itself.
func healthcheck(args []string, getenv func(string) string, stderr io.Writer) int {
	fs := flag.NewFlagSet("healthcheck", flag.ContinueOnError)
	fs.SetOutput(stderr)
	addr := fs.String("addr", getenv("BOWLER_ADDR"), "address of the instance (BOWLER_ADDR). Default 127.0.0.1:8080")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	host, port, err := net.SplitHostPort(*addr)
	if err != nil {
		host, port = "127.0.0.1", "8080"
		if *addr != "" {
			fmt.Fprintf(stderr, "healthcheck: %q is not a host:port address\n", *addr)
			return 2
		}
	}
	// An instance listening on every interface is reached through the loopback address.
	if host == "" || host == "0.0.0.0" || host == "::" {
		host = "127.0.0.1"
	}
	client := &http.Client{Timeout: 3 * time.Second}
	resp, err := client.Get("http://" + net.JoinHostPort(host, port) + "/readyz")
	if err != nil {
		fmt.Fprintln(stderr, "healthcheck:", err)
		return 1
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		fmt.Fprintf(stderr, "healthcheck: the instance answered %d\n", resp.StatusCode)
		return 1
	}
	return 0
}
