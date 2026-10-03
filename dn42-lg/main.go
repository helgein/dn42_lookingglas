package main

import (
	"context"
	"embed"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"io/fs"
	"log"
	"math"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/signal"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"
)

//go:embed web
var webFS embed.FS

const histLen = 90

// ---- config ---------------------------------------------------------------

type remote struct{ Name, URL string }

type remoteList []remote

func (r *remoteList) String() string {
	var s []string
	for _, x := range *r {
		s = append(s, x.Name+"="+x.URL)
	}
	return strings.Join(s, ",")
}

func (r *remoteList) Set(v string) error {
	name, u, ok := strings.Cut(v, "=")
	if !ok || name == "" || u == "" {
		return errors.New("Format: name=http://host:port")
	}
	if _, err := url.ParseRequestURI(u); err != nil {
		return err
	}
	*r = append(*r, remote{Name: name, URL: strings.TrimRight(u, "/")})
	return nil
}

type config struct {
	listen   string
	socket   string
	name     string
	title    string
	asn      uint64
	prefixes []*net.IPNet
	interval time.Duration
	remotes  remoteList
}

// ---- node state -----------------------------------------------------------

type OwnPrefix struct {
	Prefix    string          `json:"prefix"`
	Announced map[string]bool `json:"announced"`
}

type NodeState struct {
	Node      string      `json:"node"`
	Local     bool        `json:"local"`
	OK        bool        `json:"ok"`
	Error     string      `json:"error,omitempty"`
	Updated   int64       `json:"updated"`
	Status    Status      `json:"status"`
	Protocols []Protocol  `json:"protocols"`
	Own       []OwnPrefix `json:"own"`
}

type State struct {
	ASN       uint64      `json:"asn"`
	Title     string      `json:"title"`
	Interval  float64     `json:"interval"`
	Generated int64       `json:"generated"`
	Nodes     []NodeState `json:"nodes"`
}

type ring struct{ in, out []float64 }

func (r *ring) push(in, out float64) {
	r.in = append(r.in, round2(in))
	r.out = append(r.out, round2(out))
	if len(r.in) > histLen {
		r.in = r.in[len(r.in)-histLen:]
		r.out = r.out[len(r.out)-histLen:]
	}
}

func round2(f float64) float64 { return math.Round(f*100) / 100 }

var reName = regexp.MustCompile(`^[A-Za-z0-9_.-]+$`)

// ---- local poller ---------------------------------------------------------

type localPoller struct {
	cfg  *config
	bird *birdClient

	mu    sync.RWMutex
	state NodeState

	prev    map[string][2]int64
	prevT   time.Time
	hist    map[string]*ring
	own     []OwnPrefix
	lastOwn time.Time
}

func newLocalPoller(cfg *config, b *birdClient) *localPoller {
	return &localPoller{
		cfg:   cfg,
		bird:  b,
		prev:  map[string][2]int64{},
		hist:  map[string]*ring{},
		state: NodeState{Node: cfg.name, Local: true, Error: "noch keine Daten"},
	}
}

func (p *localPoller) get() NodeState {
	p.mu.RLock()
	defer p.mu.RUnlock()
	return p.state
}

func (p *localPoller) loop(ctx context.Context) {
	t := time.NewTicker(p.cfg.interval)
	defer t.Stop()
	p.poll()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			p.poll()
		}
	}
}

func (p *localPoller) poll() {
	now := time.Now()
	st := NodeState{Node: p.cfg.name, Local: true, Updated: now.UnixMilli()}

	lines, err := p.bird.run("show protocols all", 0)
	if err != nil {
		st.Error = err.Error()
		p.mu.Lock()
		p.state = st
		p.mu.Unlock()
		return
	}
	protos := parseProtocols(lines)
	if sl, err := p.bird.run("show status", 0); err == nil {
		st.Status = parseStatus(sl)
	}

	dt := now.Sub(p.prevT).Seconds()
	seen := map[string]bool{}
	for i := range protos {
		pr := &protos[i]
		seen[pr.Name] = true
		var in, out int64
		for _, c := range pr.Channels {
			in += c.ImpUpdRecv + c.ImpWdrRecv
			out += c.ExpUpdAcc + c.ExpWdrAcc
		}
		if prev, ok := p.prev[pr.Name]; ok && dt > 0 && in >= prev[0] && out >= prev[1] {
			pr.RateIn = round2(float64(in-prev[0]) / dt)
			pr.RateOut = round2(float64(out-prev[1]) / dt)
		}
		p.prev[pr.Name] = [2]int64{in, out}
		h := p.hist[pr.Name]
		if h == nil {
			h = &ring{}
			p.hist[pr.Name] = h
		}
		h.push(pr.RateIn, pr.RateOut)
		pr.HistIn = append([]float64(nil), h.in...)
		pr.HistOut = append([]float64(nil), h.out...)
	}
	for k := range p.hist { // forget removed protocols
		if !seen[k] {
			delete(p.hist, k)
			delete(p.prev, k)
		}
	}
	p.prevT = now

	if len(p.cfg.prefixes) > 0 && now.Sub(p.lastOwn) >= 5*time.Second {
		p.own = p.checkOwn(protos)
		p.lastOwn = now
	}

	st.OK = true
	st.Protocols = protos
	st.Own = p.own
	p.mu.Lock()
	p.state = st
	p.mu.Unlock()
}

// checkOwn asks BIRD, per own prefix and eBGP session, whether the prefix is
// currently exported to that peer.
func (p *localPoller) checkOwn(protos []Protocol) []OwnPrefix {
	var res []OwnPrefix
	for _, pfx := range p.cfg.prefixes {
		v4 := pfx.IP.To4() != nil
		op := OwnPrefix{Prefix: pfx.String(), Announced: map[string]bool{}}
		for _, pr := range protos {
			if pr.Kind != "ebgp" || !reName.MatchString(pr.Name) {
				continue
			}
			table := ""
			for _, c := range pr.Channels {
				if (v4 && c.Name == "ipv4") || (!v4 && c.Name == "ipv6") {
					table = c.Table
				}
			}
			if table == "" || !reName.MatchString(table) {
				continue
			}
			if pr.State != "up" {
				op.Announced[pr.Name] = false
				continue
			}
			cmd := fmt.Sprintf("show route %s table %s export %s", pfx.String(), table, pr.Name)
			lines, err := p.bird.run(cmd, 0)
			switch {
			case err == nil:
				found := false
				for _, l := range lines {
					if l.Code == "1007" {
						found = true
						break
					}
				}
				op.Announced[pr.Name] = found
			case isNotFound(err):
				op.Announced[pr.Name] = false
			}
		}
		res = append(res, op)
	}
	return res
}

// ---- remote poller --------------------------------------------------------

type remotePoller struct {
	r      remote
	client *http.Client
	every  time.Duration

	mu    sync.RWMutex
	state NodeState
}

func (p *remotePoller) get() NodeState {
	p.mu.RLock()
	defer p.mu.RUnlock()
	return p.state
}

func (p *remotePoller) loop(ctx context.Context) {
	t := time.NewTicker(p.every)
	defer t.Stop()
	p.poll(ctx)
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			p.poll(ctx)
		}
	}
}

func (p *remotePoller) poll(ctx context.Context) {
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, p.r.URL+"/api/local", nil)
	var st NodeState
	resp, err := p.client.Do(req)
	if err == nil {
		defer resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			err = fmt.Errorf("HTTP %d", resp.StatusCode)
		} else {
			err = json.NewDecoder(io.LimitReader(resp.Body, 8<<20)).Decode(&st)
		}
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	if err != nil {
		old := p.state
		old.Node, old.Local, old.OK = p.r.Name, false, false
		old.Error = "Knoten nicht erreichbar: " + err.Error()
		p.state = old
		return
	}
	st.Node, st.Local = p.r.Name, false
	p.state = st
}

// ---- aggregation ----------------------------------------------------------

type app struct {
	cfg     *config
	bird    *birdClient
	local   *localPoller
	remotes []*remotePoller
	lookups chan struct{}
}

func (a *app) snapshot() State {
	s := State{
		ASN:       a.cfg.asn,
		Title:     a.cfg.title,
		Interval:  a.cfg.interval.Seconds(),
		Generated: time.Now().UnixMilli(),
	}
	s.Nodes = append(s.Nodes, a.local.get())
	for _, r := range a.remotes {
		s.Nodes = append(s.Nodes, r.get())
	}
	if s.ASN == 0 {
	outer:
		for _, n := range s.Nodes {
			for _, p := range n.Protocols {
				if p.LocalAS != 0 {
					s.ASN = p.LocalAS
					break outer
				}
			}
		}
	}
	return s
}

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(v)
}

func (a *app) handleState(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, a.snapshot())
}

func (a *app) handleLocal(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, a.local.get())
}

func (a *app) handleStream(w http.ResponseWriter, r *http.Request) {
	fl, ok := w.(http.Flusher)
	if !ok {
		http.Error(w, "streaming nicht unterstützt", http.StatusInternalServerError)
		return
	}
	h := w.Header()
	h.Set("Content-Type", "text/event-stream")
	h.Set("Cache-Control", "no-store")
	h.Set("X-Accel-Buffering", "no")
	send := func() bool {
		b, err := json.Marshal(a.snapshot())
		if err != nil {
			return false
		}
		if _, err := fmt.Fprintf(w, "data: %s\n\n", b); err != nil {
			return false
		}
		fl.Flush()
		return true
	}
	if !send() {
		return
	}
	t := time.NewTicker(a.cfg.interval)
	defer t.Stop()
	for {
		select {
		case <-r.Context().Done():
			return
		case <-t.C:
			if !send() {
				return
			}
		}
	}
}

type lookupResult struct {
	Node      string   `json:"node"`
	Mode      string   `json:"mode"`
	Query     string   `json:"query"`
	Commands  []string `json:"commands"`
	Output    string   `json:"output"`
	Truncated bool     `json:"truncated"`
	Error     string   `json:"error,omitempty"`
}

const maxLines = 1000

func (a *app) handleRoute(w http.ResponseWriter, r *http.Request) {
	q := strings.TrimSpace(r.URL.Query().Get("q"))
	mode := r.URL.Query().Get("mode")
	node := r.URL.Query().Get("node")
	if mode == "" {
		mode = "for"
	}

	if node != "" && node != a.cfg.name {
		for _, rp := range a.remotes {
			if rp.r.Name == node {
				a.proxyRoute(w, r, rp, mode, q)
				return
			}
		}
		writeJSON(w, http.StatusNotFound, lookupResult{Node: node, Error: "Unbekannter Knoten " + node})
		return
	}

	res := lookupResult{Node: a.cfg.name, Mode: mode, Query: q}
	var cmds []string
	timeout := 5 * time.Second
	switch mode {
	case "for":
		if ip := net.ParseIP(q); ip != nil {
			q = ip.String()
		} else if _, n, err := net.ParseCIDR(q); err == nil {
			q = n.String()
		} else {
			res.Error = "Bitte eine IP-Adresse oder ein Präfix eingeben, z. B. 172.20.0.53 oder fd42:d42:d42::/48."
			writeJSON(w, http.StatusBadRequest, res)
			return
		}
		cmds = []string{"show route for " + q + " all"}
	case "as":
		asn, err := strconv.ParseUint(strings.TrimPrefix(strings.ToUpper(q), "AS"), 10, 32)
		if err != nil || asn == 0 {
			res.Error = "Bitte eine AS-Nummer eingeben, z. B. 4242423914."
			writeJSON(w, http.StatusBadRequest, res)
			return
		}
		q = strconv.FormatUint(asn, 10)
		timeout = 10 * time.Second
		tables := map[string]bool{}
		for _, p := range a.local.get().Protocols {
			for _, c := range p.Channels {
				if reName.MatchString(c.Table) && strings.HasPrefix(c.Table, "master") {
					tables[c.Table] = true
				}
			}
		}
		if len(tables) == 0 {
			tables["master4"], tables["master6"] = true, true
		}
		for _, t := range []string{"master4", "master6"} {
			if tables[t] {
				delete(tables, t)
				cmds = append(cmds, fmt.Sprintf("show route table %s where bgp_path ~ [= * %s * =] primary", t, q))
			}
		}
		for t := range tables {
			cmds = append(cmds, fmt.Sprintf("show route table %s where bgp_path ~ [= * %s * =] primary", t, q))
		}
	default:
		res.Error = "Unbekannter Modus"
		writeJSON(w, http.StatusBadRequest, res)
		return
	}
	res.Query, res.Commands = q, cmds

	select {
	case a.lookups <- struct{}{}:
		defer func() { <-a.lookups }()
	case <-time.After(3 * time.Second):
		res.Error = "Gerade laufen zu viele Abfragen. Bitte in ein paar Sekunden erneut versuchen."
		writeJSON(w, http.StatusTooManyRequests, res)
		return
	}

	var b strings.Builder
	n := 0
	for _, c := range cmds {
		lines, err := a.bird.run(c, timeout)
		if err != nil && !isNotFound(err) {
			res.Error = err.Error()
		}
		for _, l := range lines {
			if n >= maxLines {
				res.Truncated = true
				break
			}
			b.WriteString(l.Text)
			b.WriteByte('\n')
			n++
		}
	}
	res.Output = b.String()
	writeJSON(w, http.StatusOK, res)
}

func (a *app) proxyRoute(w http.ResponseWriter, r *http.Request, rp *remotePoller, mode, q string) {
	u := rp.r.URL + "/api/route?" + url.Values{"mode": {mode}, "q": {q}}.Encode()
	ctx, cancel := context.WithTimeout(r.Context(), 15*time.Second)
	defer cancel()
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	resp, err := rp.client.Do(req)
	if err != nil {
		writeJSON(w, http.StatusBadGateway, lookupResult{Node: rp.r.Name, Mode: mode, Query: q,
			Error: "Knoten " + rp.r.Name + " nicht erreichbar: " + err.Error()})
		return
	}
	defer resp.Body.Close()
	var res lookupResult
	if err := json.NewDecoder(io.LimitReader(resp.Body, 8<<20)).Decode(&res); err != nil {
		writeJSON(w, http.StatusBadGateway, lookupResult{Node: rp.r.Name, Error: "Ungültige Antwort von " + rp.r.Name})
		return
	}
	res.Node = rp.r.Name
	writeJSON(w, resp.StatusCode, res)
}

func securityHeaders(h http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hd := w.Header()
		hd.Set("Content-Security-Policy", "default-src 'self'; img-src 'self' data:; style-src 'self'; script-src 'self'; connect-src 'self'; frame-ancestors 'none'")
		hd.Set("X-Content-Type-Options", "nosniff")
		hd.Set("Referrer-Policy", "no-referrer")
		if r.Method != http.MethodGet && r.Method != http.MethodHead {
			http.Error(w, "nur GET", http.StatusMethodNotAllowed)
			return
		}
		h.ServeHTTP(w, r)
	})
}

// ---- main -----------------------------------------------------------------

func main() {
	cfg := &config{}
	host, _ := os.Hostname()
	host, _, _ = strings.Cut(host, ".")
	var prefixes string
	flag.StringVar(&cfg.listen, "listen", "127.0.0.1:8042", "Listen-Adressen, kommagetrennt, z. B. 172.20.15.129:8042,[fd26:3f06:520e::1]:8042")
	flag.StringVar(&cfg.socket, "socket", "/run/bird/bird.ctl", "BIRD-Control-Socket")
	flag.StringVar(&cfg.name, "name", host, "Name dieses Knotens")
	flag.StringVar(&cfg.title, "title", "", "Titel (Default: AS-Nummer)")
	flag.Uint64Var(&cfg.asn, "asn", 0, "eigene ASN (Default: aus BIRD ermittelt)")
	flag.StringVar(&prefixes, "prefixes", "", "eigene Präfixe, kommagetrennt (für die Ankündigungs-Matrix)")
	flag.DurationVar(&cfg.interval, "interval", time.Second, "Abfrageintervall")
	flag.Var(&cfg.remotes, "remote", "weiterer Knoten name=http://host:port (mehrfach möglich)")
	flag.Parse()

	if cfg.interval < 500*time.Millisecond {
		cfg.interval = 500 * time.Millisecond
	}
	for _, s := range strings.Split(prefixes, ",") {
		if s = strings.TrimSpace(s); s == "" {
			continue
		}
		_, n, err := net.ParseCIDR(s)
		if err != nil {
			log.Fatalf("ungültiges Präfix %q: %v", s, err)
		}
		cfg.prefixes = append(cfg.prefixes, n)
	}

	b := &birdClient{socket: cfg.socket, timeout: 3 * time.Second}
	a := &app{cfg: cfg, bird: b, local: newLocalPoller(cfg, b), lookups: make(chan struct{}, 2)}
	for _, r := range cfg.remotes {
		a.remotes = append(a.remotes, &remotePoller{
			r:      r,
			client: &http.Client{Timeout: 3 * time.Second},
			every:  cfg.interval,
			state:  NodeState{Node: r.Name, Error: "noch keine Daten"},
		})
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	go a.local.loop(ctx)
	for _, r := range a.remotes {
		go r.loop(ctx)
	}

	static, _ := fs.Sub(webFS, "web")
	mux := http.NewServeMux()
	mux.Handle("/", http.FileServer(http.FS(static)))
	mux.HandleFunc("/api/state", a.handleState)
	mux.HandleFunc("/api/local", a.handleLocal)
	mux.HandleFunc("/api/stream", a.handleStream)
	mux.HandleFunc("/api/route", a.handleRoute)

	var lns []net.Listener
	for _, addr := range strings.Split(cfg.listen, ",") {
		if addr = strings.TrimSpace(addr); addr == "" {
			continue
		}
		ln, err := net.Listen("tcp", addr)
		if err != nil {
			log.Fatalf("listen %s: %v", addr, err)
		}
		lns = append(lns, ln)
	}
	if len(lns) == 0 {
		log.Fatal("keine Listen-Adresse angegeben")
	}

	srv := &http.Server{
		Handler:           securityHeaders(mux),
		ReadHeaderTimeout: 5 * time.Second,
		IdleTimeout:       120 * time.Second,
		BaseContext:       func(net.Listener) context.Context { return ctx },
	}
	go func() {
		<-ctx.Done()
		sh, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()
		_ = srv.Shutdown(sh)
	}()
	errc := make(chan error, len(lns))
	for _, ln := range lns {
		log.Printf("dn42-lg %s lauscht auf %s", cfg.name, ln.Addr())
		go func(ln net.Listener) { errc <- srv.Serve(ln) }(ln)
	}
	log.Printf("BIRD %s, %d Remote(s)", cfg.socket, len(cfg.remotes))
	if err := <-errc; err != nil && !errors.Is(err, http.ErrServerClosed) {
		log.Fatal(err)
	}
}
