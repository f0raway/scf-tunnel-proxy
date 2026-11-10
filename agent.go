package main

import (
	"context"
	"crypto/subtle"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"github.com/armon/go-socks5"
	"github.com/gorilla/websocket"
	"io"
	"log"
	"net"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"
)

type targetDialError struct{ detail string }

func (e *targetDialError) Error() string { return "target connection failed: " + e.detail }

type nodeConfig struct {
	Region string `json:"region"`
	URL    string `json:"url"`
}
type agentConfig struct {
	HTTP           string       `json:"http_addr"`
	SOCKS          string       `json:"socks_addr"`
	Dashboard      string       `json:"dashboard_addr"`
	User           string       `json:"user"`
	Password       string       `json:"password"`
	Token          string       `json:"token"`
	Nodes          []nodeConfig `json:"nodes"`
	Ledger         string       `json:"ledger_path"`
	Budget         float64      `json:"monthly_limit_yuan"`
	MaxConnections int          `json:"max_connections"`
}
type nodeState struct {
	nodeConfig
	Failures       int
	RetryAfter     time.Time
	Opens          uint64
	LastError      string
	TargetFailures uint64
}
type provider struct {
	targetCooldown       map[string]time.Time
	cachedTargetFailures atomic.Uint64
	allowPrivate         bool // Only enabled by local tests; never populated from configuration.
	lifeMu               sync.Mutex
	stopping             bool
	opening              sync.WaitGroup
	live                 map[*wsStream]struct{}
	stopCtx              context.Context
	cancel               context.CancelFunc
	mu                   sync.Mutex
	nodes                []*nodeState
	index                int
	token                string
	meter                *meter
	slots                chan struct{}
	active               atomic.Int64
	success              atomic.Uint64
	failed               atomic.Uint64
	rejectedPrivate      atomic.Uint64
	rejectedLimit        atomic.Uint64
	rejectedBudget       atomic.Uint64
	noNodes              atomic.Uint64
}

func newProvider(c agentConfig) (*provider, error) {
	if len(c.Nodes) == 0 || c.Token == "" {
		return nil, fmt.Errorf("missing authenticated nodes")
	}
	m, e := openMeter(c.Ledger, c.Budget)
	if e != nil {
		return nil, e
	}
	stopCtx, cancel := context.WithCancel(context.Background())
	limit := c.MaxConnections
	if limit == 0 {
		limit = 32
	}
	if limit < 1 || limit > 64 {
		cancel()
		return nil, fmt.Errorf("max_connections must be between 1 and 64")
	}
	p := &provider{token: c.Token, meter: m, slots: make(chan struct{}, limit), live: make(map[*wsStream]struct{}), targetCooldown: make(map[string]time.Time), stopCtx: stopCtx, cancel: cancel}
	for _, n := range c.Nodes {
		p.nodes = append(p.nodes, &nodeState{nodeConfig: n})
	}
	return p, nil
}
func (p *provider) next(exclude map[*nodeState]bool) *nodeState {
	p.mu.Lock()
	defer p.mu.Unlock()
	for i := 0; i < len(p.nodes); i++ {
		n := p.nodes[p.index%len(p.nodes)]
		p.index = (p.index + 1) % len(p.nodes)
		if !exclude[n] && time.Now().After(n.RetryAfter) {
			return n
		}
	}
	return nil
}
func (p *provider) mark(n *nodeState, ok bool) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if ok {
		n.Failures = 0
		n.RetryAfter = time.Time{}
		n.Opens++
	} else {
		n.Failures++
		if n.Failures >= 3 {
			n.RetryAfter = time.Now().Add(30 * time.Second)
		}
	}
}
func (p *provider) dial(ctx context.Context, network, address string) (net.Conn, error) {
	host, _, splitErr := net.SplitHostPort(address)
	if splitErr != nil {
		return nil, splitErr
	}
	if ip := net.ParseIP(host); !p.allowPrivate && ip != nil && (!ip.IsGlobalUnicast() || ip.IsPrivate() || ip.IsLoopback() || ip.IsLinkLocalUnicast()) {
		p.rejectedPrivate.Add(1)
		return nil, fmt.Errorf("private/local target %s cannot be reached through SCF; configure Direct in Proxifier", address)
	}

	p.mu.Lock()
	until := p.targetCooldown[address]
	p.mu.Unlock()
	if time.Now().Before(until) {
		p.cachedTargetFailures.Add(1)
		return nil, fmt.Errorf("target connection recently failed; retry after %s", time.Until(until).Round(time.Second))
	}
	p.lifeMu.Lock()
	if p.stopping {
		p.lifeMu.Unlock()
		return nil, fmt.Errorf("proxy is stopping")
	}
	p.opening.Add(1)
	p.lifeMu.Unlock()
	defer p.opening.Done()
	ctx, cancel := context.WithCancel(ctx)
	stopCancel := context.AfterFunc(p.stopCtx, cancel)
	defer func() { stopCancel(); cancel() }()

	queueTimer := time.NewTimer(5 * time.Second)
	defer queueTimer.Stop()
	select {
	case p.slots <- struct{}{}:
	case <-ctx.Done():
		return nil, ctx.Err()
	case <-queueTimer.C:
		p.rejectedLimit.Add(1)
		log.Printf("tunnel_reject reason=connection_limit active=%d target=%q", p.active.Load(), address)
		return nil, fmt.Errorf("maximum active connections reached")
	}
	release := true
	defer func() {
		if release {
			<-p.slots
		}
	}()
	tried := map[*nodeState]bool{}
	targetFailures := 0
	var last error
	for i := 0; i < 2; i++ {
		n := p.next(tried)
		if n == nil {
			break
		}
		tried[n] = true
		charge, e := p.meter.begin()
		if e != nil {
			p.rejectedBudget.Add(1)
			log.Printf("tunnel_reject reason=budget target=%q", address)
			return nil, e
		}
		finish := charge.finish
		start := time.Now()
		headers := http.Header{"X-Tunnel-Token": []string{p.token}, "X-Tunnel-Target": []string{address}, "X-Tunnel-Protocol": []string{"2"}}
		dialer := websocket.Dialer{HandshakeTimeout: 25 * time.Second, ReadBufferSize: chunkSize, WriteBufferSize: chunkSize, Proxy: nil, NetDialContext: (&net.Dialer{Timeout: 8 * time.Second, KeepAlive: 30 * time.Second}).DialContext}
		ws, response, e := dialer.DialContext(ctx, n.URL, headers)
		if e == nil {
			ws.SetReadLimit(2048)
			ws.SetReadDeadline(time.Now().Add(15 * time.Second))
			var ack dialAck
			e = ws.ReadJSON(&ack)
			ws.SetReadDeadline(time.Time{})
			if e == nil && !ack.OK {
				if ack.Error == "target" {
					e = &targetDialError{detail: ack.Detail}
				} else {
					e = fmt.Errorf("invalid tunnel connection acknowledgement")
				}
			}
			if e != nil {
				ws.Close()
			}
		}
		if e != nil {
			status := 0
			detail := ""
			var targetErr *targetDialError
			targetFailure := errors.As(e, &targetErr)
			if targetFailure {
				detail = targetErr.detail
			}
			if response != nil {
				status = response.StatusCode
				if response.Body != nil {
					raw, _ := io.ReadAll(io.LimitReader(response.Body, 512))
					detail = strings.TrimSpace(string(raw))
					response.Body.Close()
				}
				targetFailure = targetFailure || status == 502 && (strings.Contains(detail, "Target connection failed") || response.Header.Get("X-Tunnel-Error") == "target")
			}
			if err := finish(time.Since(start), 0); err != nil {
				log.Printf("budget ledger: %v", err)
			}
			summary := fmt.Sprintf("status=%d error=%v detail=%s", status, e, detail)
			p.mu.Lock()
			n.LastError = summary
			if targetFailure {
				targetFailures++
				n.TargetFailures++
				n.Failures = 0
				n.RetryAfter = time.Time{}
			}
			p.mu.Unlock()
			log.Printf("tunnel_open_failed region=%s target=%q status=%d target_failure=%t duration=%s error=%q detail=%q", n.Region, address, status, targetFailure, time.Since(start).Round(time.Millisecond), e.Error(), detail)
			if !targetFailure {
				p.mark(n, false)
			}
			last = fmt.Errorf("%s: HTTP %d: %w", n.Region, status, e)
			p.failed.Add(1)
			continue
		}
		p.lifeMu.Lock()
		if p.stopping {
			p.lifeMu.Unlock()
			ws.Close()
			finish(time.Since(start), 0)
			return nil, fmt.Errorf("proxy is stopping")
		}
		p.mark(n, true)
		p.success.Add(1)
		p.active.Add(1)
		stream := newStream(ws)
		stream.onProgress = func(n int64) error { return charge.progress(time.Since(start), n) }
		stop := make(chan struct{})
		stream.onClose = func() {
			p.lifeMu.Lock()
			delete(p.live, stream)
			p.lifeMu.Unlock()
			close(stop)
			<-p.slots
			p.active.Add(-1)
			if err := finish(time.Since(start), stream.transferred.Load()); err != nil {
				log.Printf("budget ledger: %v", err)
			}
		}
		p.live[stream] = struct{}{}
		p.lifeMu.Unlock()
		release = false
		go func() {
			timer := time.NewTimer(maxTunnelSeconds * time.Second)
			defer timer.Stop()
			tick := time.NewTicker(5 * time.Second)
			defer tick.Stop()
			for {
				select {
				case <-stop:
					return
				case <-timer.C:
					stream.Close()
					return
				case <-tick.C:
					if err := charge.progress(time.Since(start), stream.transferred.Load()); err != nil {
						log.Printf("tunnel_closed reason=budget region=%s target=%q error=%q", n.Region, address, err)
						stream.Close()
						return
					}
					if time.Since(time.Unix(0, stream.lastActivity.Load())) > idleSeconds*time.Second {
						stream.Close()
						return
					}
				}
			}
		}()
		return stream, nil
	}
	if targetFailures > 0 && targetFailures == len(tried) {
		p.mu.Lock()
		if len(p.targetCooldown) >= 2048 {
			for key, value := range p.targetCooldown {
				if time.Now().After(value) {
					delete(p.targetCooldown, key)
				}
			}
			if len(p.targetCooldown) >= 2048 {
				for key := range p.targetCooldown {
					delete(p.targetCooldown, key)
					break
				}
			}
		}
		p.targetCooldown[address] = time.Now().Add(15 * time.Second)
		p.mu.Unlock()
		log.Printf("target_backoff target=%q seconds=15", address)
	}

	if last == nil {
		p.noNodes.Add(1)
		log.Printf("tunnel_reject reason=no_healthy_nodes target=%q", address)
		last = fmt.Errorf("no healthy tunnel nodes")
	}
	return nil, last
}
func (p *provider) shutdown() {
	p.lifeMu.Lock()
	p.stopping = true
	p.cancel()
	connections := make([]*wsStream, 0, len(p.live))
	for s := range p.live {
		connections = append(connections, s)
	}
	p.lifeMu.Unlock()
	for _, s := range connections {
		s.Close()
	}
	p.opening.Wait()
	log.Printf("proxy_shutdown active=%d ledger_settled=true", p.active.Load())
}

func (p *provider) stats() map[string]any {
	p.mu.Lock()
	defer p.mu.Unlock()
	nodes := []map[string]any{}
	for _, n := range p.nodes {
		status := "Healthy"
		if time.Now().Before(n.RetryAfter) {
			status = "Cooling"
		}
		nodes = append(nodes, map[string]any{"region": n.Region, "status": status, "failure_count": n.Failures, "connections": n.Opens, "target_failures": n.TargetFailures, "last_error": n.LastError})
	}
	return map[string]any{"mode": "tcp-tunnel-no-mitm", "nodes": nodes, "active": p.active.Load(), "opening_or_active": len(p.slots), "max_connections": cap(p.slots), "success": p.success.Load(), "failed": p.failed.Load(), "rejected_limit": p.rejectedLimit.Load(), "rejected_private": p.rejectedPrivate.Load(), "cached_target_failures": p.cachedTargetFailures.Load(), "rejected_budget": p.rejectedBudget.Load(), "rejected_no_nodes": p.noNodes.Load(), "cost_estimate": p.meter.snapshot(), "monthly_limit_yuan": p.meter.limit, "max_connection_seconds": maxTunnelSeconds, "max_connection_bytes": maxTunnelBytes}
}
func authOK(r *http.Request, c agentConfig) bool {
	parts := strings.SplitN(r.Header.Get("Proxy-Authorization"), " ", 2)
	if len(parts) != 2 || !strings.EqualFold(parts[0], "Basic") {
		return false
	}
	v, e := base64.StdEncoding.DecodeString(parts[1])
	return e == nil && subtle.ConstantTimeCompare(v, []byte(c.User+":"+c.Password)) == 1
}
func stripHop(h http.Header) {
	for _, v := range h.Values("Connection") {
		for _, k := range strings.Split(v, ",") {
			h.Del(strings.TrimSpace(k))
		}
	}
	for _, k := range []string{"Proxy-Authorization", "Proxy-Authenticate", "Proxy-Connection", "Connection", "Keep-Alive", "TE", "Trailer", "Transfer-Encoding", "Upgrade"} {
		h.Del(k)
	}
}
func httpProxy(c agentConfig, p *provider) http.Handler {
	transport := &http.Transport{DialContext: p.dial, DisableCompression: true, MaxIdleConns: 16, MaxIdleConnsPerHost: 2, IdleConnTimeout: 20 * time.Second, ResponseHeaderTimeout: 45 * time.Second}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !authOK(r, c) {
			w.Header().Set("Proxy-Authenticate", `Basic realm="CloudTunnel"`)
			http.Error(w, "Proxy authentication required", 407)
			return
		}
		if r.Method == "CONNECT" {
			address := r.Host
			if _, _, e := net.SplitHostPort(address); e != nil {
				http.Error(w, "CONNECT requires host:port", 400)
				return
			}
			remote, e := p.dial(r.Context(), "tcp", address)
			if e != nil {
				http.Error(w, "Tunnel unavailable: "+e.Error(), 503)
				return
			}
			raw, rw, e := w.(http.Hijacker).Hijack()
			if e != nil {
				remote.Close()
				return
			}
			if _, e = rw.WriteString("HTTP/1.1 200 Connection Established\r\n\r\n"); e != nil {
				raw.Close()
				remote.Close()
				return
			}
			if e = rw.Flush(); e != nil {
				raw.Close()
				remote.Close()
				return
			}
			buffered := &bufferedConn{Conn: raw, reader: rw.Reader}
			relay(buffered, remote.(*wsStream))
			return
		}
		if r.URL.Scheme != "http" || r.URL.Host == "" {
			http.Error(w, "Use CONNECT for HTTPS", 400)
			return
		}
		outgoing := r.Clone(r.Context())
		outgoing.RequestURI = ""
		outgoing.Header = r.Header.Clone()
		stripHop(outgoing.Header)
		resp, e := transport.RoundTrip(outgoing)
		if e != nil {
			http.Error(w, "Proxy upstream unavailable", 502)
			return
		}
		defer resp.Body.Close()
		stripHop(resp.Header)
		for k, vs := range resp.Header {
			for _, v := range vs {
				w.Header().Add(k, v)
			}
		}
		w.WriteHeader(resp.StatusCode)
		io.Copy(w, resp.Body)
	})
}

type bufferedConn struct {
	net.Conn
	reader io.Reader
}

func (c *bufferedConn) Read(b []byte) (int, error) { return c.reader.Read(b) }
func (c *bufferedConn) CloseWrite() error {
	if x, ok := c.Conn.(interface{ CloseWrite() error }); ok {
		return x.CloseWrite()
	}
	return c.Conn.Close()
}

type remoteResolver struct{}

func (remoteResolver) Resolve(ctx context.Context, name string) (context.Context, net.IP, error) {
	return ctx, nil, nil
}
func runAgent(path string) error {
	raw, e := os.ReadFile(path)
	if e != nil {
		return e
	}
	var c agentConfig
	if e = json.Unmarshal(raw, &c); e != nil {
		return e
	}
	if c.User == "" || c.Password == "" {
		return fmt.Errorf("proxy authentication must be enabled")
	}
	p, e := newProvider(c)
	if e != nil {
		return e
	}
	socks, e := socks5.New(&socks5.Config{Resolver: remoteResolver{}, Credentials: socks5.StaticCredentials{c.User: c.Password}, Dial: p.dial, Logger: log.New(&limitedSOCKSLog{}, "SOCKS5 ", log.LstdFlags)})
	if e != nil {
		return e
	}
	errors := make(chan error, 3)
	listener, e := net.Listen("tcp", c.SOCKS)
	if e != nil {
		return e
	}
	defer listener.Close()
	defer p.shutdown()
	go func() { errors <- socks.Serve(listener) }()
	go func() {
		errors <- (&http.Server{Addr: c.HTTP, Handler: httpProxy(c, p), ReadHeaderTimeout: 10 * time.Second, IdleTimeout: 60 * time.Second, MaxHeaderBytes: 64 * 1024}).ListenAndServe()
	}()
	mux := http.NewServeMux()
	mux.HandleFunc("/api/stats", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(p.stats())
	})
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		io.WriteString(w, `<!doctype html><meta charset="utf-8"><title>CloudTunnel</title><h2>CloudTunnel 无需CA的TCP代理</h2><p>费用数值包含活跃连接预留费用，并非云账单。</p><pre id="s"></pre><script>async function u(){try{s.textContent=JSON.stringify(await(await fetch('/api/stats')).json(),null,2)}catch(e){s.textContent=e}}u();setInterval(u,3000)</script>`)
	})
	go func() {
		errors <- (&http.Server{Addr: c.Dashboard, Handler: mux, ReadHeaderTimeout: 5 * time.Second}).ListenAndServe()
	}()
	log.Printf("TCP tunnel proxy started HTTP=%s SOCKS5=%s regions=%d CA=not-required", c.HTTP, c.SOCKS, len(c.Nodes))
	signals := make(chan os.Signal, 1)
	signal.Notify(signals, syscall.SIGTERM, syscall.SIGINT)
	defer signal.Stop(signals)
	select {
	case e := <-errors:
		return e
	case <-signals:
		return nil
	}
}

// Keep repetitive client errors from filling the disk while retaining samples.
type limitedSOCKSLog struct {
	mu                sync.Mutex
	start             time.Time
	count, suppressed int
}

func (w *limitedSOCKSLog) Write(b []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if time.Since(w.start) > time.Minute {
		if w.suppressed > 0 {
			fmt.Fprintf(os.Stderr, "SOCKS5 repeated_errors_suppressed=%d in_previous_minute\n", w.suppressed)
		}
		w.start = time.Now()
		w.count = 0
		w.suppressed = 0
	}
	w.count++
	if w.count > 20 {
		w.suppressed++
		return len(b), nil
	}
	return os.Stderr.Write(b)
}
