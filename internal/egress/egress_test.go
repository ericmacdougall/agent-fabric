package egress

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/netip"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

type fakeResolver struct {
	mu    sync.Mutex
	ips   []netip.Addr
	err   error
	calls int
}

func (r *fakeResolver) LookupNetIP(_ context.Context, network, host string) ([]netip.Addr, error) {
	if network != "ip" || host != "api.anthropic.com" && host != "claude.ai" && host != "console.anthropic.com" {
		return nil, fmt.Errorf("unexpected lookup %q %q", network, host)
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	r.calls++
	return append([]netip.Addr(nil), r.ips...), r.err
}

func (r *fakeResolver) set(ips ...netip.Addr) {
	r.mu.Lock()
	r.ips = append([]netip.Addr(nil), ips...)
	r.mu.Unlock()
}

func (r *fakeResolver) count() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.calls
}

type fakeTarget struct {
	ln       net.Listener
	accepted atomic.Int64
	mu       sync.Mutex
	conns    map[net.Conn]struct{}
	closed   chan struct{}
	wg       sync.WaitGroup
}

func newFakeTarget(t *testing.T) *fakeTarget {
	t.Helper()
	ln, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	f := &fakeTarget{ln: ln, conns: make(map[net.Conn]struct{}), closed: make(chan struct{})}
	f.wg.Add(1)
	go f.serve()
	t.Cleanup(f.close)
	return f
}

func (f *fakeTarget) serve() {
	defer f.wg.Done()
	for {
		c, err := f.ln.Accept()
		if err != nil {
			return
		}
		f.accepted.Add(1)
		f.mu.Lock()
		f.conns[c] = struct{}{}
		f.mu.Unlock()
		f.wg.Add(1)
		go func() {
			defer f.wg.Done()
			defer c.Close()
			defer func() {
				f.mu.Lock()
				delete(f.conns, c)
				f.mu.Unlock()
			}()
			buf := make([]byte, 4096)
			for {
				n, err := c.Read(buf)
				if n > 0 {
					if _, writeErr := c.Write(buf[:n]); writeErr != nil {
						return
					}
				}
				if err != nil {
					return
				}
			}
		}()
	}
}

func (f *fakeTarget) close() {
	select {
	case <-f.closed:
		return
	default:
		close(f.closed)
	}
	f.ln.Close()
	f.mu.Lock()
	for c := range f.conns {
		c.Close()
	}
	f.mu.Unlock()
	f.wg.Wait()
}

type dialRecord struct {
	mu        sync.Mutex
	addresses []string
	targets   map[string]*fakeTarget
}

func (d *dialRecord) dial(ctx context.Context, network, address string) (net.Conn, error) {
	d.mu.Lock()
	d.addresses = append(d.addresses, address)
	target := d.targets[address]
	d.mu.Unlock()
	if target == nil {
		return nil, fmt.Errorf("refused unexpected dial %s", address)
	}
	return (&net.Dialer{}).DialContext(ctx, network, target.ln.Addr().String())
}

func (d *dialRecord) count() int {
	d.mu.Lock()
	defer d.mu.Unlock()
	return len(d.addresses)
}

func (d *dialRecord) all() []string {
	d.mu.Lock()
	defer d.mu.Unlock()
	return append([]string(nil), d.addresses...)
}

func testConfig(r *fakeResolver, d *dialRecord) Config {
	return Config{
		Resolver: r, AllowLoopbackResolution: true,
		dialContext: d.dial,
	}
}

func startTestMediator(t *testing.T, cfg Config) *Mediator {
	t.Helper()
	m, err := Start(context.Background(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { m.Close() })
	return m
}

func proxyConn(t *testing.T, m *Mediator) net.Conn {
	t.Helper()
	c, err := net.Dial("tcp", strings.TrimPrefix(m.URL(), "http://"))
	if err != nil {
		t.Fatal(err)
	}
	c.SetDeadline(time.Now().Add(2 * time.Second))
	t.Cleanup(func() { c.Close() })
	return c
}

func established(t *testing.T, m *Mediator, authority string) (net.Conn, *bufio.Reader) {
	t.Helper()
	c := proxyConn(t, m)
	fmt.Fprintf(c, "CONNECT %s HTTP/1.1\r\nHost: %s\r\n\r\n", authority, authority)
	br := bufio.NewReader(c)
	resp, err := http.ReadResponse(br, &http.Request{Method: http.MethodConnect})
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("CONNECT status = %d", resp.StatusCode)
	}
	return c, br
}

func echo(t *testing.T, c net.Conn, br *bufio.Reader, message string) {
	t.Helper()
	if _, err := io.WriteString(c, message); err != nil {
		t.Fatal(err)
	}
	got := make([]byte, len(message))
	if _, err := io.ReadFull(br, got); err != nil {
		t.Fatal(err)
	}
	if string(got) != message {
		t.Fatalf("echo = %q, want %q", got, message)
	}
}

func TestAllowedDestinationUsesPinnedResolution(t *testing.T) {
	first := newFakeTarget(t)
	rebound := newFakeTarget(t)
	ip1 := netip.MustParseAddr("127.0.0.1")
	ip2 := netip.MustParseAddr("127.0.0.2")
	r := &fakeResolver{ips: []netip.Addr{ip1}}
	d := &dialRecord{targets: map[string]*fakeTarget{
		"127.0.0.1:443": first,
		"127.0.0.2:443": rebound,
	}}
	m := startTestMediator(t, testConfig(r, d))
	r.set(ip2) // A later answer must not migrate this run.

	for i := 0; i < 2; i++ {
		c, br := established(t, m, DefaultDestination)
		echo(t, c, br, "pinned")
		c.Close()
	}
	if got := r.count(); got != 1 {
		t.Fatalf("DNS lookups = %d, want one per run", got)
	}
	if got := d.all(); len(got) != 2 || got[0] != "127.0.0.1:443" || got[1] != "127.0.0.1:443" {
		t.Fatalf("outbound dials = %v", got)
	}
	if first.accepted.Load() != 2 || rebound.accepted.Load() != 0 {
		t.Fatalf("target connections: first=%d rebound=%d", first.accepted.Load(), rebound.accepted.Load())
	}
}

func TestHostileRequestsDeniedWithoutOutboundDial(t *testing.T) {
	target := newFakeTarget(t)
	r := &fakeResolver{ips: []netip.Addr{netip.MustParseAddr("127.0.0.1")}}
	d := &dialRecord{targets: map[string]*fakeTarget{"127.0.0.1:443": target}}
	m := startTestMediator(t, testConfig(r, d))

	requests := []string{
		"GET http://api.anthropic.com/v1/messages HTTP/1.1\r\nHost: api.anthropic.com\r\n\r\n",
		"POST api.anthropic.com:443 HTTP/1.1\r\nHost: api.anthropic.com:443\r\n\r\n",
		"CONNECT evil.example:443 HTTP/1.1\r\nHost: evil.example:443\r\n\r\n",
		"CONNECT api.anthropic.com:444 HTTP/1.1\r\nHost: api.anthropic.com:444\r\n\r\n",
		"CONNECT 127.0.0.1:443 HTTP/1.1\r\nHost: 127.0.0.1:443\r\n\r\n",
		"CONNECT [::1]:443 HTTP/1.1\r\nHost: [::1]:443\r\n\r\n",
		"CONNECT user@api.anthropic.com:443 HTTP/1.1\r\nHost: api.anthropic.com:443\r\n\r\n",
		"CONNECT http://api.anthropic.com:443/ HTTP/1.1\r\nHost: api.anthropic.com:443\r\n\r\n",
		"CONNECT api.anthropic.com.:443 HTTP/1.1\r\nHost: api.anthropic.com.:443\r\n\r\n",
		"CONNECT claude.ai:443 HTTP/1.1\r\nHost: claude.ai:443\r\n\r\n",
		"CONNECT api.anthropic.com:443 HTTP/1.0\r\nHost: api.anthropic.com:443\r\n\r\n",
		"CONNECT api.anthropic.com:443 HTTP/1.1\r\nHost: api.anthropic.com:443\r\nContent-Length: 0\r\n\r\n",
		"CONNECT api.anthropic.com:443 HTTP/1.1\r\nHost: api.anthropic.com:443\r\nTransfer-Encoding: chunked\r\n\r\n",
	}
	for _, request := range requests {
		c := proxyConn(t, m)
		io.WriteString(c, request)
		response, err := io.ReadAll(io.LimitReader(c, 4096))
		c.Close()
		if err != nil {
			t.Fatal(err)
		}
		if strings.Contains(string(response), " 200 ") || !strings.HasPrefix(string(response), "HTTP/1.1 ") {
			t.Fatalf("hostile request was not refused: %q => %q", request, response)
		}
	}
	if d.count() != 0 || target.accepted.Load() != 0 {
		t.Fatalf("hostile requests caused outbound connections: dials=%d target=%d", d.count(), target.accepted.Load())
	}
}

func TestEstablishedTunnelCannotMigrate(t *testing.T) {
	target := newFakeTarget(t)
	r := &fakeResolver{ips: []netip.Addr{netip.MustParseAddr("127.0.0.1")}}
	d := &dialRecord{targets: map[string]*fakeTarget{"127.0.0.1:443": target}}
	m := startTestMediator(t, testConfig(r, d))
	c, br := established(t, m, DefaultDestination)

	// Bytes after the 200 response belong only to the selected TCP tunnel. The
	// mediator does not interpret redirects or another CONNECT as migration.
	migration := "CONNECT evil.example:443 HTTP/1.1\r\nHost: evil.example:443\r\n\r\n"
	echo(t, c, br, migration)
	if d.count() != 1 || target.accepted.Load() != 1 {
		t.Fatalf("tunnel migrated: dials=%d targets=%d", d.count(), target.accepted.Load())
	}
}

func TestHeaderAndConnectionBounds(t *testing.T) {
	t.Run("header", func(t *testing.T) {
		target := newFakeTarget(t)
		r := &fakeResolver{ips: []netip.Addr{netip.MustParseAddr("127.0.0.1")}}
		d := &dialRecord{targets: map[string]*fakeTarget{"127.0.0.1:443": target}}
		cfg := testConfig(r, d)
		cfg.Limits.HeaderBytes = 256
		m := startTestMediator(t, cfg)
		c := proxyConn(t, m)
		fmt.Fprintf(c, "CONNECT %s HTTP/1.1\r\nHost: %s\r\nX-Filler: %s\r\n\r\n", DefaultDestination, DefaultDestination, strings.Repeat("x", 512))
		response, err := io.ReadAll(io.LimitReader(c, 4096))
		if err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(string(response), "431") || d.count() != 0 {
			t.Fatalf("oversized header response=%q dials=%d", response, d.count())
		}
	})

	t.Run("connections", func(t *testing.T) {
		target := newFakeTarget(t)
		r := &fakeResolver{ips: []netip.Addr{netip.MustParseAddr("127.0.0.1")}}
		d := &dialRecord{targets: map[string]*fakeTarget{"127.0.0.1:443": target}}
		cfg := testConfig(r, d)
		cfg.Limits.Connections = 1
		m := startTestMediator(t, cfg)
		first, firstReader := established(t, m, DefaultDestination)
		second := proxyConn(t, m)
		fmt.Fprintf(second, "CONNECT %s HTTP/1.1\r\nHost: %s\r\n\r\n", DefaultDestination, DefaultDestination)
		response, err := io.ReadAll(io.LimitReader(second, 4096))
		if err != nil && !strings.Contains(string(response), "503") {
			t.Fatal(err)
		}
		if !strings.Contains(string(response), "503") || d.count() != 1 {
			t.Fatalf("excess connection response=%q dials=%d", response, d.count())
		}
		echo(t, first, firstReader, "still-open")
	})
}

func TestTunnelTimeAndByteBounds(t *testing.T) {
	newBoundMediator := func(t *testing.T, limits Limits) (*Mediator, *dialRecord) {
		target := newFakeTarget(t)
		r := &fakeResolver{ips: []netip.Addr{netip.MustParseAddr("127.0.0.1")}}
		d := &dialRecord{targets: map[string]*fakeTarget{"127.0.0.1:443": target}}
		cfg := testConfig(r, d)
		cfg.Limits = limits
		return startTestMediator(t, cfg), d
	}

	t.Run("idle", func(t *testing.T) {
		m, _ := newBoundMediator(t, Limits{IdleTimeout: 80 * time.Millisecond})
		_, br := established(t, m, DefaultDestination)
		start := time.Now()
		if _, err := br.ReadByte(); err == nil {
			t.Fatal("idle tunnel remained open")
		}
		if elapsed := time.Since(start); elapsed > time.Second {
			t.Fatalf("idle close took %v", elapsed)
		}
	})

	t.Run("total", func(t *testing.T) {
		m, _ := newBoundMediator(t, Limits{IdleTimeout: 5 * time.Second, TotalTimeout: 120 * time.Millisecond})
		_, br := established(t, m, DefaultDestination)
		start := time.Now()
		if _, err := br.ReadByte(); err == nil {
			t.Fatal("total timeout did not close tunnel")
		}
		if elapsed := time.Since(start); elapsed > time.Second {
			t.Fatalf("total close took %v", elapsed)
		}
	})

	t.Run("aggregate bytes", func(t *testing.T) {
		m, _ := newBoundMediator(t, Limits{MaxBytes: 6})
		c, br := established(t, m, DefaultDestination)
		io.WriteString(c, "abcd")
		got, err := io.ReadAll(io.LimitReader(br, 32))
		if err != nil {
			t.Fatal(err)
		}
		// Four bytes toward the target leave two bytes for its echo.
		if string(got) != "ab" {
			t.Fatalf("relayed %q (%d bytes), aggregate limit is six", got, len(got))
		}
	})
}

func TestCloseKillsTunnelsAndListener(t *testing.T) {
	target := newFakeTarget(t)
	r := &fakeResolver{ips: []netip.Addr{netip.MustParseAddr("127.0.0.1")}}
	d := &dialRecord{targets: map[string]*fakeTarget{"127.0.0.1:443": target}}
	m := startTestMediator(t, testConfig(r, d))
	c, br := established(t, m, DefaultDestination)
	echo(t, c, br, "before-close")
	if err := m.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := br.ReadByte(); err == nil {
		t.Fatal("Close left a tunnel readable")
	}
	if c, err := net.DialTimeout("tcp", strings.TrimPrefix(m.URL(), "http://"), 100*time.Millisecond); err == nil {
		c.Close()
		t.Fatal("Close left listener reachable")
	}
	if err := m.Close(); err != nil {
		t.Fatal(err)
	}
}

func TestEnvironmentAndFirewallDescriptor(t *testing.T) {
	target := newFakeTarget(t)
	r := &fakeResolver{ips: []netip.Addr{netip.MustParseAddr("127.0.0.1")}}
	d := &dialRecord{targets: map[string]*fakeTarget{"127.0.0.1:443": target}}
	m := startTestMediator(t, testConfig(r, d))
	u, err := url.Parse(m.URL())
	if err != nil || u.Scheme != "http" || u.Hostname() != "127.0.0.1" || u.User != nil {
		t.Fatalf("proxy URL = %q, user=%v err=%v", m.URL(), u.User, err)
	}
	port, err := strconv.ParseUint(u.Port(), 10, 16)
	if err != nil || port == 0 {
		t.Fatalf("proxy port = %q", u.Port())
	}
	env := m.Env()
	if len(env) != 8 {
		t.Fatalf("environment has %d entries", len(env))
	}
	values := make(map[string]string)
	for _, entry := range env {
		name, value, ok := strings.Cut(entry, "=")
		if !ok {
			t.Fatalf("bad environment entry %q", entry)
		}
		values[name] = value
	}
	for _, name := range []string{"HTTP_PROXY", "HTTPS_PROXY", "ALL_PROXY", "http_proxy", "https_proxy", "all_proxy"} {
		if values[name] != m.URL() {
			t.Errorf("%s = %q", name, values[name])
		}
	}
	if values["NO_PROXY"] != "" || values["no_proxy"] != "" {
		t.Fatal("proxy bypass list is not empty")
	}

	p := m.Policy()
	if !p.DefaultDeny || p.ProxyURL != m.URL() || len(p.WorkerRules) != 1 || len(p.MediatorRules) != 1 {
		t.Fatalf("policy = %+v", p)
	}
	worker := p.WorkerRules[0]
	if worker.Address != "127.0.0.1" || worker.Port != uint16(port) || worker.Protocol != "tcp" || worker.Action != "allow" {
		t.Fatalf("worker rule = %+v", worker)
	}
	remote := p.MediatorRules[0]
	if remote.Address != "127.0.0.1" || remote.Port != 443 || remote.Host != "api.anthropic.com" {
		t.Fatalf("mediator rule = %+v", remote)
	}
	p.WorkerRules[0].Address = "changed"
	if m.Policy().WorkerRules[0].Address != "127.0.0.1" {
		t.Fatal("Policy returned mutable internal state")
	}
}

func TestExplicitLoginEndpointsAndFailClosedStart(t *testing.T) {
	ip := netip.MustParseAddr("127.0.0.1")
	t.Run("login endpoints are opt in", func(t *testing.T) {
		r := &fakeResolver{ips: []netip.Addr{ip}}
		d := &dialRecord{}
		cfg := testConfig(r, d)
		cfg.AdditionalDestinations = []string{"claude.ai:443", "console.anthropic.com:443"}
		m := startTestMediator(t, cfg)
		if len(m.Policy().MediatorRules) != 3 || r.count() != 3 {
			t.Fatalf("rules=%d lookups=%d", len(m.Policy().MediatorRules), r.count())
		}
	})

	t.Run("arbitrary endpoint", func(t *testing.T) {
		r := &fakeResolver{ips: []netip.Addr{ip}}
		cfg := testConfig(r, &dialRecord{})
		cfg.AdditionalDestinations = []string{"evil.example:443"}
		if m, err := Start(context.Background(), cfg); err == nil || m != nil {
			t.Fatalf("arbitrary endpoint started: mediator=%v err=%v", m, err)
		}
		if r.count() != 0 {
			t.Fatal("invalid allow-list caused DNS")
		}
	})

	t.Run("DNS error", func(t *testing.T) {
		r := &fakeResolver{err: errors.New("injected DNS failure")}
		cfg := testConfig(r, &dialRecord{})
		if m, err := Start(context.Background(), cfg); err == nil || m != nil {
			t.Fatalf("DNS failure started: mediator=%v err=%v", m, err)
		}
	})

	t.Run("loopback answer requires isolated test opt in", func(t *testing.T) {
		r := &fakeResolver{ips: []netip.Addr{ip}}
		cfg := testConfig(r, &dialRecord{})
		cfg.AllowLoopbackResolution = false
		if m, err := Start(context.Background(), cfg); err == nil || m != nil {
			t.Fatalf("loopback DNS answer started: mediator=%v err=%v", m, err)
		}
	})

	t.Run("listener error", func(t *testing.T) {
		r := &fakeResolver{ips: []netip.Addr{ip}}
		cfg := testConfig(r, &dialRecord{})
		cfg.listenContext = func(context.Context, string, string) (net.Listener, error) {
			return nil, errors.New("injected listener failure")
		}
		if m, err := Start(context.Background(), cfg); err == nil || m != nil {
			t.Fatalf("listener failure started: mediator=%v err=%v", m, err)
		}
	})
}
