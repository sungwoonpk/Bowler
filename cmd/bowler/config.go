package main

import (
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"net"
	"os"
	"strconv"
	"strings"
	"time"
)

// config is everything `bowler serve` needs, gathered from flags and the environment.
type config struct {
	Addr         string
	DB           string
	APIKey       string
	AllowPrivate bool
	AllowHTTP    bool
	Workers      int
	Timeout      time.Duration
	Drain        time.Duration
	MaxPayload   int
	LogFormat    string
	LogLevel     slog.Level
}

const minAPIKey = 24

// parseServe reads the serve command's settings. A flag beats an environment variable, which beats the default.
// The API key is read only from the environment: a flag would be visible to every user of the machine in the
// process list.
func parseServe(args []string, getenv func(string) string, stderr io.Writer) (config, error) {
	fs := flag.NewFlagSet("serve", flag.ContinueOnError)
	fs.SetOutput(stderr)
	c := config{}

	envStr := func(name, fallback string) string {
		if v := getenv(name); v != "" {
			return v
		}
		return fallback
	}
	envBool := func(name string) bool {
		v := strings.ToLower(getenv(name))
		return v == "1" || v == "true" || v == "yes"
	}
	envInt := func(name string, fallback int) int {
		if n, err := strconv.Atoi(getenv(name)); err == nil {
			return n
		}
		return fallback
	}
	envDur := func(name string, fallback time.Duration) time.Duration {
		if d, err := time.ParseDuration(getenv(name)); err == nil {
			return d
		}
		return fallback
	}

	fs.StringVar(&c.Addr, "addr", envStr("BOWLER_ADDR", "127.0.0.1:8080"), "address to listen on (BOWLER_ADDR)")
	fs.StringVar(&c.DB, "db", envStr("BOWLER_DB", "bowler.db"), "path of the SQLite database (BOWLER_DB)")
	fs.BoolVar(&c.AllowPrivate, "allow-private", envBool("BOWLER_ALLOW_PRIVATE"), "allow endpoints on private and loopback addresses. For development only (BOWLER_ALLOW_PRIVATE)")
	fs.BoolVar(&c.AllowHTTP, "allow-http", envBool("BOWLER_ALLOW_HTTP"), "allow endpoints that use plain http (BOWLER_ALLOW_HTTP)")
	fs.IntVar(&c.Workers, "workers", envInt("BOWLER_WORKERS", 128), "most deliveries in progress at once (BOWLER_WORKERS)")
	fs.DurationVar(&c.Timeout, "timeout", envDur("BOWLER_TIMEOUT", 15*time.Second), "how long one delivery attempt may take (BOWLER_TIMEOUT)")
	fs.DurationVar(&c.Drain, "drain", envDur("BOWLER_DRAIN", 10*time.Second), "how long to let attempts finish on shutdown (BOWLER_DRAIN)")
	fs.IntVar(&c.MaxPayload, "max-payload", envInt("BOWLER_MAX_PAYLOAD", 256<<10), "largest message payload in bytes (BOWLER_MAX_PAYLOAD)")
	fs.StringVar(&c.LogFormat, "log-format", envStr("BOWLER_LOG_FORMAT", "json"), "json or text (BOWLER_LOG_FORMAT)")
	level := fs.String("log-level", envStr("BOWLER_LOG_LEVEL", "info"), "debug, info, warn or error (BOWLER_LOG_LEVEL)")
	if err := fs.Parse(args); err != nil {
		return c, err
	}

	c.APIKey = getenv("BOWLER_API_KEY")
	if len(c.APIKey) < minAPIKey {
		return c, errors.New("BOWLER_API_KEY must be set to at least 24 characters. Make one with: openssl rand -hex 24")
	}
	if _, _, err := net.SplitHostPort(c.Addr); err != nil {
		return c, fmt.Errorf("--addr %q is not a host:port address", c.Addr)
	}
	switch c.LogFormat {
	case "json", "text":
	default:
		return c, errors.New("--log-format must be json or text")
	}
	if err := c.LogLevel.UnmarshalText([]byte(*level)); err != nil {
		return c, errors.New("--log-level must be debug, info, warn or error")
	}
	if c.Workers < 1 || c.Workers > 10_000 {
		return c, errors.New("--workers must be from 1 to 10000")
	}
	if c.Timeout < time.Second || c.Timeout > 5*time.Minute {
		return c, errors.New("--timeout must be from 1s to 5m")
	}
	if c.Drain < 0 || c.Drain > 10*time.Minute {
		return c, errors.New("--drain must be from 0 to 10m")
	}
	if c.MaxPayload < 1024 || c.MaxPayload > 1<<20 {
		return c, errors.New("--max-payload must be from 1024 to 1048576 bytes")
	}
	return c, nil
}

func newLogger(c config, w io.Writer) *slog.Logger {
	opts := &slog.HandlerOptions{Level: c.LogLevel}
	if c.LogFormat == "text" {
		return slog.New(slog.NewTextHandler(w, opts))
	}
	return slog.New(slog.NewJSONHandler(w, opts))
}

func osGetenv(name string) string { return os.Getenv(name) }
