package main

import (
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	"github.com/armon/go-socks5"
	xproxy "golang.org/x/net/proxy"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"
)

func setupTest(t *testing.T) (agentConfig, *provider) {
	t.Helper()
	node := httptest.NewServer(nodeHandler("secret", true))
	t.Cleanup(node.Close)
	c := agentConfig{User: "user", Password: "password", Token: "secret", Nodes: []nodeConfig{{Region: "test", URL: "ws" + node.URL[4:] + "/tunnel"}}, Ledger: filepath.Join(t.TempDir(), "usage.json"), Budget: 8}
	p, e := newProvider(c)
	if e != nil {
		t.Fatal(e)
	}
	p.allowPrivate = true
	return c, p
}
func TestHTTPSCertificateAndConnectionReuse(t *testing.T) {
	c, p := setupTest(t)
	var connections atomic.Int64
	target := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { io.WriteString(w, "unchanged TLS") }))
	target.Config.ConnState = func(_ net.Conn, s http.ConnState) {
		if s == http.StateNew {
			connections.Add(1)
		}
	}
	target.StartTLS()
	defer target.Close()
	roots := x509.NewCertPool()
	roots.AddCert(target.Certificate())
	server := httptest.NewServer(httpProxy(c, p))
	defer server.Close()
	proxyURL, _ := url.Parse(server.URL)
	proxyURL.User = url.UserPassword(c.User, c.Password)
	transport := &http.Transport{Proxy: http.ProxyURL(proxyURL), TLSClientConfig: &tls.Config{RootCAs: roots}}
	defer transport.CloseIdleConnections()
	client := &http.Client{Transport: transport, Timeout: 10 * time.Second}
	for i := 0; i < 3; i++ {
		r, e := client.Get(target.URL)
		if e != nil {
			t.Fatal(e)
		}
		body, e := io.ReadAll(r.Body)
		r.Body.Close()
		if e != nil || string(body) != "unchanged TLS" {
			t.Fatal("damaged response", e)
		}
		if !r.TLS.PeerCertificates[0].Equal(target.Certificate()) {
			t.Fatal("target certificate replaced")
		}
	}
	if connections.Load() != 1 {
		t.Fatalf("TLS connection was not reused: %d", connections.Load())
	}
	p.mu.Lock()
	opened := p.nodes[0].Opens
	p.mu.Unlock()
	if opened != 1 {
		t.Fatalf("expected one cloud tunnel, got %d", opened)
	}
	transport.CloseIdleConnections()
}
func TestSOCKS5TLSPassthrough(t *testing.T) {
	c, p := setupTest(t)
	target := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { io.WriteString(w, "socks TLS") }))
	defer target.Close()
	roots := x509.NewCertPool()
	roots.AddCert(target.Certificate())
	listener, e := net.Listen("tcp", "127.0.0.1:0")
	if e != nil {
		t.Fatal(e)
	}
	defer listener.Close()
	socks, e := socks5.New(&socks5.Config{Resolver: remoteResolver{}, Credentials: socks5.StaticCredentials{c.User: c.Password}, Dial: p.dial})
	if e != nil {
		t.Fatal(e)
	}
	go socks.Serve(listener)
	dialer, e := xproxy.SOCKS5("tcp", listener.Addr().String(), &xproxy.Auth{User: c.User, Password: c.Password}, xproxy.Direct)
	if e != nil {
		t.Fatal(e)
	}
	transport := &http.Transport{DialContext: func(ctx context.Context, n, a string) (net.Conn, error) {
		return dialer.(xproxy.ContextDialer).DialContext(ctx, n, a)
	}, TLSClientConfig: &tls.Config{RootCAs: roots}}
	defer transport.CloseIdleConnections()
	client := &http.Client{Transport: transport, Timeout: 10 * time.Second}
	r, e := client.Get(target.URL)
	if e != nil {
		t.Fatal(e)
	}
	b, e := io.ReadAll(r.Body)
	r.Body.Close()
	if e != nil || string(b) != "socks TLS" {
		t.Fatal("bad TLS response", e)
	}
	if !r.TLS.PeerCertificates[0].Equal(target.Certificate()) {
		t.Fatal("certificate replaced")
	}
}
func TestAuthAndPrivateTargets(t *testing.T) {
	c, p := setupTest(t)
	proxy := httptest.NewServer(httpProxy(c, p))
	defer proxy.Close()
	u, _ := url.Parse(proxy.URL)
	client := &http.Client{Transport: &http.Transport{Proxy: http.ProxyURL(u)}}
	r, e := client.Get("http://example.invalid/")
	if e != nil {
		t.Fatal(e)
	}
	r.Body.Close()
	if r.StatusCode != 407 {
		t.Fatal("unauthorized proxy access")
	}
	if conn, e := dialTarget(context.Background(), "127.0.0.1:22", false); e == nil {
		conn.Close()
		t.Fatal("private target allowed")
	}
}
func TestMeterReservesActiveSessionsAndPersists(t *testing.T) {
	path := filepath.Join(t.TempDir(), "usage.json")
	m, e := openMeter(path, 0.0013)
	if e != nil {
		t.Fatal(e)
	}
	finish, e := m.reserve()
	if e != nil {
		t.Fatal(e)
	}
	if _, e = m.reserve(); e == nil {
		t.Fatal("active reservations bypassed budget")
	}
	if e = finish(time.Second, 1024); e != nil {
		t.Fatal(e)
	}
	restored, e := openMeter(path, 0.0013)
	if e != nil {
		t.Fatal(e)
	}
	if restored.snapshot().Calls != 1 {
		t.Fatal("lost ledger")
	}
	if _, e = restored.reserve(); e != nil {
		t.Fatal("reservation was not reconciled")
	}
}

func TestBinaryTunnelAndHalfClose(t *testing.T) {
	_, p := setupTest(t)
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	go func() {
		c, e := listener.Accept()
		if e != nil {
			return
		}
		defer c.Close()
		io.Copy(c, c)
	}()
	c, err := p.dial(context.Background(), "tcp", listener.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	data := bytes.Repeat([]byte{0, 1, 255, 127}, 13000)
	go func() { c.Write(data); c.(*wsStream).CloseWrite() }()
	received, err := io.ReadAll(c)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(data, received) {
		t.Fatalf("binary data lost: %d/%d", len(received), len(data))
	}
}
func TestBudgetCarriesActiveConnectionAcrossMonth(t *testing.T) {
	m, err := openMeter(filepath.Join(t.TempDir(), "usage.json"), 8)
	if err != nil {
		t.Fatal(err)
	}
	finish, err := m.reserve()
	if err != nil {
		t.Fatal(err)
	}
	reserved := m.snapshot().Estimated
	m.mu.Lock()
	m.state.Month = "2000-01"
	m.mu.Unlock()
	if got := m.snapshot().Estimated; got != reserved {
		t.Fatalf("active reservation lost at month boundary: %v", got)
	}
	if err = finish(time.Second, 100); err != nil {
		t.Fatal(err)
	}
	if m.snapshot().Estimated >= reserved {
		t.Fatal("reservation not settled")
	}
	settled := m.snapshot().Estimated
	finish(time.Second, 100)
	if m.snapshot().Estimated != settled {
		t.Fatal("reservation settled twice")
	}
}

func TestUnreachableTargetDoesNotDisableNode(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { http.Error(w, "Target connection failed", 502) }))
	defer server.Close()
	c := agentConfig{Token: "secret", Nodes: []nodeConfig{{Region: "test", URL: "ws" + server.URL[4:]}}, Ledger: filepath.Join(t.TempDir(), "usage.json"), Budget: 8}
	p, err := newProvider(c)
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 5; i++ {
		if conn, err := p.dial(context.Background(), "tcp", "unreachable.example:443"); err == nil {
			conn.Close()
			t.Fatal("unreachable target unexpectedly connected")
		}
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.nodes[0].Failures != 0 || p.nodes[0].TargetFailures < 1 {
		t.Fatalf("target failures incorrectly cooled node: %+v", p.nodes[0])
	}
}

func TestTargetErrorOverWebSocketDoesNotCoolNode(t *testing.T) {
	_, p := setupTest(t)
	for i := 0; i < 4; i++ {
		conn, err := p.dial(context.Background(), "tcp", "diagnostic.invalid:443")
		if err == nil {
			conn.Close()
			t.Fatal("invalid target connected")
		}
		if _, ok := err.(*targetDialError); ok {
			t.Fatal("expected error with region context")
		}
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.nodes[0].TargetFailures < 1 || p.nodes[0].Failures != 0 {
		t.Fatalf("target error was confused with node failure: %+v", p.nodes[0])
	}
}

func TestGracefulShutdownSettlesActiveTunnel(t *testing.T) {
	_, p := setupTest(t)
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	go func() {
		c, e := listener.Accept()
		if e != nil {
			return
		}
		defer c.Close()
		io.Copy(io.Discard, c)
	}()
	connection, err := p.dial(context.Background(), "tcp", listener.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	defer connection.Close()
	reserved := p.meter.snapshot().Estimated
	p.shutdown()
	if p.active.Load() != 0 || p.meter.snapshot().Estimated >= reserved {
		t.Fatal("active reservation was not settled during shutdown")
	}
	if c, e := p.dial(context.Background(), "tcp", listener.Addr().String()); e == nil {
		c.Close()
		t.Fatal("new connections accepted after shutdown")
	}
}

func TestPrivateTargetRejectedBeforeCloudInvocation(t *testing.T) {
	_, p := setupTest(t)
	p.allowPrivate = false
	before := p.meter.snapshot().Calls
	if conn, err := p.dial(context.Background(), "tcp", "192.168.1.10:443"); err == nil {
		conn.Close()
		t.Fatal("private target allowed")
	}
	if p.meter.snapshot().Calls != before || p.rejectedPrivate.Load() != 1 {
		t.Fatal("private target consumed a cloud invocation")
	}
}

func TestRollingBudgetAllowsBrowserConcurrency(t *testing.T) {
	m, err := openMeter(filepath.Join(t.TempDir(), "usage.json"), 8)
	if err != nil {
		t.Fatal(err)
	}
	var charges []*budgetLease
	for i := 0; i < 32; i++ {
		c, e := m.begin()
		if e != nil {
			t.Fatal(e)
		}
		charges = append(charges, c)
	}
	if m.snapshot().Estimated > 0.1 {
		t.Fatal("browser connections reserved excessive budget")
	}
	for _, c := range charges {
		if e := c.progress(20*time.Second, 128*1024); e != nil {
			t.Fatal(e)
		}
		c.finish(20*time.Second, 128*1024)
	}
}
func TestRollingBudgetClosesBeforeReserveExhausted(t *testing.T) {
	m, err := openMeter(filepath.Join(t.TempDir(), "usage.json"), 0.0012)
	if err != nil {
		t.Fatal(err)
	}
	c, err := m.begin()
	if err != nil {
		t.Fatal(err)
	}
	before := m.snapshot().Estimated
	if err = c.progress(20*time.Second, 128*1024); err == nil {
		t.Fatal("rolling budget allowed spending without reserve")
	}
	if m.snapshot().Estimated != before {
		t.Fatal("failed reserve changed ledger")
	}
	if estimatedCost(20*time.Second, 128*1024) > c.held {
		t.Fatal("budget rejected too late")
	}
	c.finish(20*time.Second, 128*1024)
}

func TestUnreachableTargetBackoffPreservesSlotsAndBudget(t *testing.T) {
	_, p := setupTest(t)
	p.mu.Lock()
	p.targetCooldown["offline.invalid:443"] = time.Now().Add(time.Second)
	p.mu.Unlock()
	calls := p.meter.snapshot().Calls
	if conn, e := p.dial(context.Background(), "tcp", "offline.invalid:443"); e == nil {
		conn.Close()
		t.Fatal("failed target backoff bypassed")
	}
	if len(p.slots) != 0 || p.meter.snapshot().Calls != calls || p.cachedTargetFailures.Load() != 1 {
		t.Fatal("cached failure consumed connection or cloud budget")
	}
}
