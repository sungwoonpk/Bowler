package main

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/sungwoonpk/bowler/internal/api"
	"github.com/sungwoonpk/bowler/internal/deliver"
	"github.com/sungwoonpk/bowler/internal/dispatcher"
	"github.com/sungwoonpk/bowler/internal/ssrf"
	"github.com/sungwoonpk/bowler/internal/store"
	"github.com/sungwoonpk/bowler/retry"
	"github.com/sungwoonpk/bowler/signature"
)

// receiverKind is how a demo receiver misbehaves.
type receiverKind struct {
	name   string
	note   string
	handle func(r *demoReceiver, id string) (status int, delay time.Duration)
}

// demoReceiver stands in for a customer's server. It checks the signature the way a real customer's server would.
type demoReceiver struct {
	kind     receiverKind
	srv      *http.Server
	url      string
	started  time.Time
	secret   atomic.Value // string
	mu       sync.Mutex
	tries    map[string]int // attempts seen per message
	accepted int
	verified int
	invalid  int
	fixed    atomic.Bool // set when the demo "repairs" the receiver
}

func (r *demoReceiver) serve(w http.ResponseWriter, req *http.Request) {
	body, _ := io.ReadAll(req.Body)
	id := req.Header.Get("webhook-id")
	secret, _ := r.secret.Load().(string)
	err := signature.Verify([]string{secret}, id, req.Header.Get("webhook-timestamp"), req.Header.Get("webhook-signature"), body, time.Now(), 0)

	r.mu.Lock()
	r.tries[id]++
	if err != nil {
		r.invalid++
	} else {
		r.verified++
	}
	r.mu.Unlock()

	status, delay := r.kind.handle(r, id)
	if r.fixed.Load() {
		status, delay = 200, 0
	}
	if delay > 0 {
		select {
		case <-time.After(delay):
		case <-req.Context().Done():
			return
		}
	}
	if status/100 == 2 {
		r.mu.Lock()
		r.accepted++
		r.mu.Unlock()
	}
	w.WriteHeader(status)
}

func (r *demoReceiver) triesFor(id string) int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.tries[id]
}

func startReceiver(kind receiverKind) (*demoReceiver, error) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return nil, err
	}
	r := &demoReceiver{kind: kind, started: time.Now(), tries: map[string]int{}, url: "http://" + ln.Addr().String() + "/hook"}
	r.srv = &http.Server{Handler: http.HandlerFunc(r.serve), ReadHeaderTimeout: 5 * time.Second}
	go func() { _ = r.srv.Serve(ln) }()
	return r, nil
}

var receiverKinds = []receiverKind{
	{"healthy", "always answers 200", func(*demoReceiver, string) (int, time.Duration) { return 200, 0 }},
	{"flaky", "fails the first two tries of every message", func(r *demoReceiver, id string) (int, time.Duration) {
		if r.triesFor(id) <= 2 {
			return 500, 0
		}
		return 200, 0
	}},
	{"outage", "answers 503 for the first 5 seconds", func(r *demoReceiver, _ string) (int, time.Duration) {
		if time.Since(r.started) < 5*time.Second {
			return 503, 0
		}
		return 200, 0
	}},
	{"slow", "hangs, so every try times out", func(*demoReceiver, string) (int, time.Duration) { return 200, 30 * time.Second }},
	{"gone", "answers 410 Gone, a permanent no", func(*demoReceiver, string) (int, time.Duration) { return 410, 0 }},
}

type demoClient struct {
	base, key string
}

func (c demoClient) call(method, path string, body any) (map[string]any, int, error) {
	var rd io.Reader
	if body != nil {
		b, _ := json.Marshal(body)
		rd = bytes.NewReader(b)
	}
	req, _ := http.NewRequest(method, c.base+path, rd)
	req.Header.Set("Authorization", "Bearer "+c.key)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, 0, err
	}
	defer resp.Body.Close()
	var out map[string]any
	_ = json.NewDecoder(resp.Body).Decode(&out)
	return out, resp.StatusCode, nil
}

func str(m map[string]any, k string) string { s, _ := m[k].(string); return s }

// runDemo starts the service and a set of receivers that fail in different ways, sends messages, and shows what
// happens. It is the quickest way to see what Bowler is for.
func runDemo(ctx context.Context, args []string, out io.Writer) error {
	fs := flag.NewFlagSet("demo", flag.ContinueOnError)
	once := fs.Bool("once", false, "run the scenario and exit, instead of leaving the service running")
	messages := fs.Int("messages", 10, "how many messages to send")
	if err := fs.Parse(args); err != nil {
		return err
	}
	say := func(format string, a ...any) { fmt.Fprintf(out, format+"\n", a...) }

	dir, err := os.MkdirTemp("", "bowler-demo-")
	if err != nil {
		return err
	}
	defer func() { _ = os.RemoveAll(dir) }()

	db, err := store.Open(filepath.Join(dir, "demo.db"))
	if err != nil {
		return err
	}
	defer func() { _ = db.Close() }()

	policy := ssrf.Policy{AllowPrivate: true, AllowHTTP: true} // the receivers are on this machine
	sender := deliver.New(deliver.Config{SSRF: policy, Timeout: 2 * time.Second, UserAgent: "Bowler/" + version})
	defer sender.Close()
	// A short schedule so retries show up within seconds. The real one stretches over about a day.
	fast := retry.Policy{Schedule: []time.Duration{0, time.Second, 2 * time.Second, 3 * time.Second}, Jitter: 0.1}
	disp, err := dispatcher.New(db, sender, dispatcher.Config{Retry: fast, AttemptTimeout: 2 * time.Second, LeaseFor: 30 * time.Second,
		PollInterval: 100 * time.Millisecond, DrainTimeout: 2 * time.Second})
	if err != nil {
		return err
	}
	var rawKey [16]byte
	rand.Read(rawKey[:])
	apiKey := "demo-" + hex.EncodeToString(rawKey[:])
	srv, err := api.New(api.Config{Store: db, APIKey: apiKey, SSRF: policy, Notify: disp.Notify, Stats: disp.Stats, Version: version})
	if err != nil {
		return err
	}
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return err
	}
	httpServer := &http.Server{Handler: srv.Handler(), ReadHeaderTimeout: 5 * time.Second}
	go func() { _ = httpServer.Serve(ln) }()
	defer func() { _ = httpServer.Shutdown(context.Background()) }()
	runCtx, stopRun := context.WithCancel(ctx)
	runDone := make(chan struct{})
	go func() { _ = disp.Run(runCtx); close(runDone) }()
	defer func() { stopRun(); <-runDone }()

	client := demoClient{base: "http://" + ln.Addr().String(), key: apiKey}
	say("Bowler demo")
	say("  service   %s", client.base)
	say("  retries   immediately, then after 1s, 2s and 3s (a real deployment keeps trying for about a day)\n")

	app, _, err := client.call("POST", "/v1/apps", map[string]any{"name": "Acme Billing"})
	if err != nil {
		return err
	}
	appPath := "/v1/apps/" + str(app, "id")

	receivers := make([]*demoReceiver, len(receiverKinds))
	endpoints := make([]string, len(receiverKinds))
	say("Five customers, each with a server that behaves differently:")
	for i, kind := range receiverKinds {
		r, err := startReceiver(kind)
		if err != nil {
			return err
		}
		defer func() { _ = r.srv.Close() }()
		ep, code, err := client.call("POST", appPath+"/endpoints", map[string]any{"url": r.url, "description": kind.name, "concurrency": 10})
		if err != nil || code != http.StatusCreated {
			return fmt.Errorf("could not create the %s endpoint: %v %v", kind.name, code, err)
		}
		r.secret.Store(str(ep, "secret"))
		receivers[i], endpoints[i] = r, str(ep, "id")
		say("  %-8s %s", kind.name, kind.note)
	}

	say("\nSending %d messages. Each goes to all five customers.\n", *messages)
	started := time.Now()
	var msgIDs []string
	for i := range *messages {
		m, code, err := client.call("POST", appPath+"/messages", map[string]any{"event_type": "invoice.paid", "payload": map[string]any{"invoice": fmt.Sprintf("in_%03d", i+1), "amount": 1000 + i*50}})
		if err != nil || code != http.StatusAccepted {
			return fmt.Errorf("could not send message %d: %v %v", i+1, code, err)
		}
		msgIDs = append(msgIDs, str(m, "id"))
	}

	type tally struct{ pending, inflight, succeeded, failed int }
	read := func() ([]tally, int) {
		t := make([]tally, len(endpoints))
		open := 0
		for i, ep := range endpoints {
			for _, st := range []string{"pending", "inflight", "succeeded", "failed"} {
				r, _, _ := client.call("GET", appPath+"/endpoints/"+ep+"/deliveries?state="+st+"&limit=250", nil)
				n := 0
				if data, ok := r["data"].([]any); ok {
					n = len(data)
				}
				switch st {
				case "pending":
					t[i].pending = n
				case "inflight":
					t[i].inflight = n
				case "succeeded":
					t[i].succeeded = n
				case "failed":
					t[i].failed = n
				}
			}
			open += t[i].pending + t[i].inflight
		}
		return t, open
	}

	header := fmt.Sprintf("  %5s", "time")
	for _, k := range receiverKinds {
		header += fmt.Sprintf("  %-18s", k.name)
	}
	say("%s\n  %s", header, strings.Repeat("-", len(header)-2))
	doneAt := make([]time.Duration, len(endpoints))
	lastBody := ""
	deadline := time.After(40 * time.Second)
	tick := time.NewTicker(time.Second)
	defer tick.Stop()
settle:
	for {
		select {
		case <-ctx.Done():
			return nil
		case <-deadline:
			break settle
		case <-tick.C:
		}
		t, open := read()
		line := fmt.Sprintf("  %4.0fs", time.Since(started).Seconds())
		for i, x := range t {
			cell := fmt.Sprintf("%d ok", x.succeeded)
			if x.pending+x.inflight > 0 {
				cell += fmt.Sprintf(", %d wait", x.pending+x.inflight)
			}
			if x.failed > 0 {
				cell += fmt.Sprintf(", %d fail", x.failed)
			}
			if doneAt[i] == 0 && x.pending+x.inflight == 0 {
				doneAt[i] = time.Since(started)
			}
			line += fmt.Sprintf("  %-18s", cell)
		}
		if body := line[6:]; body != lastBody {
			say("%s", line)
			lastBody = body
		}
		if open == 0 {
			break
		}
	}

	say("\nWhat happened:")
	t, _ := read()
	for i, k := range receiverKinds {
		r := receivers[i]
		r.mu.Lock()
		verified, invalid := r.verified, r.invalid
		r.mu.Unlock()
		finished := "still trying"
		if doneAt[i] > 0 {
			finished = fmt.Sprintf("settled after %.0fs", doneAt[i].Seconds())
		}
		say("  %-8s delivered %2d of %d, gave up on %d, %s. Its server verified %d signatures and rejected %d.",
			k.name, t[i].succeeded, *messages, t[i].failed, finished, verified, invalid)
	}
	say("\n  The healthy customer was done in %.0fs even though others were failing, timing out or gone.", doneAt[0].Seconds())

	ep, _, _ := client.call("GET", appPath+"/endpoints/"+endpoints[4], nil)
	if ep["disabled_at"] != nil {
		say("  The gone customer's endpoint was switched off after the 410: %q", str(ep, "disabled_reason"))
		receivers[4].fixed.Store(true)
		say("\nThe gone customer says it was a mistake and fixes its server. Bowler replays what it could not deliver:")
		rec, _, _ := client.call("POST", appPath+"/endpoints/"+endpoints[4]+"/recover", map[string]any{"since": started.Add(-time.Minute).Format(time.RFC3339)})
		say("  recover -> %v deliveries replayed", rec["replayed"])
		for range 20 {
			time.Sleep(500 * time.Millisecond)
			t, _ := read()
			if t[4].succeeded == *messages {
				break
			}
		}
		t, _ = read()
		say("  the gone customer has now received %d of %d", t[4].succeeded, *messages)
	}

	if len(msgIDs) > 0 {
		ds, _, _ := client.call("GET", appPath+"/messages/"+msgIDs[0]+"/deliveries", nil)
		if data, ok := ds["data"].([]any); ok {
			names := map[string]string{}
			for i, id := range endpoints {
				names[id] = receiverKinds[i].name
			}
			say("\nThe history of the first message, as a customer would see it in the API:")
			var rows []string
			for _, d := range data {
				dm := d.(map[string]any)
				atts, _, _ := client.call("GET", appPath+"/deliveries/"+str(dm, "id")+"/attempts", nil)
				var parts []string
				if list, ok := atts["data"].([]any); ok {
					for _, a := range list {
						am := a.(map[string]any)
						code := "-"
						if sc, ok := am["status_code"].(float64); ok {
							code = fmt.Sprintf("%d", int(sc))
						}
						if f := str(am, "failure"); f != "" {
							code += " (" + f + ")"
						}
						parts = append(parts, code)
					}
				}
				rows = append(rows, fmt.Sprintf("  %-8s %-10s attempts: %s", names[str(dm, "endpoint_id")], str(dm, "state"), strings.Join(parts, " -> ")))
			}
			sort.Strings(rows)
			for _, r := range rows {
				say("%s", r)
			}
		}
	}

	if *once {
		return nil
	}
	say("\nThe service is still running. Try it:")
	say("  curl -s -H 'Authorization: Bearer %s' %s%s/messages | head -c 400", apiKey, client.base, appPath)
	say("  curl -s -H 'Authorization: Bearer %s' %s/metrics", apiKey, client.base)
	say("Press Ctrl+C to stop.")
	<-ctx.Done()
	return nil
}
