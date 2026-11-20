// SPDX-License-Identifier: Apache-2.0

// Command agentload drives agent-shaped traffic against a running wallet
// service: N simulated agents, each running multi-step tasks (read, think,
// write, verify) with log-normal think time standing in for LLM latency. No
// LLM is involved, so it costs nothing. The service only sees HTTP, so this
// loads it the way real agents would.
//
// It reports per-operation latency, task outcomes, stale-read blocks and
// retries, and checks an invariant at the end (balance conservation).
//
//	make run                                  # in one terminal
//	make agentload ARGS="-agents 20 -duration 15s"
package main

import (
	"bytes"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"math"
	"math/rand/v2"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"text/tabwriter"
	"time"
)

type config struct {
	url        string
	agents     int
	wallets    int
	duration   time.Duration
	ramp       time.Duration
	think      time.Duration
	sigma      float64
	api        string
	mix        string
	initial    int64
	seed       uint64
	writeToken string
	readToken  string
	maxRetries int
	baseline   bool
	save, vs   string
	aggregate  string
}

// ---- HTTP -----------------------------------------------------------------

type result struct {
	code int // 0 = transport error
	body []byte
	dur  time.Duration
}

func (r result) class() string {
	switch {
	case r.code == 0:
		return "neterr"
	case r.code/100 == 2:
		return "ok"
	case r.code == http.StatusConflict && bytes.Contains(r.body, []byte("stale read")):
		return "stale"
	case r.code == http.StatusConflict:
		return "conflict"
	case r.code == http.StatusForbidden:
		return "forbidden"
	}
	return "http_" + strconv.Itoa(r.code)
}

type client struct {
	base string
	hc   *http.Client
}

func (c *client) raw(method, path, token, session string, body any) result {
	var rd io.Reader
	if body != nil {
		b, _ := json.Marshal(body)
		rd = bytes.NewReader(b)
	}
	req, err := http.NewRequest(method, c.base+path, rd)
	if err != nil {
		return result{}
	}
	req.Header.Set("Authorization", "Bearer "+token)
	if session != "" {
		req.Header.Set("X-Session-ID", session)
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	start := time.Now()
	resp, err := c.hc.Do(req)
	if err != nil {
		return result{dur: time.Since(start)}
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return result{code: resp.StatusCode, body: b, dur: time.Since(start)}
}

// ---- stats ----------------------------------------------------------------

type flowStat struct {
	outcomes map[string]int
	dur      []time.Duration // task wall time including think
	active   []time.Duration // task wall time minus think
}

type stats struct {
	mu      sync.Mutex
	lat     map[string][]time.Duration
	cls     map[string]map[string]int
	flows   map[string]*flowStat
	retries int
	blind   int // retry_storm resends the guard refused as stale
	indet   int // writes whose outcome is unknown (transport error)
}

func newStats() *stats {
	return &stats{lat: map[string][]time.Duration{}, cls: map[string]map[string]int{}, flows: map[string]*flowStat{}}
}

func (s *stats) op(name string, r result) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.lat[name] = append(s.lat[name], r.dur)
	if s.cls[name] == nil {
		s.cls[name] = map[string]int{}
	}
	s.cls[name][r.class()]++
}

func (s *stats) flow(name, outcome string, d, think time.Duration) {
	s.mu.Lock()
	defer s.mu.Unlock()
	f := s.flows[name]
	if f == nil {
		f = &flowStat{outcomes: map[string]int{}}
		s.flows[name] = f
	}
	f.outcomes[outcome]++
	f.dur = append(f.dur, d)
	f.active = append(f.active, d-think)
}

func (s *stats) merge(o *stats) {
	for k, v := range o.lat {
		s.lat[k] = append(s.lat[k], v...)
	}
	for k, m := range o.cls {
		if s.cls[k] == nil {
			s.cls[k] = map[string]int{}
		}
		for c, n := range m {
			s.cls[k][c] += n
		}
	}
	for k, f := range o.flows {
		d := s.flows[k]
		if d == nil {
			d = &flowStat{outcomes: map[string]int{}}
			s.flows[k] = d
		}
		for c, n := range f.outcomes {
			d.outcomes[c] += n
		}
		d.dur = append(d.dur, f.dur...)
		d.active = append(d.active, f.active...)
	}
	s.retries += o.retries
	s.blind += o.blind
	s.indet += o.indet
}

func pct(d []time.Duration, p float64) time.Duration {
	if len(d) == 0 {
		return 0
	}
	sort.Slice(d, func(i, j int) bool { return d[i] < d[j] })
	i := int(math.Ceil(p*float64(len(d)))) - 1
	return d[max(i, 0)]
}

// ---- model ----------------------------------------------------------------

type wallet struct {
	id, customer string
	baseline     int64
	delta        atomic.Int64 // net of every credit/debit the service confirmed
}

type walletState struct {
	Balance   int64 `json:"balance_minor"`
	Available int64 `json:"available_minor"`
	Holds     []struct {
		ID     string `json:"hold_id"`
		Reason string `json:"reason"`
	} `json:"holds"`
}

type agent struct {
	cfg *config
	c   *client
	rng *rand.Rand
	st  *stats
}

// task is one agent run: one session, one wallet.
type task struct {
	a       *agent
	session string
	w       *wallet
	think   time.Duration
}

// pause sleeps a log-normal time around the configured median, standing in
// for the model deciding its next step.
func (t *task) pause() {
	m := t.a.cfg.think
	if m <= 0 {
		return
	}
	d := min(time.Duration(float64(m)*math.Exp(t.a.cfg.sigma*t.a.rng.NormFloat64())), 10*m)
	t.think += d
	time.Sleep(d)
}

func (t *task) call(op, method, path, token string, body any) result {
	r := t.a.c.raw(method, path, token, t.session, body)
	t.a.st.op(op, r)
	return r
}

func (t *task) tool(op, name, token string, args any) result {
	return t.call(op, "POST", "/tools/"+name, token, args)
}

func (t *task) getWallet() (walletState, result) {
	var r result
	if t.a.cfg.api == "tools" {
		r = t.tool("get_wallet", "get_wallet", t.a.cfg.writeToken, map[string]any{"id": t.w.id})
	} else {
		r = t.call("get_wallet", "GET", "/wallets/"+t.w.id, t.a.cfg.writeToken, nil)
	}
	var s walletState
	if r.class() == "ok" {
		_ = json.Unmarshal(r.body, &s)
	}
	return s, r
}

func (t *task) getCustomer() result {
	if t.a.cfg.api == "tools" {
		return t.tool("get_customer", "get_customer", t.a.cfg.writeToken, map[string]any{"id": t.w.customer})
	}
	return t.call("get_customer", "GET", "/customers/"+t.w.customer, t.a.cfg.writeToken, nil)
}

// getCustomerFields reads the customer asking for specific fields only.
func (t *task) getCustomerFields(token string, fields ...string) result {
	if t.a.cfg.api == "tools" {
		return t.tool("get_customer_fields", "get_customer", token, map[string]any{"id": t.w.customer, "fields": fields})
	}
	return t.call("get_customer_fields", "GET", "/customers/"+t.w.customer+"?fields="+strings.Join(fields, ","), token, nil)
}

// move credits (+) or debits (-) the wallet and tracks it in the ledger when
// the service confirms it.
func (t *task) move(op string, amt int64, token string) result {
	return t.moveAs(op, op, amt, token, nil)
}

// moveAs is move with a separate name for the latency stats and an optional
// ?fields= narrowing of the response.
func (t *task) moveAs(stat, op string, amt int64, token string, fields []string) result {
	args := map[string]any{"amount_minor": amt}
	var r result
	if t.a.cfg.api == "tools" {
		args["id"] = t.w.id
		if fields != nil {
			args["fields"] = fields
		}
		r = t.tool(stat, op, token, args)
	} else {
		path := "/wallets/" + t.w.id + "/" + op
		if fields != nil {
			path += "?fields=" + strings.Join(fields, ",")
		}
		r = t.call(stat, "POST", path, token, args)
	}
	if token == t.a.cfg.writeToken {
		switch r.class() {
		case "ok":
			if op == "credit" {
				t.w.delta.Add(amt)
			} else {
				t.w.delta.Add(-amt)
			}
		case "neterr":
			t.a.st.mu.Lock()
			t.a.st.indet++
			t.a.st.mu.Unlock()
		}
	}
	return r
}

// withRetry is what a well-behaved agent does on a stale-read block: read the
// entity again, think, and retry the write.
func (t *task) withRetry(do func() result) result {
	for i := 0; ; i++ {
		r := do()
		if r.class() != "stale" || i >= t.a.cfg.maxRetries {
			return r
		}
		t.a.st.mu.Lock()
		t.a.st.retries++
		t.a.st.mu.Unlock()
		t.getWallet()
		t.pause()
	}
}

func (t *task) amount() int64 { return int64(100 + t.a.rng.IntN(1900)) }

// outcomeOf maps a write result to a task outcome: a business conflict
// (insufficient funds) is a legitimate refusal, anything else is a failure.
func outcomeOf(r result) string {
	switch r.class() {
	case "ok":
		return "ok"
	case "conflict":
		return "rejected"
	}
	return "failed"
}

// ---- flows ----------------------------------------------------------------

// stormResends is how many times retry_storm resends a refused write without
// re-reading.
const stormResends = 3

type flow struct {
	name string
	run  func(*task) string
}

var flows = map[string]flow{
	"topup": {"topup", func(t *task) string {
		if _, r := t.getWallet(); r.class() != "ok" {
			return "failed"
		}
		t.pause()
		amt := t.amount()
		if out := outcomeOf(t.withRetry(func() result { return t.move("credit", amt, t.a.cfg.writeToken) })); out != "ok" {
			return out
		}
		t.pause()
		if _, r := t.getWallet(); r.class() != "ok" {
			return "failed"
		}
		return "ok"
	}},

	"spend": {"spend", func(t *task) string {
		s, r := t.getWallet()
		if r.class() != "ok" {
			return "failed"
		}
		amt := min(t.amount(), s.Available)
		if amt <= 0 {
			return "rejected"
		}
		t.pause()
		if out := outcomeOf(t.withRetry(func() result { return t.move("debit", amt, t.a.cfg.writeToken) })); out != "ok" {
			return out
		}
		t.pause()
		if _, r := t.getWallet(); r.class() != "ok" {
			return "failed"
		}
		return "ok"
	}},

	// hold: reserve funds, look again, release. Holds have no tool, so this
	// flow always uses the REST routes.
	"hold": {"hold", func(t *task) string {
		s, r := t.getWallet()
		if r.class() != "ok" {
			return "failed"
		}
		amt := min(t.amount(), s.Available)
		if amt <= 0 {
			return "rejected"
		}
		t.pause()
		reason := t.session
		cr := t.withRetry(func() result {
			return t.call("hold_create", "POST", "/wallets/"+t.w.id+"/holds", t.a.cfg.writeToken,
				map[string]any{"amount_minor": amt, "reason": reason})
		})
		if out := outcomeOf(cr); out != "ok" {
			return out
		}
		var after walletState
		_ = json.Unmarshal(cr.body, &after)
		holdID := ""
		for _, h := range after.Holds {
			if h.Reason == reason {
				holdID = h.ID
			}
		}
		if holdID == "" {
			return "failed"
		}
		t.pause()
		if _, r := t.getWallet(); r.class() != "ok" {
			return "failed"
		}
		t.pause()
		if t.call("hold_release", "POST", "/holds/"+holdID+"/release", t.a.cfg.writeToken, map[string]any{}).class() != "ok" {
			return "failed"
		}
		return "ok"
	}},

	// browse: an agent fetching customer and wallet in parallel (parallel tool
	// calls), then looking again. Read only.
	"browse": {"browse", func(t *task) string {
		var wg sync.WaitGroup
		var c, w result
		wg.Add(2)
		go func() { defer wg.Done(); c = t.getCustomer() }()
		go func() { defer wg.Done(); _, w = t.getWallet() }()
		wg.Wait()
		if c.class() != "ok" || w.class() != "ok" {
			return "failed"
		}
		t.pause()
		if _, r := t.getWallet(); r.class() != "ok" {
			return "failed"
		}
		return "ok"
	}},

	// retry_storm: an agent that does not re-read after a refusal. Another
	// session changes the wallet after this session read it, then this session
	// resends its write stormResends times without re-reading. Every resend
	// must be a 409 stale read and move no money; then the agent re-reads and
	// succeeds. Needs the guard (dropped from baseline runs); balance
	// conservation proves the refused resends changed nothing.
	"retry_storm": {"retry_storm", func(t *task) string {
		if _, r := t.getWallet(); r.class() != "ok" {
			return "failed"
		}
		t.pause()
		other := &task{a: t.a, w: t.w, session: t.session + "-other"}
		if other.move("credit", t.amount(), t.a.cfg.writeToken).class() != "ok" {
			return "failed"
		}
		t.pause()
		amt := t.amount()
		for range stormResends {
			if t.moveAs("credit_resend", "credit", amt, t.a.cfg.writeToken, nil).class() != "stale" {
				return "failed" // the guard let a write through on an unseen change
			}
			t.a.st.mu.Lock()
			t.a.st.blind++
			t.a.st.mu.Unlock()
		}
		if _, r := t.getWallet(); r.class() != "ok" {
			return "failed"
		}
		t.pause()
		return outcomeOf(t.withRetry(func() result { return t.move("credit", amt, t.a.cfg.writeToken) }))
	}},

	// rewrite: an agent asks for a field it may not have, is refused with a
	// 403, and rewrites the request narrower. Needs the service to run with
	// STRICT_FIELDS=on. Two halves: a read-only token asks a customer for
	// "email" (PII) and must not get it; a write token credits a wallet asking
	// for a field that does not exist, which must be refused before anything is
	// saved (balance conservation catches a write that was saved and then 403'd).
	"rewrite": {"rewrite", func(t *task) string {
		rd := t.a.cfg.readToken
		if t.getCustomerFields(rd, "customer_id", "email").class() != "forbidden" {
			return "failed" // not refused: wrong service config or a leak
		}
		t.pause()
		r := t.getCustomerFields(rd, "customer_id", "phone")
		var got map[string]any
		if r.class() != "ok" || json.Unmarshal(r.body, &got) != nil {
			return "failed"
		}
		if _, leaked := got["email"]; leaked || got["phone"] != "***" {
			return "failed"
		}
		t.pause()

		wr := t.a.cfg.writeToken
		amt := t.amount()
		if t.moveAs("credit_badfield", "credit", amt, wr, []string{"balanc_minor"}).class() != "forbidden" {
			return "failed"
		}
		t.pause()
		return outcomeOf(t.withRetry(func() result {
			return t.moveAs("credit", "credit", amt, wr, []string{"balance_minor"})
		}))
	}},

	// denied: a read-only token attempts a write. Must be refused with 403.
	// Over the tools API the refusal is the lib's Config.Authorize hook; over
	// REST it is the service's own role check.
	"denied": {"denied", func(t *task) string {
		r := t.move("credit", 100, t.a.cfg.readToken)
		if r.class() != "forbidden" {
			return "failed"
		}
		return "ok"
	}},
}

type weighted struct {
	f flow
	w int
}

func parseMix(s string) ([]weighted, int, error) {
	var out []weighted
	total := 0
	for _, part := range strings.Split(s, ",") {
		name, ws, ok := strings.Cut(strings.TrimSpace(part), "=")
		f, known := flows[name]
		w, err := strconv.Atoi(ws)
		if !ok || !known || err != nil || w < 0 {
			return nil, 0, fmt.Errorf("bad mix entry %q (want name=weight, names: topup spend hold browse denied retry_storm rewrite)", part)
		}
		out = append(out, weighted{f, w})
		total += w
	}
	if total == 0 {
		return nil, 0, fmt.Errorf("mix has no weight")
	}
	return out, total, nil
}

// ---- run ------------------------------------------------------------------

func setup(c *client, cfg *config, runID string) ([]*wallet, error) {
	n := cfg.wallets
	ws := make([]*wallet, n)
	errs := make(chan error, n)
	sem := make(chan struct{}, 8)
	var wg sync.WaitGroup
	for i := range n {
		wg.Add(1)
		sem <- struct{}{}
		go func() {
			defer wg.Done()
			defer func() { <-sem }()
			cid := fmt.Sprintf("%s-c%d", runID, i)
			r := c.raw("POST", "/customers", cfg.writeToken, "", map[string]any{
				"customer_id": cid, "email": cid + "@load.test", "phone": "+40700000000",
			})
			var cu struct {
				Wallet struct {
					ID string `json:"wallet_id"`
				} `json:"wallet"`
			}
			if r.class() != "ok" || json.Unmarshal(r.body, &cu) != nil || cu.Wallet.ID == "" {
				errs <- fmt.Errorf("create customer %s: %d %s", cid, r.code, r.body)
				return
			}
			r = c.raw("POST", "/wallets/"+cu.Wallet.ID+"/credit", cfg.writeToken, "", map[string]any{"amount_minor": cfg.initial})
			var s walletState
			if r.class() != "ok" || json.Unmarshal(r.body, &s) != nil {
				errs <- fmt.Errorf("fund wallet %s: %d %s", cu.Wallet.ID, r.code, r.body)
				return
			}
			ws[i] = &wallet{id: cu.Wallet.ID, customer: cid, baseline: s.Balance}
		}()
	}
	wg.Wait()
	close(errs)
	if err := <-errs; err != nil {
		return nil, err
	}
	return ws, nil
}

func runAgent(i int, cfg *config, c *client, pool []*wallet, mix []weighted, total int, deadline time.Time, st *stats) {
	a := &agent{cfg: cfg, c: c, rng: rand.New(rand.NewPCG(cfg.seed, uint64(i)+1)), st: st}
	if cfg.ramp > 0 {
		time.Sleep(cfg.ramp * time.Duration(i) / time.Duration(cfg.agents))
	}
	runTag := fmt.Sprintf("a%d", i)
	for n := 0; time.Now().Before(deadline); n++ {
		w := pool[i%len(pool)]
		if cfg.wallets != cfg.agents { // shared pool: each task picks a wallet
			w = pool[a.rng.IntN(len(pool))]
		}
		pick := a.rng.IntN(total)
		var f flow
		for _, m := range mix {
			if pick -= m.w; pick < 0 {
				f = m.f
				break
			}
		}
		t := &task{a: a, w: w, session: fmt.Sprintf("%s-%s-t%d-%s", cfg.runID(), runTag, n, f.name)}
		start := time.Now()
		out := f.run(t)
		st.flow(f.name, out, time.Since(start), t.think)
	}
}

var runStamp = strconv.FormatInt(time.Now().Unix(), 36)

func (c *config) runID() string { return "load" + runStamp }

func verify(c *client, cfg *config, ws []*wallet, st *stats) (failed bool) {
	fmt.Println("\ninvariants")
	bad := 0
	for _, w := range ws {
		r := c.raw("GET", "/wallets/"+w.id, cfg.writeToken, "", nil)
		var s walletState
		if r.class() != "ok" || json.Unmarshal(r.body, &s) != nil {
			fmt.Printf("  %s: cannot read (%d)\n", w.id, r.code)
			bad++
			continue
		}
		if want := w.baseline + w.delta.Load(); s.Balance != want || s.Balance < 0 || s.Available > s.Balance {
			fmt.Printf("  %s: balance %d, expected %d (baseline %d + confirmed %d), available %d\n",
				w.id, s.Balance, want, w.baseline, w.delta.Load(), s.Available)
			bad++
		}
	}
	switch {
	case bad == 0:
		fmt.Printf("  balance conservation: PASS (%d wallets: balance == baseline + confirmed credits - confirmed debits)\n", len(ws))
	case st.indet > 0:
		fmt.Printf("  balance conservation: WARN (%d of %d wallets differ; %d writes had unknown outcome after transport errors)\n", bad, len(ws), st.indet)
	default:
		fmt.Printf("  balance conservation: FAIL (%d of %d wallets differ)\n", bad, len(ws))
		failed = true
	}
	return failed
}

func report(st *stats, cfg *config, wall time.Duration, nWallets int) {
	var reqs, tasks int
	for _, v := range st.lat {
		reqs += len(v)
	}
	stale := 0
	for _, m := range st.cls {
		stale += m["stale"]
	}
	for _, f := range st.flows {
		tasks += len(f.dur)
	}
	fmt.Printf("agents %d, wallets %d, api %s, think median %s (sigma %.2f), ran %s\n",
		cfg.agents, nWallets, cfg.api, cfg.think, cfg.sigma, wall.Round(time.Millisecond))
	fmt.Printf("%d requests (%.0f/s), %d tasks (%.1f/s), %d stale-read blocks, %d retries\n",
		reqs, float64(reqs)/wall.Seconds(), tasks, float64(tasks)/wall.Seconds(), stale, st.retries)

	fmt.Println("\nrequests by operation (ms)")
	tw := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
	fmt.Fprintln(tw, "op\tn\tok\tstale\tconflict\tforbidden\tother\tp50\tp95\tp99\tmax")
	for _, op := range sortedKeys(st.lat) {
		l, m := st.lat[op], st.cls[op]
		other := len(l) - m["ok"] - m["stale"] - m["conflict"] - m["forbidden"]
		fmt.Fprintf(tw, "%s\t%d\t%d\t%d\t%d\t%d\t%d\t%s\t%s\t%s\t%s\n", op, len(l), m["ok"], m["stale"], m["conflict"], m["forbidden"], other,
			ms(pct(l, .50)), ms(pct(l, .95)), ms(pct(l, .99)), ms(pct(l, 1)))
	}
	tw.Flush()

	fmt.Println("\ntasks by flow (ms; active = wall time minus think time)")
	tw = tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
	fmt.Fprintln(tw, "flow\tn\tok\trejected\tfailed\tp50\tp95\tactive p50\tactive p95")
	for _, name := range sortedKeys(st.flows) {
		f := st.flows[name]
		fmt.Fprintf(tw, "%s\t%d\t%d\t%d\t%d\t%s\t%s\t%s\t%s\n", name, len(f.dur), f.outcomes["ok"], f.outcomes["rejected"], f.outcomes["failed"],
			ms(pct(f.dur, .50)), ms(pct(f.dur, .95)), ms(pct(f.active, .50)), ms(pct(f.active, .95)))
	}
	tw.Flush()
}

func ms(d time.Duration) string { return fmt.Sprintf("%.2f", float64(d)/float64(time.Millisecond)) }

func sortedKeys[V any](m map[string]V) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

func main() {
	cfg := &config{}
	flag.StringVar(&cfg.url, "url", "http://localhost:8080", "service base URL")
	flag.IntVar(&cfg.agents, "agents", 10, "concurrent simulated agents")
	flag.IntVar(&cfg.wallets, "wallets", 0, "wallet pool size; 0 = one private wallet per agent, fewer than agents = shared wallets (contention)")
	flag.DurationVar(&cfg.duration, "duration", 10*time.Second, "how long agents keep starting tasks (the last task finishes after)")
	flag.DurationVar(&cfg.ramp, "ramp", 0, "spread agent start times over this period")
	flag.DurationVar(&cfg.think, "think", 200*time.Millisecond, "median think time between steps (stand-in for LLM latency); 0 disables")
	flag.Float64Var(&cfg.sigma, "sigma", 0.6, "log-normal spread of think time")
	flag.StringVar(&cfg.api, "api", "tools", "tools = agent tool endpoints, rest = REST routes")
	flag.StringVar(&cfg.mix, "mix", "topup=4,spend=3,hold=1,browse=4,denied=1", "flow weights")
	flag.Int64Var(&cfg.initial, "initial", 100000, "starting balance (minor units) per wallet")
	flag.Uint64Var(&cfg.seed, "seed", 1, "random seed (same seed, same choices)")
	flag.StringVar(&cfg.writeToken, "write-token", "dev-write", "token with write role")
	flag.StringVar(&cfg.readToken, "read-token", "dev-read", "token with read role (used for denied calls)")
	flag.IntVar(&cfg.maxRetries, "max-retries", 3, "retries after a stale-read block before a task fails")
	flag.BoolVar(&cfg.baseline, "baseline", false, "target runs with GUARD=off: REST only, no denied flow")
	flag.StringVar(&cfg.save, "save", "", "write per-operation latency summary to this JSON file")
	flag.StringVar(&cfg.vs, "vs", "", "print latency delta against a summary saved with -save (usually the baseline run)")
	flag.StringVar(&cfg.aggregate, "aggregate", "", "dir with base-N.json and guarded-N.json from repeated runs; prints median and range of the paired deltas, then exits")
	flag.Parse()

	if cfg.aggregate != "" {
		if err := aggregate(cfg.aggregate); err != nil {
			fmt.Fprintln(os.Stderr, "aggregate:", err)
			os.Exit(1)
		}
		return
	}

	if cfg.agents < 1 || (cfg.api != "tools" && cfg.api != "rest") {
		fmt.Fprintln(os.Stderr, "need -agents >= 1 and -api tools|rest")
		os.Exit(2)
	}
	if cfg.wallets <= 0 {
		cfg.wallets = cfg.agents
	}
	if cfg.baseline {
		cfg.api = "rest"
		for _, f := range []string{"denied", "retry_storm", "rewrite"} {
			cfg.mix = dropFlow(cfg.mix, f)
		}
	}
	mix, total, err := parseMix(cfg.mix)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(2)
	}

	c := &client{base: strings.TrimRight(cfg.url, "/"), hc: &http.Client{
		Timeout:   30 * time.Second,
		Transport: &http.Transport{MaxIdleConnsPerHost: cfg.agents*2 + 8},
	}}
	if r := c.raw("GET", "/health", cfg.writeToken, "", nil); r.class() != "ok" {
		fmt.Fprintf(os.Stderr, "service not healthy at %s (status %d); start it with `make run`\n", cfg.url, r.code)
		os.Exit(1)
	}
	pool, err := setup(c, cfg, cfg.runID())
	if err != nil {
		fmt.Fprintln(os.Stderr, "setup:", err)
		os.Exit(1)
	}

	start := time.Now()
	deadline := start.Add(cfg.duration)
	perAgent := make([]*stats, cfg.agents)
	var wg sync.WaitGroup
	for i := range cfg.agents {
		perAgent[i] = newStats()
		wg.Add(1)
		go func() {
			defer wg.Done()
			runAgent(i, cfg, c, pool, mix, total, deadline, perAgent[i])
		}()
	}
	wg.Wait()
	wall := time.Since(start)

	all := newStats()
	for _, s := range perAgent {
		all.merge(s)
	}
	report(all, cfg, wall, len(pool))
	if cfg.save != "" {
		if err := saveSummary(cfg.save, all); err != nil {
			fmt.Fprintln(os.Stderr, "save:", err)
			os.Exit(1)
		}
	}
	if cfg.vs != "" {
		if err := compare(cfg.vs, all); err != nil {
			fmt.Fprintln(os.Stderr, "vs:", err)
			os.Exit(1)
		}
	}
	if verify(c, cfg, pool, all) {
		os.Exit(1)
	}
}

func dropFlow(mix, name string) string {
	var keep []string
	for _, part := range strings.Split(mix, ",") {
		if n, _, _ := strings.Cut(strings.TrimSpace(part), "="); n != name {
			keep = append(keep, part)
		}
	}
	return strings.Join(keep, ",")
}

// opSummary is what -save writes and -vs reads, in milliseconds.
type opSummary struct {
	N   int     `json:"n"`
	P50 float64 `json:"p50"`
	P95 float64 `json:"p95"`
	P99 float64 `json:"p99"`
}

func toMS(d time.Duration) float64 { return float64(d) / float64(time.Millisecond) }

func summarize(st *stats) map[string]opSummary {
	out := map[string]opSummary{}
	for op, l := range st.lat {
		out[op] = opSummary{len(l), toMS(pct(l, .50)), toMS(pct(l, .95)), toMS(pct(l, .99))}
	}
	return out
}

func saveSummary(path string, st *stats) error {
	b, err := json.MarshalIndent(summarize(st), "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(path, b, 0o644)
}

// compare prints this run minus a saved run, per operation. Delta is only
// meaningful when both runs used the same flags and seed, on a fresh DB each.
func compare(path string, st *stats) error {
	b, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	var base map[string]opSummary
	if err := json.Unmarshal(b, &base); err != nil {
		return err
	}
	cur := summarize(st)
	fmt.Printf("\ndelta vs %s (this run minus saved, ms; saved run is the baseline)\n", path)
	tw := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
	fmt.Fprintln(tw, "op\tn\tp50 base\tp50 +\tp95 base\tp95 +\tp99 base\tp99 +")
	for _, op := range sortedKeys(cur) {
		b, ok := base[op]
		if !ok {
			continue
		}
		c := cur[op]
		fmt.Fprintf(tw, "%s\t%d/%d\t%.2f\t%+.2f\t%.2f\t%+.2f\t%.2f\t%+.2f\n", op, c.N, b.N,
			b.P50, c.P50-b.P50, b.P95, c.P95-b.P95, b.P99, c.P99-b.P99)
	}
	return tw.Flush()
}

func loadSummary(path string) (map[string]opSummary, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var m map[string]opSummary
	return m, json.Unmarshal(b, &m)
}

func median(v []float64) float64 {
	sort.Float64s(v)
	if n := len(v); n%2 == 0 {
		return (v[n/2-1] + v[n/2]) / 2
	}
	return v[len(v)/2]
}

// aggregate pairs base-N.json with guarded-N.json (same repetition) and
// prints, per operation and percentile, the baseline median and the median
// and range of the guarded-minus-baseline delta across repetitions.
func aggregate(dir string) error {
	files, _ := filepath.Glob(filepath.Join(dir, "base-*.json"))
	sort.Strings(files)
	var bases, guards []map[string]opSummary
	for _, bf := range files {
		gf := filepath.Join(dir, "guarded-"+strings.TrimPrefix(filepath.Base(bf), "base-"))
		b, err := loadSummary(bf)
		if err != nil {
			return err
		}
		g, err := loadSummary(gf)
		if err != nil {
			return err
		}
		bases, guards = append(bases, b), append(guards, g)
	}
	if len(bases) == 0 {
		return fmt.Errorf("no base-N.json in %s", dir)
	}
	ops := map[string]opSummary{}
	for op := range bases[0] {
		ops[op] = opSummary{}
	}
	fmt.Printf("guarded minus baseline over %d paired runs (ms): base median, delta median [min, max]\n", len(bases))
	tw := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
	fmt.Fprintln(tw, "op\tp50 base\tp50 delta\tp95 base\tp95 delta\tp99 base\tp99 delta")
	for _, op := range sortedKeys(ops) {
		cols := []string{op}
		for _, pick := range []func(opSummary) float64{
			func(o opSummary) float64 { return o.P50 },
			func(o opSummary) float64 { return o.P95 },
			func(o opSummary) float64 { return o.P99 },
		} {
			var base, delta []float64
			for i := range bases {
				b, bok := bases[i][op]
				g, gok := guards[i][op]
				if bok && gok {
					base, delta = append(base, pick(b)), append(delta, pick(g)-pick(b))
				}
			}
			if len(delta) == 0 {
				cols = append(cols, "-", "-")
				continue
			}
			bm, dm := median(base), median(delta) // median sorts delta in place
			cols = append(cols, fmt.Sprintf("%.3f", bm), fmt.Sprintf("%+.3f [%+.3f, %+.3f]", dm, delta[0], delta[len(delta)-1]))
		}
		fmt.Fprintln(tw, strings.Join(cols, "\t"))
	}
	return tw.Flush()
}
