package main

import (
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/netip"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/sungwoonpk/bowler/internal/secretbox"
	"github.com/sungwoonpk/bowler/internal/ssrf"
)

// config is everything `bowler serve` needs, gathered from flags and the environment.
type config struct {
	Addr   string
	DB     string
	APIKey string
	// EncryptionKey seals signing secrets in the database. Nil leaves them as they are. EncryptionKeyPrevious is the key
	// being rotated out, still needed to open what it sealed. Both come from the environment only.
	EncryptionKey         []byte
	EncryptionKeyPrevious []byte
	AllowPrivate          bool
	// AllowCIDRs are private ranges that may be called without turning on AllowPrivate.
	AllowCIDRs []netip.Prefix
	AllowHTTP  bool
	Workers    int
	Timeout    time.Duration
	Lease      time.Duration
	Drain      time.Duration
	MaxPayload int
	// Retention is how long finished data is kept. Zero keeps it for ever.
	Retention time.Duration
	// AuditRetention is how long the audit log is kept. Zero keeps it for ever.
	AuditRetention time.Duration
	LogFormat      string
	LogLevel       slog.Level
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
	// The flag can be given more than once, and each value can hold several ranges separated by commas. A flag replaces the
	// environment variable as a whole, as every other setting does.
	var cidrFlags []string
	fs.Func("allow-cidr", "a private range endpoints may use, such as 10.20.0.0/16. Repeat it or separate with commas (BOWLER_ALLOW_CIDRS)", func(v string) error {
		cidrFlags = append(cidrFlags, v)
		return nil
	})
	fs.BoolVar(&c.AllowHTTP, "allow-http", envBool("BOWLER_ALLOW_HTTP"), "allow endpoints that use plain http (BOWLER_ALLOW_HTTP)")
	fs.IntVar(&c.Workers, "workers", envInt("BOWLER_WORKERS", 128), "most deliveries in progress at once (BOWLER_WORKERS)")
	fs.DurationVar(&c.Timeout, "timeout", envDur("BOWLER_TIMEOUT", 15*time.Second), "how long one delivery attempt may take (BOWLER_TIMEOUT)")
	fs.DurationVar(&c.Lease, "lease", envDur("BOWLER_LEASE", 0), "how long work taken by an instance that dies waits before another takes it. Default: the attempt timeout plus 45s (BOWLER_LEASE)")
	fs.DurationVar(&c.Drain, "drain", envDur("BOWLER_DRAIN", 10*time.Second), "how long to let attempts finish on shutdown (BOWLER_DRAIN)")
	fs.IntVar(&c.MaxPayload, "max-payload", envInt("BOWLER_MAX_PAYLOAD", 256<<10), "largest message payload in bytes (BOWLER_MAX_PAYLOAD)")
	fs.DurationVar(&c.Retention, "retention", envDur("BOWLER_RETENTION", 30*24*time.Hour), "how long finished messages and their history are kept, 0 to keep for ever (BOWLER_RETENTION)")
	fs.DurationVar(&c.AuditRetention, "audit-retention", envDur("BOWLER_AUDIT_RETENTION", 365*24*time.Hour), "how long the audit log is kept, 0 to keep for ever (BOWLER_AUDIT_RETENTION)")
	fs.StringVar(&c.LogFormat, "log-format", envStr("BOWLER_LOG_FORMAT", "json"), "json or text (BOWLER_LOG_FORMAT)")
	level := fs.String("log-level", envStr("BOWLER_LOG_LEVEL", "info"), "debug, info, warn or error (BOWLER_LOG_LEVEL)")
	if err := fs.Parse(args); err != nil {
		return c, err
	}

	c.APIKey = getenv("BOWLER_API_KEY")
	if len(c.APIKey) < minAPIKey {
		return c, errors.New("BOWLER_API_KEY must be set to at least 24 characters. Make one with: openssl rand -hex 24")
	}
	// Like the API key, the encryption key can only come from the environment, and its value is never put in an error.
	if v := getenv("BOWLER_ENCRYPTION_KEY"); v != "" {
		k, err := secretbox.ParseKey(v)
		if err != nil {
			return c, fmt.Errorf("BOWLER_ENCRYPTION_KEY: %w", err)
		}
		c.EncryptionKey = k
	}
	if v := getenv("BOWLER_ENCRYPTION_KEY_PREVIOUS"); v != "" {
		if c.EncryptionKey == nil {
			return c, errors.New("BOWLER_ENCRYPTION_KEY_PREVIOUS is only for rotating: set BOWLER_ENCRYPTION_KEY to the new key as well")
		}
		k, err := secretbox.ParseKey(v)
		if err != nil {
			return c, fmt.Errorf("BOWLER_ENCRYPTION_KEY_PREVIOUS: %w", err)
		}
		c.EncryptionKeyPrevious = k
	}
	cidrs := cidrFlags
	if len(cidrs) == 0 && getenv("BOWLER_ALLOW_CIDRS") != "" {
		cidrs = []string{getenv("BOWLER_ALLOW_CIDRS")}
	}
	var ranges []string
	for _, v := range cidrs {
		ranges = append(ranges, strings.Split(v, ",")...)
	}
	if len(ranges) > 0 {
		list, err := ssrf.ParseAllowList(ranges)
		if err != nil {
			return c, fmt.Errorf("--allow-cidr: %w", err)
		}
		c.AllowCIDRs = list
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
	if c.Lease == 0 {
		c.Lease = c.Timeout + 45*time.Second
	}
	if c.Lease < c.Timeout+5*time.Second {
		return c, errors.New("--lease must be at least 5s longer than --timeout, or a slow attempt would be handed to a second worker while it is still running")
	}
	if c.Drain < 0 || c.Drain > 10*time.Minute {
		return c, errors.New("--drain must be from 0 to 10m")
	}
	if c.MaxPayload < 1024 || c.MaxPayload > 1<<20 {
		return c, errors.New("--max-payload must be from 1024 to 1048576 bytes")
	}
	if c.Retention != 0 && c.Retention < 24*time.Hour {
		return c, errors.New("--retention must be 0 to keep for ever, or at least 24h. A shorter time would delete history before a slow retry schedule has finished")
	}
	if c.AuditRetention != 0 && c.AuditRetention < 7*24*time.Hour {
		return c, errors.New("--audit-retention must be 0 to keep for ever, or at least 168h (7 days). A record of who did what is of little use if it is gone within days")
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
