package main

import (
	"io"
	"log/slog"
	"strings"
	"testing"
	"time"
)

const goodKey = "0123456789abcdef0123456789abcdef"

func env(m map[string]string) func(string) string { return func(k string) string { return m[k] } }

func parse(t *testing.T, args []string, vars map[string]string) (config, error) {
	t.Helper()
	return parseServe(args, env(vars), io.Discard)
}

func TestDefaults(t *testing.T) {
	c, err := parse(t, nil, map[string]string{"BOWLER_API_KEY": goodKey})
	if err != nil {
		t.Fatal(err)
	}
	if c.Addr != "127.0.0.1:8080" {
		t.Errorf("it must listen on loopback by default so a quick local run is not exposed, got %s", c.Addr)
	}
	if c.AllowPrivate || c.AllowHTTP {
		t.Error("the unsafe options must be off by default")
	}
	if c.Retention != 30*24*time.Hour {
		t.Errorf("finished data should be kept 30 days by default, got %s", c.Retention)
	}
	if c.Timeout != 15*time.Second || c.Lease != 60*time.Second || c.Workers != 128 || c.LogFormat != "json" || c.LogLevel != slog.LevelInfo {
		t.Errorf("%+v", c)
	}
}

func TestTheAPIKeyIsRequiredAndLongEnough(t *testing.T) {
	for name, key := range map[string]string{"missing": "", "short": "short", "one short of enough": strings.Repeat("k", 23)} {
		if _, err := parse(t, nil, map[string]string{"BOWLER_API_KEY": key}); err == nil {
			t.Errorf("%s: a weak key must be refused", name)
		}
	}
	if _, err := parse(t, nil, map[string]string{"BOWLER_API_KEY": strings.Repeat("k", 24)}); err != nil {
		t.Errorf("24 characters is enough: %v", err)
	}
}

func TestTheAPIKeyCannotBeGivenAsAFlag(t *testing.T) {
	// A flag would show up in the process list for every user of the machine.
	if _, err := parse(t, []string{"-api-key", goodKey}, nil); err == nil {
		t.Fatal("there must be no flag for the API key")
	}
}

func TestFlagsBeatEnvironmentWhichBeatsDefaults(t *testing.T) {
	vars := map[string]string{"BOWLER_API_KEY": goodKey, "BOWLER_ADDR": "0.0.0.0:9000", "BOWLER_WORKERS": "10", "BOWLER_ALLOW_HTTP": "true"}
	c, err := parse(t, nil, vars)
	if err != nil || c.Addr != "0.0.0.0:9000" || c.Workers != 10 || !c.AllowHTTP {
		t.Fatalf("environment should apply: %+v %v", c, err)
	}
	c, err = parse(t, []string{"-addr", "127.0.0.1:7000", "-workers", "5"}, vars)
	if err != nil || c.Addr != "127.0.0.1:7000" || c.Workers != 5 {
		t.Fatalf("a flag should win: %+v %v", c, err)
	}
}

func TestLeaseMustOutlastAnAttempt(t *testing.T) {
	vars := map[string]string{"BOWLER_API_KEY": goodKey}
	if _, err := parse(t, []string{"-timeout", "30s", "-lease", "31s"}, vars); err == nil {
		t.Fatal("a lease that barely outlasts an attempt would hand a running attempt to a second worker")
	}
	c, err := parse(t, []string{"-timeout", "30s"}, vars)
	if err != nil || c.Lease != 75*time.Second {
		t.Fatalf("the default lease follows the timeout: %+v %v", c, err)
	}
	if c, err := parse(t, []string{"-timeout", "2s", "-lease", "8s"}, vars); err != nil || c.Lease != 8*time.Second {
		t.Fatalf("a short lease is allowed when it is long enough: %+v %v", c, err)
	}
}

func TestBadValuesAreRefusedWithAClearReason(t *testing.T) {
	vars := map[string]string{"BOWLER_API_KEY": goodKey}
	cases := map[string][]string{
		"address without a port": {"-addr", "localhost"},
		"unknown log format":     {"-log-format", "xml"},
		"unknown log level":      {"-log-level", "loud"},
		"no workers":             {"-workers", "0"},
		"too many workers":       {"-workers", "100000"},
		"timeout too short":      {"-timeout", "100ms"},
		"timeout too long":       {"-timeout", "1h"},
		"negative drain":         {"-drain", "-1s"},
		"tiny payload limit":     {"-max-payload", "10"},
		"huge payload limit":     {"-max-payload", "999999999"},
		"unknown flag":           {"-nope"},
		"retention too short":    {"-retention", "1h"},
		"negative retention":     {"-retention", "-24h"},
	}
	for name, args := range cases {
		if _, err := parse(t, args, vars); err == nil {
			t.Errorf("%s should be refused", name)
		}
	}
}

func TestLoggerFormats(t *testing.T) {
	var out strings.Builder
	newLogger(config{LogFormat: "json", LogLevel: slog.LevelInfo}, &out).Info("hello", "k", "v")
	if !strings.HasPrefix(out.String(), "{") || !strings.Contains(out.String(), `"k":"v"`) {
		t.Errorf("json: %s", out.String())
	}
	out.Reset()
	newLogger(config{LogFormat: "text", LogLevel: slog.LevelInfo}, &out).Info("hello")
	if strings.HasPrefix(out.String(), "{") {
		t.Errorf("text: %s", out.String())
	}
	out.Reset()
	newLogger(config{LogFormat: "json", LogLevel: slog.LevelWarn}, &out).Info("quiet")
	if out.Len() != 0 {
		t.Errorf("a message below the level must not be written: %s", out.String())
	}
}

func TestRetentionCanBeChangedOrSwitchedOff(t *testing.T) {
	vars := map[string]string{"BOWLER_API_KEY": goodKey, "BOWLER_RETENTION": "720h"}
	if c, err := parse(t, nil, vars); err != nil || c.Retention != 720*time.Hour {
		t.Fatalf("environment should apply: %+v %v", c, err)
	}
	if c, err := parse(t, []string{"-retention", "48h"}, vars); err != nil || c.Retention != 48*time.Hour {
		t.Fatalf("a flag should win: %+v %v", c, err)
	}
	if c, err := parse(t, []string{"-retention", "0"}, vars); err != nil || c.Retention != 0 {
		t.Fatalf("zero keeps everything: %+v %v", c, err)
	}
}
