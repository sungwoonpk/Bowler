package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"
)

const (
	keyOne = "AQEBAQEBAQEBAQEBAQEBAQEBAQEBAQEBAQEBAQEBAQE=" // 32 bytes of 1
	keyTwo = "AgICAgICAgICAgICAgICAgICAgICAgICAgICAgICAgI=" // 32 bytes of 2
)

func stop(t *testing.T, p *process) {
	t.Helper()
	p.cmd.Process.Signal(syscall.SIGTERM)
	select {
	case <-p.exited:
	case <-time.After(10 * time.Second):
		t.Fatal("it must stop on SIGTERM")
	}
}

// fileHolds reports whether the database file or its journal contains text.
func fileHolds(t *testing.T, db, text string) bool {
	t.Helper()
	for _, f := range []string{db, db + "-wal"} {
		b, err := os.ReadFile(f)
		if err != nil {
			continue
		}
		if strings.Contains(string(b), text) {
			return true
		}
	}
	return false
}

// runToExit starts the program and waits for it to stop by itself, for the cases where it should refuse to run.
func runToExit(t *testing.T, db string, env ...string) (string, int) {
	t.Helper()
	cmd := exec.Command(binary, "serve", "-addr", freeAddr(t), "-db", db)
	cmd.Env = append([]string{"PATH=" + os.Getenv("PATH"), "BOWLER_API_KEY=" + procKey}, env...)
	out, err := cmd.CombinedOutput()
	if exit, ok := err.(*exec.ExitError); ok {
		return string(out), exit.ExitCode()
	}
	if err != nil {
		t.Fatal(err)
	}
	return string(out), 0
}

func deliverOne(t *testing.T, p *process, appName string) (secret string, ok bool) {
	t.Helper()
	cust := newCustomer(t)
	_, app := p.call(t, "POST", "/v1/apps", map[string]any{"name": appName})
	_, ep := p.call(t, "POST", "/v1/apps/"+s(app, "id")+"/endpoints", map[string]any{"url": cust.srv.URL})
	cust.secret.Store(s(ep, "secret"))
	_, msg := p.call(t, "POST", "/v1/apps/"+s(app, "id")+"/messages", map[string]any{"event_type": "x", "payload": map[string]any{"n": 1}})
	waitFor(t, 10*time.Second, "the customer to receive it", func() bool { return cust.received()[s(msg, "id")] == 1 })
	return s(ep, "secret"), cust.badSigs.Load() == 0
}

func TestWithAKeyTheFileHoldsNoSecretAndSigningStillWorksAfterARestart(t *testing.T) {
	db := filepath.Join(t.TempDir(), "bowler.db")
	env := []string{"BOWLER_ENCRYPTION_KEY=" + keyOne}

	p := startWithEnv(t, db, freeAddr(t), env)
	secret, ok := deliverOne(t, p, "acme")
	if !ok {
		t.Fatal("the customer could not verify the signature")
	}
	if strings.Contains(p.logs(), "stored unencrypted") {
		t.Errorf("with a key it must not warn that secrets are unencrypted:\n%s", p.logs())
	}
	stop(t, p)
	if fileHolds(t, db, secret) {
		t.Fatal("the signing secret is in the database file in the clear")
	}

	// After a restart the secret is read back and used.
	p2 := startWithEnv(t, db, freeAddr(t), env)
	_, app := p2.call(t, "GET", "/v1/apps", nil)
	_ = app
	code, list := p2.call(t, "GET", "/v1/apps", nil)
	if code != 200 || len(list["data"].([]any)) != 1 {
		t.Fatalf("%d %v", code, list)
	}
	appID := s(list["data"].([]any)[0].(map[string]any), "id")
	_, eps := p2.call(t, "GET", "/v1/apps/"+appID+"/endpoints", nil)
	epID := s(eps["data"].([]any)[0].(map[string]any), "id")
	_, got := p2.call(t, "GET", "/v1/apps/"+appID+"/endpoints/"+epID+"/secret", nil)
	if s(got, "secret") != secret {
		t.Fatalf("the secret must read back the same after a restart: %q", s(got, "secret"))
	}
}

func TestAddingAKeyToAnExistingDatabaseSealsItsSecrets(t *testing.T) {
	db := filepath.Join(t.TempDir(), "bowler.db")

	old := start(t, db, freeAddr(t))
	secret, _ := deliverOne(t, old, "acme")
	if !strings.Contains(old.logs(), "stored unencrypted") {
		t.Errorf("without a key it should say that secrets are unencrypted:\n%s", old.logs())
	}
	stop(t, old)
	if !fileHolds(t, db, secret) {
		t.Fatal("setup: without a key the secret is stored as it always was")
	}

	p := startWithEnv(t, db, freeAddr(t), []string{"BOWLER_ENCRYPTION_KEY=" + keyOne})
	if !strings.Contains(p.logs(), "sealed under the current key") || !strings.Contains(p.logs(), "compacted") {
		t.Errorf("it should say what it did:\n%s", p.logs())
	}
	stop(t, p)
	if fileHolds(t, db, secret) {
		t.Fatal("after sealing, no plain secret may stay in the file or its journal")
	}
}

func TestRotatingTheKeyNeedsBothKeysOnceAndThenOnlyTheNewOne(t *testing.T) {
	db := filepath.Join(t.TempDir(), "bowler.db")
	one := startWithEnv(t, db, freeAddr(t), []string{"BOWLER_ENCRYPTION_KEY=" + keyOne})
	deliverOne(t, one, "acme")
	stop(t, one)

	rot := startWithEnv(t, db, freeAddr(t), []string{"BOWLER_ENCRYPTION_KEY=" + keyTwo, "BOWLER_ENCRYPTION_KEY_PREVIOUS=" + keyOne})
	if !strings.Contains(rot.logs(), "sealed under the current key") {
		t.Errorf("rotation should re-seal:\n%s", rot.logs())
	}
	stop(t, rot)

	only := startWithEnv(t, db, freeAddr(t), []string{"BOWLER_ENCRYPTION_KEY=" + keyTwo})
	stop(t, only)
	if out, code := runToExit(t, db, "BOWLER_ENCRYPTION_KEY="+keyOne); code == 0 || !strings.Contains(out, "not available") {
		t.Fatalf("the old key alone must be refused, exit %d: %s", code, out)
	}
}

func TestAnEncryptedDatabaseIsRefusedWithoutTheKeyOrWithTheWrongOne(t *testing.T) {
	db := filepath.Join(t.TempDir(), "bowler.db")
	p := startWithEnv(t, db, freeAddr(t), []string{"BOWLER_ENCRYPTION_KEY=" + keyOne})
	deliverOne(t, p, "acme")
	stop(t, p)

	out, code := runToExit(t, db)
	if code == 0 || !strings.Contains(out, "no encryption key is set") || !strings.Contains(out, "BOWLER_ENCRYPTION_KEY") {
		t.Fatalf("without the key it must refuse and say why, exit %d: %s", code, out)
	}
	out, code = runToExit(t, db, "BOWLER_ENCRYPTION_KEY="+keyTwo)
	if code == 0 || !strings.Contains(out, "not available") {
		t.Fatalf("with the wrong key it must refuse, exit %d: %s", code, out)
	}
	if strings.Contains(out, keyTwo) {
		t.Fatalf("a key must never be printed: %s", out)
	}
}

func TestABadEncryptionKeyIsRefusedWithoutBeingPrinted(t *testing.T) {
	for name, env := range map[string][]string{
		"not base64":       {"BOWLER_ENCRYPTION_KEY=this is not base64!!"},
		"too short":        {"BOWLER_ENCRYPTION_KEY=AQEB"},
		"previous alone":   {"BOWLER_ENCRYPTION_KEY_PREVIOUS=" + keyOne},
		"bad previous":     {"BOWLER_ENCRYPTION_KEY=" + keyOne, "BOWLER_ENCRYPTION_KEY_PREVIOUS=nope!!"},
		"same key as both": {"BOWLER_ENCRYPTION_KEY=" + keyOne, "BOWLER_ENCRYPTION_KEY_PREVIOUS=" + keyOne},
	} {
		out, code := runToExit(t, filepath.Join(t.TempDir(), "x.db"), env...)
		if code == 0 || !strings.Contains(out, "ENCRYPTION_KEY") && !strings.Contains(out, "same key") {
			t.Errorf("%s: it should refuse and say why, exit %d: %s", name, code, out)
		}
		for _, e := range env {
			if v := strings.SplitN(e, "=", 2)[1]; len(v) > 10 && strings.Contains(out, v) {
				t.Errorf("%s: the key value was printed: %s", name, out)
			}
		}
	}
}
