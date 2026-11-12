package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"github.com/sungwoonpk/bowler/signature"
)

// These tests run the real program. They build it once and start it as a separate process, which is the only way
// to test what happens when it is killed.

var binary string

func TestMain(m *testing.M) {
	dir, err := os.MkdirTemp("", "bowler-bin-")
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	binary = filepath.Join(dir, "bowler")
	build := exec.Command("go", "build", "-o", binary, ".")
	if out, err := build.CombinedOutput(); err != nil {
		fmt.Fprintf(os.Stderr, "cannot build the program for the tests: %v\n%s", err, out)
		os.Exit(1)
	}
	code := m.Run()
	os.RemoveAll(dir)
	os.Exit(code)
}

const procKey = "process-test-key-0123456789abcdef"

func freeAddr(t *testing.T) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	return ln.Addr().String()
}

type process struct {
	cmd    *exec.Cmd
	addr   string
	mu     sync.Mutex
	stderr bytes.Buffer
	exited chan struct{}
	code   int
}

func (p *process) logs() string { p.mu.Lock(); defer p.mu.Unlock(); return p.stderr.String() }

type lockedWriter struct{ p *process }

func (w lockedWriter) Write(b []byte) (int, error) {
	w.p.mu.Lock()
	defer w.p.mu.Unlock()
	return w.p.stderr.Write(b)
}

func start(t *testing.T, db, addr string, extra ...string) *process {
	t.Helper()
	args := append([]string{"serve", "-addr", addr, "-db", db, "-allow-private", "-allow-http"}, extra...)
	cmd := exec.Command(binary, args...)
	cmd.Env = append(os.Environ(), "BOWLER_API_KEY="+procKey)
	p := &process{cmd: cmd, addr: addr, exited: make(chan struct{})}
	cmd.Stderr = lockedWriter{p}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	go func() {
		err := cmd.Wait()
		p.code = cmd.ProcessState.ExitCode()
		_ = err
		close(p.exited)
	}()
	t.Cleanup(func() {
		select {
		case <-p.exited:
		default:
			cmd.Process.Kill()
			<-p.exited
		}
	})
	waitFor(t, 10*time.Second, "the service to answer", func() bool {
		resp, err := http.Get("http://" + addr + "/healthz")
		if err != nil {
			return false
		}
		resp.Body.Close()
		return resp.StatusCode == 200
	})
	return p
}

func waitFor(t *testing.T, within time.Duration, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(within)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for: %s", what)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

func (p *process) call(t *testing.T, method, path string, body any) (int, map[string]any) {
	t.Helper()
	var rd io.Reader
	if body != nil {
		b, _ := json.Marshal(body)
		rd = bytes.NewReader(b)
	}
	req, _ := http.NewRequest(method, "http://"+p.addr+path, rd)
	req.Header.Set("Authorization", "Bearer "+procKey)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var out map[string]any
	json.NewDecoder(resp.Body).Decode(&out)
	return resp.StatusCode, out
}

func s(m map[string]any, k string) string { v, _ := m[k].(string); return v }

// customer is a receiver that records every webhook it is sent, and can be told to hold requests open.
type customer struct {
	srv     *httptest.Server
	mu      sync.Mutex
	seen    map[string]int // message id -> times received
	hold    atomic.Bool
	arrived atomic.Int32
	secret  atomic.Value
	badSigs atomic.Int32
}

func newCustomer(t *testing.T) *customer {
	t.Helper()
	c := &customer{seen: map[string]int{}}
	c.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		id := r.Header.Get("webhook-id")
		secret, _ := c.secret.Load().(string)
		if err := signature.Verify([]string{secret}, id, r.Header.Get("webhook-timestamp"), r.Header.Get("webhook-signature"), body, time.Now(), 0); err != nil {
			c.badSigs.Add(1)
		}
		c.arrived.Add(1)
		if c.hold.Load() {
			<-r.Context().Done() // the program is killed, or gives up, and the connection closes
			return
		}
		c.mu.Lock()
		c.seen[id]++
		c.mu.Unlock()
		w.WriteHeader(200)
	}))
	t.Cleanup(c.srv.Close)
	return c
}

func (c *customer) received() map[string]int {
	c.mu.Lock()
	defer c.mu.Unlock()
	out := map[string]int{}
	for k, v := range c.seen {
		out[k] = v
	}
	return out
}

func TestRefusesToStartWithoutAGoodKey(t *testing.T) {
	for name, key := range map[string]string{"no key": "", "short key": "short"} {
		cmd := exec.Command(binary, "serve", "-addr", freeAddr(t), "-db", filepath.Join(t.TempDir(), "x.db"))
		cmd.Env = []string{"PATH=" + os.Getenv("PATH"), "BOWLER_API_KEY=" + key}
		out, err := cmd.CombinedOutput()
		exit, ok := err.(*exec.ExitError)
		if !ok || exit.ExitCode() != 2 || !strings.Contains(string(out), "BOWLER_API_KEY") {
			t.Errorf("%s: it should exit with 2 and say why, got %v %q", name, err, out)
		}
	}
}

func TestUnknownCommandsAndVersion(t *testing.T) {
	if out, err := exec.Command(binary, "version").CombinedOutput(); err != nil || !strings.HasPrefix(string(out), "bowler ") {
		t.Errorf("version: %v %q", err, out)
	}
	if _, err := exec.Command(binary, "frobnicate").CombinedOutput(); err == nil {
		t.Error("an unknown command must fail")
	}
	if _, err := exec.Command(binary).CombinedOutput(); err == nil {
		t.Error("no command must fail")
	}
}

func TestServesDeliversAndStopsCleanly(t *testing.T) {
	p := start(t, filepath.Join(t.TempDir(), "bowler.db"), freeAddr(t))
	cust := newCustomer(t)

	if resp, _ := http.Get("http://" + p.addr + "/v1/apps"); resp.StatusCode != 401 {
		t.Fatalf("the API must need the key: %d", resp.StatusCode)
	}
	_, app := p.call(t, "POST", "/v1/apps", map[string]any{"name": "acme"})
	_, ep := p.call(t, "POST", "/v1/apps/"+s(app, "id")+"/endpoints", map[string]any{"url": cust.srv.URL})
	cust.secret.Store(s(ep, "secret"))
	_, msg := p.call(t, "POST", "/v1/apps/"+s(app, "id")+"/messages", map[string]any{"event_type": "invoice.paid", "payload": map[string]any{"n": 1}})

	waitFor(t, 10*time.Second, "the customer to receive the webhook", func() bool { return cust.received()[s(msg, "id")] == 1 })
	if cust.badSigs.Load() != 0 {
		t.Fatal("the customer could not verify the signature")
	}

	p.cmd.Process.Signal(syscall.SIGTERM)
	select {
	case <-p.exited:
	case <-time.After(10 * time.Second):
		t.Fatal("it must stop on SIGTERM")
	}
	if p.code != 0 {
		t.Fatalf("a clean shutdown exits 0, got %d\n%s", p.code, p.logs())
	}
	logs := p.logs()
	if !strings.Contains(logs, `"msg":"stopped"`) {
		t.Errorf("it should log that it stopped:\n%s", logs)
	}
	for _, secret := range []string{procKey, s(ep, "secret")} {
		if strings.Contains(logs, secret) {
			t.Fatalf("a secret must never reach the log:\n%s", logs)
		}
	}
	for _, line := range strings.Split(strings.TrimSpace(logs), "\n") {
		var rec map[string]any
		if json.Unmarshal([]byte(line), &rec) != nil {
			t.Errorf("every log line is JSON, got %q", line)
		}
	}
}

// The central promise: kill the service in the middle of delivering and nothing is lost.
func TestKillingTheProcessLosesNothing(t *testing.T) {
	if testing.Short() {
		t.Skip("takes about ten seconds")
	}
	const messages = 20
	db := filepath.Join(t.TempDir(), "bowler.db")
	addr := freeAddr(t)
	cust := newCustomer(t)
	cust.hold.Store(true) // the customer's server is stuck, so deliveries are in flight when we kill

	p := start(t, db, addr, "-timeout", "2s", "-lease", "7s", "-drain", "1s")
	_, app := p.call(t, "POST", "/v1/apps", map[string]any{"name": "acme"})
	_, ep := p.call(t, "POST", "/v1/apps/"+s(app, "id")+"/endpoints", map[string]any{"url": cust.srv.URL, "concurrency": 10})
	cust.secret.Store(s(ep, "secret"))

	want := map[string]bool{}
	for i := range messages {
		_, m := p.call(t, "POST", "/v1/apps/"+s(app, "id")+"/messages", map[string]any{"event_type": "x", "payload": map[string]any{"i": i}})
		want[s(m, "id")] = true
	}
	waitFor(t, 10*time.Second, "deliveries to be in flight", func() bool { return cust.arrived.Load() >= 5 })

	// No warning, no shutdown: the process is gone.
	if err := p.cmd.Process.Kill(); err != nil {
		t.Fatal(err)
	}
	<-p.exited
	time.Sleep(100 * time.Millisecond)

	cust.hold.Store(false) // the customer's server is fine again
	p2 := start(t, db, addr, "-timeout", "2s", "-lease", "7s", "-drain", "1s")

	waitFor(t, 40*time.Second, "every message to reach the customer after the restart", func() bool {
		got := cust.received()
		for id := range want {
			if got[id] == 0 {
				return false
			}
		}
		return true
	})

	got := cust.received()
	var repeats int
	for id := range want {
		if got[id] > 1 {
			repeats += got[id] - 1
		}
	}
	for id := range got {
		if !want[id] {
			t.Errorf("the customer received a message that was never sent: %s", id)
		}
	}
	t.Logf("all %d messages delivered after a kill -9, %d repeated (the customer drops those by webhook-id)", messages, repeats)
	if cust.badSigs.Load() != 0 {
		t.Fatalf("%d requests had an invalid signature", cust.badSigs.Load())
	}

	// And the history agrees with what the customer saw.
	waitFor(t, 10*time.Second, "every delivery to be recorded as succeeded", func() bool {
		_, r := p2.call(t, "GET", "/v1/apps/"+s(app, "id")+"/endpoints/"+s(ep, "id")+"/deliveries?state=succeeded&limit=250", nil)
		data, _ := r["data"].([]any)
		return len(data) == messages
	})
}

func TestSurvivesBeingKilledWhileIdleAndKeepsItsData(t *testing.T) {
	db := filepath.Join(t.TempDir(), "bowler.db")
	addr := freeAddr(t)
	p := start(t, db, addr)
	_, app := p.call(t, "POST", "/v1/apps", map[string]any{"name": "keep me", "uid": "acme-1"})
	p.cmd.Process.Kill()
	<-p.exited

	p2 := start(t, db, addr)
	code, got := p2.call(t, "GET", "/v1/apps/acme-1", nil)
	if code != 200 || s(got, "id") != s(app, "id") {
		t.Fatalf("data written before the kill must still be there: %d %v", code, got)
	}
}
