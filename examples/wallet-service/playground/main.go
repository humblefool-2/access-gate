// SPDX-License-Identifier: Apache-2.0

// Command playground is a local web UI for trying the access-gate library: run
// requests as different roles against the service with the guard (lib) and
// without it (GUARD=off baseline), compare latency, and start the benchmarks.
//
//	make playground     # then open http://localhost:9090
//
// It builds and starts two child services (guarded and baseline) on their own
// databases, and serves one embedded page plus a small JSON API. Standard
// library only. Local use only: it forwards arbitrary requests to its own
// children and runs the bench tools, so do not expose it.
package main

import (
	"bytes"
	"context"
	_ "embed"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"sort"
	"strconv"
	"sync"
	"syscall"
	"time"
)

//go:embed ui.html
var uiHTML []byte

const (
	guardedPort  = "18182"
	baselinePort = "18183"
)

type child struct {
	name string
	port string
	db   string
	cmd  *exec.Cmd
}

type pg struct {
	root   string // repo root
	bin    string // dir with built binaries
	urls   map[string]string
	client *http.Client

	mu       sync.Mutex
	children map[string]*child
	cancel   context.CancelFunc // running bench, if any
}

func newPG(urls map[string]string) *pg {
	return &pg{urls: urls, client: &http.Client{Timeout: 30 * time.Second}, children: map[string]*child{}}
}

// ---- children -------------------------------------------------------------

func (p *pg) startChild(c *child, guard string) error {
	cmd := exec.Command(filepath.Join(p.bin, "service"))
	cmd.Env = append(os.Environ(), "PORT="+c.port, "DB_PATH="+c.db, "GUARD="+guard)
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	logf, err := os.Create(c.db + ".log")
	if err != nil {
		return err
	}
	cmd.Stdout, cmd.Stderr = logf, logf
	if err := cmd.Start(); err != nil {
		return err
	}
	c.cmd = cmd
	go func() { _ = cmd.Wait(); logf.Close() }()
	for range 100 {
		resp, err := p.client.Get(p.urls[c.name] + "/health")
		if err == nil {
			resp.Body.Close()
			if resp.StatusCode == http.StatusOK {
				return nil
			}
		}
		time.Sleep(100 * time.Millisecond)
	}
	return fmt.Errorf("%s service did not become healthy (see %s.log)", c.name, c.db)
}

func (c *child) stop() {
	if c.cmd != nil && c.cmd.Process != nil {
		_ = syscall.Kill(-c.cmd.Process.Pid, syscall.SIGTERM)
		time.Sleep(200 * time.Millisecond)
		c.cmd = nil
	}
}

// startAll (re)starts both children on fresh databases.
func (p *pg) startAll() error {
	p.mu.Lock()
	defer p.mu.Unlock()
	dir := filepath.Join(p.root, "data", "playground")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	for _, spec := range []struct{ name, port, guard string }{
		{"guarded", guardedPort, "on"},
		{"baseline", baselinePort, "off"},
	} {
		c := p.children[spec.name]
		if c == nil {
			c = &child{name: spec.name, port: spec.port, db: filepath.Join(dir, spec.name+".db")}
			p.children[spec.name] = c
		}
		c.stop()
		for _, suf := range []string{"", "-wal", "-shm"} {
			_ = os.Remove(c.db + suf)
		}
		if err := p.startChild(c, spec.guard); err != nil {
			return err
		}
	}
	return nil
}

func (p *pg) stopAll() {
	p.mu.Lock()
	cancel := p.cancel
	p.mu.Unlock()
	if cancel != nil {
		cancel()
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	for _, c := range p.children {
		c.stop()
	}
}

// ---- send -----------------------------------------------------------------

type sendReq struct {
	Target  string `json:"target"` // guarded | baseline | both
	Method  string `json:"method"`
	Path    string `json:"path"`
	Token   string `json:"token"`
	Session string `json:"session"`
	Body    string `json:"body"`
}

type sendRes struct {
	Target    string  `json:"target"`
	Status    int     `json:"status"`
	LatencyMS float64 `json:"latency_ms"`
	Body      string  `json:"body"`
	Error     string  `json:"error,omitempty"`
}

func (r sendReq) validate() error {
	if r.Method != http.MethodGet && r.Method != http.MethodPost {
		return errors.New("method must be GET or POST")
	}
	if len(r.Path) == 0 || r.Path[0] != '/' {
		return errors.New("path must start with /")
	}
	return nil
}

func targets(t string) ([]string, error) {
	switch t {
	case "guarded", "baseline":
		return []string{t}, nil
	case "both", "":
		return []string{"guarded", "baseline"}, nil
	}
	return nil, fmt.Errorf("unknown target %q", t)
}

func (p *pg) do(target string, r sendReq) sendRes {
	res := sendRes{Target: target}
	var body io.Reader
	if r.Body != "" {
		body = bytes.NewReader([]byte(r.Body))
	}
	req, err := http.NewRequest(r.Method, p.urls[target]+r.Path, body)
	if err != nil {
		res.Error = err.Error()
		return res
	}
	if r.Token != "" {
		req.Header.Set("Authorization", "Bearer "+r.Token)
	}
	if r.Session != "" {
		req.Header.Set("X-Session-ID", r.Session)
	}
	if r.Body != "" {
		req.Header.Set("Content-Type", "application/json")
	}
	start := time.Now()
	resp, err := p.client.Do(req)
	res.LatencyMS = float64(time.Since(start).Microseconds()) / 1000
	if err != nil {
		res.Error = err.Error()
		return res
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	res.Status, res.Body = resp.StatusCode, string(b)
	return res
}

func (p *pg) handleSend(w http.ResponseWriter, r *http.Request) {
	var in sendReq
	if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
		httpErr(w, http.StatusBadRequest, "invalid JSON")
		return
	}
	ts, err := targets(in.Target)
	if err == nil {
		err = in.validate()
	}
	if err != nil {
		httpErr(w, http.StatusBadRequest, err.Error())
		return
	}
	out := make([]sendRes, 0, len(ts))
	for _, t := range ts {
		out = append(out, p.do(t, in))
	}
	writeJSON(w, http.StatusOK, out)
}

// ---- repeat ---------------------------------------------------------------

type repeatReq struct {
	Path  string `json:"path"`
	Token string `json:"token"`
	N     int    `json:"n"`
}

type pctl struct {
	P50 float64 `json:"p50_ms"`
	P95 float64 `json:"p95_ms"`
	P99 float64 `json:"p99_ms"`
	OK  int     `json:"ok"`
	Bad int     `json:"non2xx"`
}

func pct(sorted []float64, q float64) float64 {
	if len(sorted) == 0 {
		return 0
	}
	return sorted[min(len(sorted)-1, int(q*float64(len(sorted))))]
}

func summarize(ms []float64, ok, bad int) pctl {
	sort.Float64s(ms)
	return pctl{P50: pct(ms, .50), P95: pct(ms, .95), P99: pct(ms, .99), OK: ok, Bad: bad}
}

// handleRepeat times n GETs against each side from Go, alternating the side
// that goes first so warm-up and drift hit both equally.
func (p *pg) handleRepeat(w http.ResponseWriter, r *http.Request) {
	var in repeatReq
	if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
		httpErr(w, http.StatusBadRequest, "invalid JSON")
		return
	}
	if in.N < 1 || in.N > 5000 {
		httpErr(w, http.StatusBadRequest, "n must be 1..5000")
		return
	}
	req := sendReq{Method: http.MethodGet, Path: in.Path, Token: in.Token, Session: "repeat-" + strconv.FormatInt(time.Now().UnixNano(), 36)}
	if err := req.validate(); err != nil {
		httpErr(w, http.StatusBadRequest, err.Error())
		return
	}
	lat := map[string][]float64{}
	okc, bad := map[string]int{}, map[string]int{}
	order := [2]string{"guarded", "baseline"}
	for i := 0; i < in.N+5; i++ { // 5 warm-up pairs, not counted
		o := order
		if i%2 == 1 {
			o = [2]string{"baseline", "guarded"}
		}
		for _, t := range o {
			res := p.do(t, req)
			if i < 5 {
				continue
			}
			lat[t] = append(lat[t], res.LatencyMS)
			if res.Status/100 == 2 {
				okc[t]++
			} else {
				bad[t]++
			}
		}
	}
	g, b := summarize(lat["guarded"], okc["guarded"], bad["guarded"]), summarize(lat["baseline"], okc["baseline"], bad["baseline"])
	writeJSON(w, http.StatusOK, map[string]any{
		"n": in.N, "guarded": g, "baseline": b,
		"delta": pctl{P50: g.P50 - b.P50, P95: g.P95 - b.P95, P99: g.P99 - b.P99},
	})
}

// ---- benches --------------------------------------------------------------

type benchReq struct {
	Target   string `json:"target"` // agentload only
	Agents   int    `json:"agents"`
	Duration string `json:"duration"`
	Think    string `json:"think"`
	Wallets  int    `json:"wallets"`
	API      string `json:"api"`
	Reps     int    `json:"reps"` // compare only
}

type flushWriter struct {
	w http.ResponseWriter
	f http.Flusher
}

func (fw flushWriter) Write(b []byte) (int, error) {
	n, err := fw.w.Write(b)
	fw.f.Flush()
	return n, err
}

func (b benchReq) flags() ([]string, error) {
	if b.Agents < 1 || b.Agents > 500 {
		return nil, errors.New("agents must be 1..500")
	}
	if b.Wallets < 0 || b.Wallets > 500 {
		return nil, errors.New("wallets must be 0..500")
	}
	for _, d := range []string{b.Duration, b.Think} {
		if _, err := time.ParseDuration(d); err != nil {
			return nil, fmt.Errorf("bad duration %q", d)
		}
	}
	return []string{"-agents", strconv.Itoa(b.Agents), "-duration", b.Duration, "-think", b.Think, "-wallets", strconv.Itoa(b.Wallets)}, nil
}

// stream runs one command and streams its output. Only one bench at a time.
func (p *pg) stream(w http.ResponseWriter, r *http.Request, label, name string, args, env []string) {
	fl, ok := w.(http.Flusher)
	if !ok {
		httpErr(w, http.StatusInternalServerError, "streaming unsupported")
		return
	}
	p.mu.Lock()
	if p.cancel != nil {
		p.mu.Unlock()
		httpErr(w, http.StatusConflict, "a benchmark is already running")
		return
	}
	ctx, cancel := context.WithCancel(r.Context())
	p.cancel = cancel
	p.mu.Unlock()
	defer func() {
		cancel()
		p.mu.Lock()
		p.cancel = nil
		p.mu.Unlock()
	}()

	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.Header().Set("X-Accel-Buffering", "no")
	cmd := exec.CommandContext(ctx, name, args...)
	cmd.Dir = p.root
	cmd.Env = append(os.Environ(), env...)
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Cancel = func() error { return syscall.Kill(-cmd.Process.Pid, syscall.SIGTERM) }
	cmd.WaitDelay = 5 * time.Second
	fw := flushWriter{w, fl}
	cmd.Stdout, cmd.Stderr = fw, fw
	fmt.Fprintf(fw, "$ %s\n", label)
	start := time.Now()
	took := func() string { return time.Since(start).Round(100 * time.Millisecond).String() }
	if err := cmd.Run(); err != nil {
		if ctx.Err() != nil {
			fmt.Fprintf(fw, "\n[stopped after %s]\n", took())
		} else {
			fmt.Fprintf(fw, "\n[failed after %s: %v]\n", took(), err)
		}
		return
	}
	fmt.Fprintf(fw, "\n[done in %s]\n", took())
}

func (p *pg) handleAgentload(w http.ResponseWriter, r *http.Request) {
	var in benchReq
	if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
		httpErr(w, http.StatusBadRequest, "invalid JSON")
		return
	}
	flags, err := in.flags()
	if err == nil && in.Target != "guarded" && in.Target != "baseline" {
		err = errors.New("target must be guarded or baseline")
	}
	if err == nil && in.API != "tools" && in.API != "rest" {
		err = errors.New("api must be tools or rest")
	}
	if err != nil {
		httpErr(w, http.StatusBadRequest, err.Error())
		return
	}
	args := append([]string{"-url", p.urls[in.Target]}, flags...)
	if in.Target == "baseline" {
		args = append(args, "-baseline") // baseline has no tools/roles; agentload then forces REST
	} else {
		args = append(args, "-api", in.API)
	}
	p.stream(w, r, fmt.Sprintf("agentload %v", args), filepath.Join(p.bin, "agentload"), args, nil)
}

func (p *pg) handleCompare(w http.ResponseWriter, r *http.Request) {
	var in benchReq
	if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
		httpErr(w, http.StatusBadRequest, "invalid JSON")
		return
	}
	flags, err := in.flags()
	if err == nil && (in.Reps < 1 || in.Reps > 20) {
		err = errors.New("reps must be 1..20")
	}
	if err != nil {
		httpErr(w, http.StatusBadRequest, err.Error())
		return
	}
	script := filepath.Join(p.root, "benchmarks", "compare.sh")
	p.stream(w, r, fmt.Sprintf("REPS=%d compare.sh %v", in.Reps, flags), script, flags, []string{"REPS=" + strconv.Itoa(in.Reps)})
}

func (p *pg) handleStop(w http.ResponseWriter, _ *http.Request) {
	p.mu.Lock()
	cancel := p.cancel
	p.mu.Unlock()
	if cancel != nil {
		cancel()
	}
	writeJSON(w, http.StatusOK, map[string]bool{"stopped": cancel != nil})
}

func (p *pg) handleReset(w http.ResponseWriter, _ *http.Request) {
	p.mu.Lock()
	busy := p.cancel != nil
	p.mu.Unlock()
	if busy {
		httpErr(w, http.StatusConflict, "stop the running benchmark first")
		return
	}
	if err := p.startAll(); err != nil {
		httpErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]bool{"reset": true})
}

// ---- wiring ---------------------------------------------------------------

func (p *pg) mux() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /{$}", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		_, _ = w.Write(uiHTML)
	})
	mux.HandleFunc("POST /api/send", p.handleSend)
	mux.HandleFunc("POST /api/repeat", p.handleRepeat)
	mux.HandleFunc("GET /api/bench/options", p.handleBenchOptions)
	mux.HandleFunc("POST /api/bench/run", p.handleBenchRun)
	mux.HandleFunc("POST /api/bench/agentload", p.handleAgentload)
	mux.HandleFunc("POST /api/bench/compare", p.handleCompare)
	mux.HandleFunc("POST /api/bench/stop", p.handleStop)
	mux.HandleFunc("POST /api/reset", p.handleReset)
	return mux
}

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(v)
}

func httpErr(w http.ResponseWriter, code int, msg string) {
	writeJSON(w, code, map[string]string{"error": msg})
}

func repoRoot() (string, error) {
	dir, err := os.Getwd()
	if err != nil {
		return "", err
	}
	for {
		if _, err := os.Stat(filepath.Join(dir, "go.work")); err == nil {
			return dir, nil
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			return "", errors.New("go.work not found; run from inside the repo (make playground)")
		}
		dir = parent
	}
}

func fatal(msg string, err error) {
	slog.Error(msg, "err", err)
	os.Exit(1)
}

func main() {
	port := os.Getenv("PLAYGROUND_PORT")
	if port == "" {
		port = "9090"
	}
	root, err := repoRoot()
	if err != nil {
		fatal("repo root", err)
	}
	bin, err := os.MkdirTemp("", "playground-bin-")
	if err != nil {
		fatal("temp dir", err)
	}
	defer os.RemoveAll(bin)

	slog.Info("building service and agentload")
	for _, b := range [][2]string{{"service", "./examples/wallet-service"}, {"agentload", "./benchmarks/agentload"}} {
		cmd := exec.Command("go", "build", "-o", filepath.Join(bin, b[0]), b[1])
		cmd.Dir = root
		if out, err := cmd.CombinedOutput(); err != nil {
			fatal("build "+b[0], fmt.Errorf("%w\n%s", err, out))
		}
	}

	p := newPG(map[string]string{
		"guarded":  "http://localhost:" + guardedPort,
		"baseline": "http://localhost:" + baselinePort,
	})
	p.root, p.bin = root, bin
	if err := p.startAll(); err != nil {
		p.stopAll()
		fatal("start services", err)
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	srv := &http.Server{Addr: "127.0.0.1:" + port, Handler: p.mux()}
	go func() {
		<-ctx.Done()
		p.stopAll()
		sctx, c := context.WithTimeout(context.Background(), 3*time.Second)
		defer c()
		_ = srv.Shutdown(sctx)
	}()
	slog.Info("playground ready", "url", "http://localhost:"+port, "guarded", guardedPort, "baseline", baselinePort)
	if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
		p.stopAll()
		fatal("server", err)
	}
	// Give stopAll (in the shutdown goroutine) a moment to finish.
	time.Sleep(500 * time.Millisecond)
}
