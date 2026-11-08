// Package egress provides a per-run, provider-only CONNECT mediator.
package egress

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/netip"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

const (
	// DefaultDestination is the only destination enabled by default.
	DefaultDestination = "api.anthropic.com:443"

	defaultHeaderBytes  = 16 << 10
	defaultConnections  = 16
	defaultIdleTimeout  = 30 * time.Second
	defaultTotalTimeout = 15 * time.Minute
	defaultMaxBytes     = 64 << 20
	defaultResolveTime  = 10 * time.Second

	maxHeaderBytes  = 64 << 10
	maxConnections  = 128
	maxIdleTimeout  = 5 * time.Minute
	maxTotalTimeout = time.Hour
	maxTunnelBytes  = 1 << 30
	maxResolveTime  = time.Minute
)

// officialLoginDestinations is deliberately closed. Adding an endpoint is a
// policy change, not worker-controlled configuration.
var officialLoginDestinations = map[string]bool{
	"claude.ai:443":             true,
	"console.anthropic.com:443": true,
}

// Resolver is the small part of net.Resolver used by the mediator.
type Resolver interface {
	LookupNetIP(ctx context.Context, network, host string) ([]netip.Addr, error)
}

// Limits bounds every accepted connection and every established tunnel. Zero
// values select defaults. Values above the package ceilings are refused rather
// than silently weakened.
type Limits struct {
	HeaderBytes  int
	Connections  int
	IdleTimeout  time.Duration
	TotalTimeout time.Duration
	MaxBytes     int64
}

// Config configures one run. AdditionalDestinations may contain only the
// official Claude login endpoints compiled into this package; arbitrary hosts
// and ports are rejected before DNS resolution or listening.
type Config struct {
	AdditionalDestinations []string
	Resolver               Resolver
	ResolveTimeout         time.Duration
	Limits                 Limits

	// AllowLoopbackResolution exists for isolated tests with a fake target. It
	// does not allow clients to CONNECT to a loopback hostname or IP literal.
	AllowLoopbackResolution bool

	dialContext   func(context.Context, string, string) (net.Conn, error)
	listenContext func(context.Context, string, string) (net.Listener, error)
}

type destination struct {
	host  string
	port  uint16
	addrs []netip.Addr
	next  atomic.Uint64
}

func (d *destination) addrPort() netip.AddrPort {
	i := (d.next.Add(1) - 1) % uint64(len(d.addrs))
	return netip.AddrPortFrom(d.addrs[i], d.port)
}

// Mediator owns all listeners and connections for one run.
type Mediator struct {
	url      string
	listener net.Listener
	dests    map[string]*destination
	limits   Limits
	dial     func(context.Context, string, string) (net.Conn, error)
	policy   PolicyDescriptor

	ctx    context.Context
	cancel context.CancelFunc
	done   chan struct{}

	closeOnce sync.Once
	mu        sync.Mutex
	closed    bool
	active    int
	conns     map[net.Conn]struct{}
	wg        sync.WaitGroup
}

// Start resolves and pins every enabled destination, then starts a loopback
// listener on an ephemeral port. Any policy, DNS, or listener error fails
// closed and returns no mediator.
func Start(ctx context.Context, cfg Config) (*Mediator, error) {
	if ctx == nil {
		return nil, errors.New("egress: nil context")
	}
	limits, err := checkLimits(cfg.Limits)
	if err != nil {
		return nil, err
	}
	resolveTimeout, err := boundedDuration("resolve timeout", cfg.ResolveTimeout, defaultResolveTime, maxResolveTime)
	if err != nil {
		return nil, err
	}

	names := []string{DefaultDestination}
	seen := map[string]bool{DefaultDestination: true}
	for _, name := range cfg.AdditionalDestinations {
		key, _, _, parseErr := parseDestination(name)
		if parseErr != nil || !officialLoginDestinations[key] {
			return nil, fmt.Errorf("egress: destination is not an explicitly supported Claude login endpoint")
		}
		if !seen[key] {
			seen[key] = true
			names = append(names, key)
		}
	}

	resolver := cfg.Resolver
	if resolver == nil {
		resolver = net.DefaultResolver
	}
	resolveCtx, cancelResolve := context.WithTimeout(ctx, resolveTimeout)
	defer cancelResolve()
	dests := make(map[string]*destination, len(names))
	for _, name := range names {
		key, host, port, _ := parseDestination(name)
		ips, lookupErr := resolver.LookupNetIP(resolveCtx, "ip", host)
		if lookupErr != nil {
			return nil, fmt.Errorf("egress: resolve allowed destination: %w", lookupErr)
		}
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		pinned, pinErr := pinAddresses(ips, cfg.AllowLoopbackResolution)
		if pinErr != nil {
			return nil, fmt.Errorf("egress: resolve allowed destination: %w", pinErr)
		}
		dests[key] = &destination{host: host, port: port, addrs: pinned}
	}

	listen := cfg.listenContext
	if listen == nil {
		var lc net.ListenConfig
		listen = lc.Listen
	}
	ln, err := listen(ctx, "tcp", "127.0.0.1:0")
	if err != nil {
		return nil, fmt.Errorf("egress: listen on loopback: %w", err)
	}
	tcpAddr, ok := ln.Addr().(*net.TCPAddr)
	if !ok || !tcpAddr.IP.IsLoopback() || tcpAddr.Port == 0 || !strings.HasPrefix(ln.Addr().String(), "127.0.0.1:") {
		ln.Close()
		return nil, errors.New("egress: listener is not 127.0.0.1 on an ephemeral port")
	}
	if err := ctx.Err(); err != nil {
		ln.Close()
		return nil, err
	}

	runCtx, cancel := context.WithCancel(ctx)
	dial := cfg.dialContext
	if dial == nil {
		nd := &net.Dialer{Timeout: 10 * time.Second, KeepAlive: 30 * time.Second}
		dial = nd.DialContext
	}
	m := &Mediator{
		url:      "http://" + ln.Addr().String(),
		listener: ln,
		dests:    dests,
		limits:   limits,
		dial:     dial,
		ctx:      runCtx,
		cancel:   cancel,
		done:     make(chan struct{}),
		conns:    make(map[net.Conn]struct{}),
	}
	m.policy = makePolicy(m.url, dests)
	m.wg.Add(1)
	go m.serve()
	go func() {
		select {
		case <-runCtx.Done():
			m.Close()
		case <-m.done:
		}
	}()
	return m, nil
}

// URL is the per-run HTTP CONNECT proxy URL.
func (m *Mediator) URL() string { return m.url }

// Env returns a fresh set of proxy variables for a run. NO_PROXY is explicitly
// empty. Host sandbox policy must still deny direct worker egress because some
// applications special-case loopback proxies or ignore proxy environment.
func (m *Mediator) Env() []string {
	return []string{
		"HTTP_PROXY=" + m.url,
		"HTTPS_PROXY=" + m.url,
		"ALL_PROXY=" + m.url,
		"NO_PROXY=",
		"http_proxy=" + m.url,
		"https_proxy=" + m.url,
		"all_proxy=" + m.url,
		"no_proxy=",
	}
}

// Policy returns a deep copy of the host-independent, default-deny firewall
// descriptor for this run.
func (m *Mediator) Policy() PolicyDescriptor { return clonePolicy(m.policy) }

// Close closes the listener and every accepted or outbound connection and
// waits for their handlers. It is idempotent.
func (m *Mediator) Close() error {
	m.closeOnce.Do(func() {
		m.mu.Lock()
		m.closed = true
		m.cancel()
		m.listener.Close()
		for c := range m.conns {
			c.Close()
		}
		m.mu.Unlock()
		m.wg.Wait()
		close(m.done)
	})
	return nil
}

func (m *Mediator) serve() {
	defer m.wg.Done()
	var delay time.Duration
	for {
		c, err := m.listener.Accept()
		if err != nil {
			m.mu.Lock()
			closed := m.closed
			m.mu.Unlock()
			if closed || errors.Is(err, net.ErrClosed) {
				return
			}
			if ne, ok := err.(net.Error); ok && ne.Temporary() {
				if delay == 0 {
					delay = 5 * time.Millisecond
				} else {
					delay *= 2
				}
				if delay > time.Second {
					delay = time.Second
				}
				t := time.NewTimer(delay)
				select {
				case <-m.ctx.Done():
					t.Stop()
					return
				case <-t.C:
				}
				continue
			}
			// A permanent listener failure is a run failure, not a direct-egress
			// fallback and not a reason to leave existing tunnels alive.
			go m.Close()
			return
		}
		delay = 0
		m.mu.Lock()
		if m.closed {
			m.mu.Unlock()
			c.Close()
			return
		}
		if m.active >= m.limits.Connections {
			m.mu.Unlock()
			c.SetDeadline(time.Now().Add(time.Second))
			writeStatus(c, http.StatusServiceUnavailable)
			c.Close()
			continue
		}
		m.active++
		m.conns[c] = struct{}{}
		m.wg.Add(1)
		m.mu.Unlock()
		go m.handle(c)
	}
}

func (m *Mediator) handle(client net.Conn) {
	start := time.Now()
	totalDeadline := start.Add(m.limits.TotalTimeout)
	defer func() {
		client.Close()
		m.mu.Lock()
		delete(m.conns, client)
		m.active--
		m.mu.Unlock()
		m.wg.Done()
	}()
	client.SetDeadline(minTime(start.Add(m.limits.IdleTimeout), totalDeadline))

	br := bufio.NewReaderSize(client, 4096)
	raw, err := readHeader(br, m.limits.HeaderBytes)
	if err != nil {
		writeStatus(client, http.StatusRequestHeaderFieldsTooLarge)
		return
	}
	key, err := validateConnect(raw)
	if err != nil {
		writeStatus(client, http.StatusForbidden)
		return
	}
	d := m.dests[key]
	if d == nil {
		writeStatus(client, http.StatusForbidden)
		return
	}
	m.mu.Lock()
	closed := m.closed
	m.mu.Unlock()
	if closed {
		return
	}

	dialCtx, cancel := context.WithDeadline(m.ctx, totalDeadline)
	target, err := m.dial(dialCtx, "tcp", d.addrPort().String())
	cancel()
	if err != nil {
		writeStatus(client, http.StatusBadGateway)
		return
	}
	m.mu.Lock()
	if m.closed {
		m.mu.Unlock()
		target.Close()
		return
	}
	m.conns[target] = struct{}{}
	m.mu.Unlock()
	defer func() {
		target.Close()
		m.mu.Lock()
		delete(m.conns, target)
		m.mu.Unlock()
	}()
	target.SetDeadline(minTime(time.Now().Add(m.limits.IdleTimeout), totalDeadline))
	client.SetDeadline(minTime(time.Now().Add(m.limits.IdleTimeout), totalDeadline))
	if _, err := io.WriteString(client, "HTTP/1.1 200 Connection Established\r\n\r\n"); err != nil {
		return
	}
	m.runTunnel(client, target, br, totalDeadline)
}

func readHeader(br *bufio.Reader, max int) ([]byte, error) {
	var raw bytes.Buffer
	for lines := 0; ; lines++ {
		if lines > 64 {
			return nil, errors.New("too many headers")
		}
		var line []byte
		for {
			part, err := br.ReadSlice('\n')
			line = append(line, part...)
			if raw.Len()+len(line) > max {
				return nil, errors.New("header too large")
			}
			if errors.Is(err, bufio.ErrBufferFull) {
				continue
			}
			if err != nil {
				return nil, err
			}
			break
		}
		if !bytes.HasSuffix(line, []byte("\r\n")) {
			return nil, errors.New("non-CRLF header")
		}
		raw.Write(line)
		if len(line) == 2 {
			return raw.Bytes(), nil
		}
	}
}

func validateConnect(raw []byte) (string, error) {
	// Inspect the wire header as well as Request.Header: net/http consumes
	// framing fields such as Content-Length while parsing.
	lines := bytes.Split(raw, []byte("\r\n"))
	for _, line := range lines[1:] {
		if len(line) == 0 {
			break
		}
		name, _, ok := bytes.Cut(line, []byte(":"))
		if !ok {
			continue // ReadRequest below rejects the malformed line.
		}
		switch http.CanonicalHeaderKey(string(name)) {
		case "Content-Length", "Transfer-Encoding", "Expect", "Upgrade":
			return "", errors.New("CONNECT body or upgrade is forbidden")
		}
	}
	req, err := http.ReadRequest(bufio.NewReader(bytes.NewReader(raw)))
	if err != nil || req.Method != http.MethodConnect || req.Proto != "HTTP/1.1" {
		return "", errors.New("not HTTP/1.1 CONNECT")
	}
	authority := req.RequestURI
	if authority == "" || req.Host != authority || req.URL.Host != authority ||
		req.URL.Scheme != "" || req.URL.User != nil || req.URL.Path != "" ||
		req.URL.RawQuery != "" || req.URL.Fragment != "" || req.URL.ForceQuery ||
		strings.ContainsAny(authority, "@/?#") {
		return "", errors.New("CONNECT target is not authority-form")
	}
	key, _, _, err := parseDestination(authority)
	if err != nil {
		return "", err
	}
	return key, nil
}

func parseDestination(authority string) (key, host string, port uint16, err error) {
	if strings.Contains(authority, "[") || strings.Contains(authority, "]") {
		return "", "", 0, errors.New("IP literals are not destinations")
	}
	host, portText, err := net.SplitHostPort(authority)
	if err != nil || host == "" || portText == "" || strings.Contains(host, ":") {
		return "", "", 0, errors.New("invalid authority")
	}
	if len(host) > 253 || strings.HasSuffix(host, ".") || net.ParseIP(host) != nil {
		return "", "", 0, errors.New("destination must be a canonical DNS name")
	}
	for _, label := range strings.Split(host, ".") {
		if label == "" || len(label) > 63 || label[0] == '-' || label[len(label)-1] == '-' {
			return "", "", 0, errors.New("invalid DNS name")
		}
		for _, r := range label {
			if r >= 'A' && r <= 'Z' || r >= 'a' && r <= 'z' || r >= '0' && r <= '9' || r == '-' {
				continue
			}
			return "", "", 0, errors.New("invalid DNS name")
		}
	}
	p, err := strconv.ParseUint(portText, 10, 16)
	if err != nil || p == 0 {
		return "", "", 0, errors.New("invalid effective port")
	}
	host = strings.ToLower(host)
	return net.JoinHostPort(host, strconv.FormatUint(p, 10)), host, uint16(p), nil
}

// specialUseIPv4 and specialUseIPv6 are deny tables, not an attempt to infer
// routability from IsGlobalUnicast. The IANA registries include global-unicast
// ranges that are not public-Internet destinations (for example CGNAT and
// benchmarking), and special ranges with narrow globally reachable exceptions.
// A provider API has no reason to resolve into any of them, so the whole
// special-use allocation is refused. Loopback alone has an isolated-test
// exception in pinAddresses.
var specialUseIPv4 = mustPrefixes(
	"0.0.0.0/8",       // this host on this network
	"10.0.0.0/8",      // private-use
	"100.64.0.0/10",   // shared address space / CGNAT
	"127.0.0.0/8",     // loopback
	"169.254.0.0/16",  // link-local
	"172.16.0.0/12",   // private-use
	"192.0.0.0/24",    // IETF protocol assignments
	"192.0.2.0/24",    // documentation (TEST-NET-1)
	"192.31.196.0/24", // AS112-v4
	"192.52.193.0/24", // AMT
	"192.88.99.0/24",  // 6to4 relay anycast
	"192.168.0.0/16",  // private-use
	"192.175.48.0/24", // direct delegation AS112 service
	"198.18.0.0/15",   // benchmarking
	"198.51.100.0/24", // documentation (TEST-NET-2)
	"203.0.113.0/24",  // documentation (TEST-NET-3)
	"224.0.0.0/4",     // multicast
	"240.0.0.0/4",     // reserved, including limited broadcast
)

var specialUseIPv6 = mustPrefixes(
	"::/96",          // unspecified and deprecated IPv4-compatible addresses
	"::ffff:0:0/96",  // IPv4-mapped addresses (also rejected explicitly)
	"64:ff9b::/96",   // IPv4/IPv6 translation well-known prefix
	"64:ff9b:1::/48", // local-use IPv4/IPv6 translation
	"100::/64",       // discard-only
	"2001::/23",      // IETF protocol assignments, Teredo, benchmarking, ORCHID
	"2001:db8::/32",  // documentation
	"2002::/16",      // 6to4
	"3fff::/20",      // documentation
	"5f00::/16",      // segment routing (SRv6)
	"fc00::/7",       // unique local
	"fe80::/10",      // link-local
	"fec0::/10",      // deprecated site-local
	"ff00::/8",       // multicast
)

// RFC 4291 defines the currently allocated global-unicast portion of IPv6.
// IsGlobalUnicast intentionally has broader Go semantics and also reports
// reserved space outside this prefix, so it is not sufficient on its own.
var globalUnicastIPv6 = netip.MustParsePrefix("2000::/3")

func mustPrefixes(values ...string) []netip.Prefix {
	prefixes := make([]netip.Prefix, len(values))
	for i, value := range values {
		prefixes[i] = netip.MustParsePrefix(value)
	}
	return prefixes
}

func isPublicInternetUnicast(ip netip.Addr) bool {
	if !ip.IsValid() || ip.Zone() != "" || ip.Is4In6() || !ip.IsGlobalUnicast() {
		return false
	}
	table := specialUseIPv6
	if ip.Is4() {
		table = specialUseIPv4
	} else if !globalUnicastIPv6.Contains(ip) {
		return false
	}
	for _, prefix := range table {
		if prefix.Contains(ip) {
			return false
		}
	}
	return true
}

func pinAddresses(ips []netip.Addr, allowLoopback bool) ([]netip.Addr, error) {
	if len(ips) == 0 {
		return nil, errors.New("empty DNS answer")
	}
	seen := make(map[netip.Addr]bool, len(ips))
	out := make([]netip.Addr, 0, len(ips))
	for _, ip := range ips {
		// Never normalize a mapped address into an allowed IPv4 address: its
		// original form and any zone must be rejected before policy evaluation.
		if !ip.IsValid() || ip.Zone() != "" || ip.Is4In6() {
			return nil, errors.New("DNS answer contains an invalid or mapped address")
		}
		if ip.IsLoopback() {
			if !allowLoopback {
				return nil, errors.New("DNS answer contains a non-public address")
			}
			// Explicit isolated-test exception; all other special addresses stay denied.
		} else if !isPublicInternetUnicast(ip) {
			return nil, errors.New("DNS answer contains a non-public address")
		}
		if !seen[ip] {
			seen[ip] = true
			out = append(out, ip)
		}
	}
	return out, nil
}

func checkLimits(in Limits) (Limits, error) {
	var err error
	if in.HeaderBytes == 0 {
		in.HeaderBytes = defaultHeaderBytes
	}
	if in.HeaderBytes < 256 || in.HeaderBytes > maxHeaderBytes {
		return Limits{}, errors.New("egress: invalid header bound")
	}
	if in.Connections == 0 {
		in.Connections = defaultConnections
	}
	if in.Connections < 1 || in.Connections > maxConnections {
		return Limits{}, errors.New("egress: invalid connection bound")
	}
	if in.IdleTimeout, err = boundedDuration("idle timeout", in.IdleTimeout, defaultIdleTimeout, maxIdleTimeout); err != nil {
		return Limits{}, err
	}
	if in.TotalTimeout, err = boundedDuration("total timeout", in.TotalTimeout, defaultTotalTimeout, maxTotalTimeout); err != nil {
		return Limits{}, err
	}
	if in.MaxBytes == 0 {
		in.MaxBytes = defaultMaxBytes
	}
	if in.MaxBytes < 1 || in.MaxBytes > maxTunnelBytes {
		return Limits{}, errors.New("egress: invalid byte bound")
	}
	return in, nil
}

func boundedDuration(name string, value, fallback, ceiling time.Duration) (time.Duration, error) {
	if value == 0 {
		value = fallback
	}
	if value < time.Millisecond || value > ceiling {
		return 0, fmt.Errorf("egress: invalid %s", name)
	}
	return value, nil
}

func writeStatus(w io.Writer, status int) {
	text := http.StatusText(status)
	if text == "" {
		status = http.StatusForbidden
		text = http.StatusText(status)
	}
	fmt.Fprintf(w, "HTTP/1.1 %d %s\r\nContent-Length: 0\r\nConnection: close\r\n\r\n", status, text)
}

func minTime(a, b time.Time) time.Time {
	if a.Before(b) {
		return a
	}
	return b
}
