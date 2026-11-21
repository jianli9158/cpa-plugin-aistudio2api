package api

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/netip"
	"net/url"
	"strconv"
	"strings"
	"syscall"
	"time"
)

// errOutboundBlocked marks outbound requests rejected by administrator allowlist or SSRF IP/redirect policy.
var errOutboundBlocked = errors.New("outbound destination blocked by security policy")

// nonPublicPrefixes covers IANA special-purpose IPv4/IPv6 ranges not flagged by netip.Addr.IsPrivate / IsLoopback.
var nonPublicPrefixes = []netip.Prefix{
	netip.MustParsePrefix("0.0.0.0/8"),       // RFC 1122 "this network" (routed to loopback on Linux)
	netip.MustParsePrefix("100.64.0.0/10"),   // RFC 6598 Shared Address Space (CGNAT / Tailscale / VPC internal)
	netip.MustParsePrefix("192.0.0.0/24"),    // RFC 6890 IETF Protocol Assignments
	netip.MustParsePrefix("192.0.2.0/24"),    // RFC 5737 TEST-NET-1
	netip.MustParsePrefix("198.18.0.0/15"),   // RFC 2544 Benchmarking
	netip.MustParsePrefix("198.51.100.0/24"), // RFC 5737 TEST-NET-2
	netip.MustParsePrefix("203.0.113.0/24"),  // RFC 5737 TEST-NET-3
	netip.MustParsePrefix("240.0.0.0/4"),     // RFC 1112 Reserved + 255.255.255.255/32 broadcast
	netip.MustParsePrefix("::/96"),           // Deprecated IPv4-compatible IPv6
	netip.MustParsePrefix("100::/64"),        // RFC 6666 Discard-Only
	netip.MustParsePrefix("2001::/32"),       // RFC 4380 Teredo
	netip.MustParsePrefix("2001:2::/48"),     // RFC 5180 Benchmarking
	netip.MustParsePrefix("2001:db8::/32"),   // RFC 3849 Documentation
	netip.MustParsePrefix("2002::/16"),       // RFC 3056 6to4 (can embed private IPv4)
	netip.MustParsePrefix("64:ff9b::/96"),    // RFC 6052 NAT64 well-known prefix
	netip.MustParsePrefix("64:ff9b:1::/48"),  // RFC 8215 NAT64 local-use prefix
	netip.MustParsePrefix("fec0::/10"),       // Deprecated IPv6 site-local
}

// CheckPublicIP rejects loopback, private, link-local, CGNAT, multicast, unspecified, zoned, and reserved addresses.
func CheckPublicIP(addr netip.Addr) error {
	if !addr.IsValid() {
		return fmt.Errorf("%w: invalid IP address", errOutboundBlocked)
	}
	if addr.Zone() != "" {
		return fmt.Errorf("%w: zoned IP address %s is not allowed", errOutboundBlocked, addr)
	}
	addr = addr.Unmap()
	if !addr.IsGlobalUnicast() || addr.IsLoopback() || addr.IsPrivate() || addr.IsLinkLocalUnicast() ||
		addr.IsLinkLocalMulticast() || addr.IsInterfaceLocalMulticast() || addr.IsMulticast() || addr.IsUnspecified() {
		return fmt.Errorf("%w: non-public address %s", errOutboundBlocked, addr)
	}
	for _, prefix := range nonPublicPrefixes {
		if prefix.Contains(addr) {
			return fmt.Errorf("%w: non-public address %s", errOutboundBlocked, addr)
		}
	}
	return nil
}

type outboundRule struct {
	scheme   string
	host     string
	wildcard bool
	port     int
}

func (r outboundRule) effectivePort() int {
	if r.port > 0 {
		return r.port
	}
	if r.scheme == "http" {
		return 80
	}
	return 443
}

// OutboundAllowlist holds administrator-configured host/origin allowlist rules.
// An empty allowlist denies all destinations.
type OutboundAllowlist struct {
	rules []outboundRule
}

// ParseOutboundAllowlist parses a comma- or whitespace-separated list of host or origin allowlist entries.
// Supported forms:
//   - "mcp.example.com" (HTTPS, port 443)
//   - "mcp.example.com:8443" (HTTPS, port 8443)
//   - "*.trusted.example.com" or "*.trusted.example.com:8443" (HTTPS, subdomain match only; requires >=2 domain labels)
//   - "https://mcp.example.com[:port]" or "http://mcp.example.com[:port]"
//   - "https://*.trusted.example.com[:port]" or "http://*.trusted.example.com[:port]"
//
// Bare wildcards ("*"), userinfo, paths, and literal non-public IPs are ignored/rejected.
func ParseOutboundAllowlist(raw string) OutboundAllowlist {
	var list OutboundAllowlist
	fields := strings.FieldsFunc(raw, func(r rune) bool {
		return r == ',' || r == ';' || r == ' ' || r == '\t' || r == '\n' || r == '\r'
	})
	for _, token := range fields {
		if rule, ok := parseOutboundRule(token); ok {
			list.rules = append(list.rules, rule)
		}
	}
	return list
}

func parseOutboundRule(token string) (outboundRule, bool) {
	entry := strings.TrimSpace(strings.ToLower(token))
	if entry == "" || entry == "*" {
		return outboundRule{}, false
	}

	scheme := "https"
	if idx := strings.Index(entry, "://"); idx >= 0 {
		scheme = entry[:idx]
		entry = entry[idx+3:]
		if scheme != "http" && scheme != "https" {
			return outboundRule{}, false
		}
	}
	// Strip optional trailing slash only when no path segments exist.
	entry = strings.TrimSuffix(entry, "/")
	if entry == "" || strings.ContainsAny(entry, "/?#@\\") {
		return outboundRule{}, false
	}

	var host string
	var port int
	if strings.HasPrefix(entry, "[") {
		closeIdx := strings.Index(entry, "]")
		if closeIdx < 0 {
			return outboundRule{}, false
		}
		host = entry[1:closeIdx]
		rest := entry[closeIdx+1:]
		if rest != "" {
			if !strings.HasPrefix(rest, ":") {
				return outboundRule{}, false
			}
			p, err := strconv.Atoi(rest[1:])
			if err != nil || p < 1 || p > 65535 {
				return outboundRule{}, false
			}
			port = p
		}
	} else if strings.Contains(entry, ":") {
		h, portStr, err := net.SplitHostPort(entry)
		if err != nil {
			return outboundRule{}, false
		}
		p, err := strconv.Atoi(portStr)
		if err != nil || p < 1 || p > 65535 {
			return outboundRule{}, false
		}
		host, port = h, p
	} else {
		host = entry
	}

	host = strings.TrimSuffix(strings.TrimSpace(host), ".")
	if host == "" {
		return outboundRule{}, false
	}

	wildcard := false
	if strings.HasPrefix(host, "*.") {
		wildcard = true
		host = strings.TrimPrefix(host, "*.")
		// Require at least two labels (e.g., example.com) so "*.com" cannot be configured.
		if !isValidHostname(host) || strings.Count(host, ".") < 1 {
			return outboundRule{}, false
		}
		if _, err := netip.ParseAddr(host); err == nil {
			return outboundRule{}, false
		}
	} else {
		if strings.Contains(host, "*") {
			return outboundRule{}, false
		}
		if addr, err := netip.ParseAddr(host); err == nil {
			if CheckPublicIP(addr) != nil {
				return outboundRule{}, false
			}
			host = addr.Unmap().String()
		} else if !isValidHostname(host) {
			return outboundRule{}, false
		}
	}

	return outboundRule{
		scheme:   scheme,
		host:     host,
		wildcard: wildcard,
		port:     port,
	}, true
}

func isValidHostname(host string) bool {
	if host == "" || len(host) > 253 {
		return false
	}
	for label := range strings.SplitSeq(host, ".") {
		if label == "" || len(label) > 63 {
			return false
		}
		if label[0] == '-' || label[len(label)-1] == '-' {
			return false
		}
		for i := 0; i < len(label); i++ {
			c := label[i]
			if !((c >= 'a' && c <= 'z') || (c >= '0' && c <= '9') || c == '-') {
				return false
			}
		}
	}
	return true
}

// IsEmpty reports whether no valid allowlist rules are configured.
func (l OutboundAllowlist) IsEmpty() bool {
	return len(l.rules) == 0
}

// Allows reports whether u matches at least one configured rule (scheme, host, and port).
func (l OutboundAllowlist) Allows(u *url.URL) bool {
	if u == nil || len(l.rules) == 0 {
		return false
	}
	scheme := strings.ToLower(u.Scheme)
	if scheme != "http" && scheme != "https" {
		return false
	}
	host := normalizeURLHost(u)
	if host == "" {
		return false
	}
	port, ok := effectiveURLPort(u)
	if !ok {
		return false
	}
	for _, rule := range l.rules {
		if rule.scheme != scheme || rule.effectivePort() != port {
			continue
		}
		if rule.wildcard {
			if len(host) > len(rule.host)+1 && strings.HasSuffix(host, "."+rule.host) {
				return true
			}
		} else if host == rule.host {
			return true
		}
	}
	return false
}

func normalizeURLHost(u *url.URL) string {
	if u == nil {
		return ""
	}
	host := strings.TrimSuffix(strings.ToLower(u.Hostname()), ".")
	if addr, err := netip.ParseAddr(host); err == nil {
		if addr.Zone() != "" {
			return ""
		}
		return addr.Unmap().String()
	}
	return host
}

func effectiveURLPort(u *url.URL) (int, bool) {
	if u == nil {
		return 0, false
	}
	if portStr := u.Port(); portStr != "" {
		p, err := strconv.Atoi(portStr)
		if err != nil || p < 1 || p > 65535 {
			return 0, false
		}
		return p, true
	}
	switch strings.ToLower(u.Scheme) {
	case "http":
		return 80, true
	case "https":
		return 443, true
	default:
		return 0, false
	}
}

func sameURLOrigin(a, b *url.URL) bool {
	if a == nil || b == nil {
		return false
	}
	pa, okA := effectiveURLPort(a)
	pb, okB := effectiveURLPort(b)
	return okA && okB &&
		strings.EqualFold(a.Scheme, b.Scheme) &&
		normalizeURLHost(a) == normalizeURLHost(b) &&
		pa == pb
}

// validateHTTPURLSyntax checks scheme (http/https), non-empty host, non-opaque form, and forbids embedded userinfo.
func validateHTTPURLSyntax(rawURL string) (*url.URL, error) {
	parsed, err := url.Parse(strings.TrimSpace(rawURL))
	if err != nil {
		return nil, fmt.Errorf("%w: invalid url: %v", errOutboundBlocked, err)
	}
	scheme := strings.ToLower(parsed.Scheme)
	if scheme != "http" && scheme != "https" {
		return nil, fmt.Errorf("%w: unsupported url scheme %q", errOutboundBlocked, parsed.Scheme)
	}
	if parsed.Opaque != "" {
		return nil, fmt.Errorf("%w: opaque url is not allowed", errOutboundBlocked)
	}
	if parsed.User != nil {
		return nil, fmt.Errorf("%w: url credentials are not allowed", errOutboundBlocked)
	}
	host := parsed.Hostname()
	if strings.TrimSpace(host) == "" {
		return nil, fmt.Errorf("%w: missing url host", errOutboundBlocked)
	}
	if _, ok := effectiveURLPort(parsed); !ok {
		return nil, fmt.Errorf("%w: invalid url port", errOutboundBlocked)
	}
	return parsed, nil
}

type outboundValidator struct {
	kind             string
	configKey        string
	allowlist        OutboundAllowlist
	requireAllowlist bool
	allowRedirects   bool
	maxRedirects     int
	egressProxy      func() string
	envProxy         bool
	lookupIP         func(ctx context.Context, host string) ([]netip.Addr, error)
}

func (v outboundValidator) resolveAndCheckHost(ctx context.Context, rawHost string) ([]netip.Addr, error) {
	host := strings.TrimSuffix(strings.TrimSpace(rawHost), ".")
	if host == "" {
		return nil, fmt.Errorf("%w: empty host", errOutboundBlocked)
	}
	if addr, err := netip.ParseAddr(host); err == nil {
		if err := CheckPublicIP(addr); err != nil {
			return nil, fmt.Errorf("host %s: %w", rawHost, err)
		}
		return []netip.Addr{addr.Unmap()}, nil
	}
	lookup := v.lookupIP
	if lookup == nil {
		lookup = func(ctx context.Context, h string) ([]netip.Addr, error) {
			return net.DefaultResolver.LookupNetIP(ctx, "ip", h)
		}
	}
	addresses, err := lookup(ctx, host)
	if err != nil {
		return nil, err
	}
	if len(addresses) == 0 {
		return nil, fmt.Errorf("%w: host %s resolved to no addresses", errOutboundBlocked, rawHost)
	}
	validated := make([]netip.Addr, 0, len(addresses))
	for _, addr := range addresses {
		if err := CheckPublicIP(addr); err != nil {
			return nil, fmt.Errorf("host %s resolves to %w", rawHost, err)
		}
		validated = append(validated, addr.Unmap())
	}
	return validated, nil
}

func (v outboundValidator) validateURL(ctx context.Context, rawURL string) (*url.URL, []netip.Addr, error) {
	parsed, err := validateHTTPURLSyntax(rawURL)
	if err != nil {
		return nil, nil, err
	}
	if v.requireAllowlist {
		if v.allowlist.IsEmpty() {
			return nil, nil, fmt.Errorf("%w: %s outbound connections are disabled by default; configure %s to allow trusted hosts", errOutboundBlocked, v.kind, v.configKey)
		}
		if !v.allowlist.Allows(parsed) {
			return nil, nil, fmt.Errorf("%w: %s destination %q is not in %s", errOutboundBlocked, v.kind, parsed.Scheme+"://"+parsed.Host, v.configKey)
		}
	}
	ips, err := v.resolveAndCheckHost(ctx, parsed.Hostname())
	if err != nil {
		return nil, nil, err
	}
	return parsed, ips, nil
}

type validatedOutboundTransport struct {
	base      http.RoundTripper
	validator outboundValidator
}

func (t validatedOutboundTransport) RoundTrip(request *http.Request) (*http.Response, error) {
	if request == nil || request.URL == nil {
		return nil, fmt.Errorf("%w: missing request URL", errOutboundBlocked)
	}
	if _, _, err := t.validator.validateURL(request.Context(), request.URL.String()); err != nil {
		return nil, err
	}
	return t.base.RoundTrip(request)
}

func (v outboundValidator) newTransport() (*http.Transport, error) {
	transport := http.DefaultTransport.(*http.Transport).Clone()
	transport.TLSHandshakeTimeout = 10 * time.Second
	transport.ResponseHeaderTimeout = 30 * time.Second

	var explicitProxyAddr string
	if v.egressProxy != nil {
		transport.Proxy = nil
		if proxyRaw := strings.TrimSpace(v.egressProxy()); proxyRaw != "" {
			proxyURL, err := url.Parse(proxyRaw)
			if err != nil {
				return nil, fmt.Errorf("%s egress proxy: %w", v.kind, err)
			}
			transport.Proxy = http.ProxyURL(proxyURL)
			explicitProxyAddr = proxyHostPort(proxyURL)
		}
	} else if !v.envProxy {
		transport.Proxy = nil
	}

	proxyDialer := &net.Dialer{Timeout: 30 * time.Second, KeepAlive: 30 * time.Second}
	directDialer := &net.Dialer{
		Timeout:   30 * time.Second,
		KeepAlive: 30 * time.Second,
		Control: func(_, address string, _ syscall.RawConn) error {
			ap, err := netip.ParseAddrPort(address)
			if err != nil {
				return fmt.Errorf("%w: invalid peer address %q", errOutboundBlocked, address)
			}
			return CheckPublicIP(ap.Addr())
		},
	}

	transport.DialContext = func(ctx context.Context, network, address string) (net.Conn, error) {
		if explicitProxyAddr != "" && address == explicitProxyAddr {
			return proxyDialer.DialContext(ctx, network, address)
		}
		if v.envProxy && remoteMediaProxy(address) {
			return proxyDialer.DialContext(ctx, network, address)
		}
		host, port, err := net.SplitHostPort(address)
		if err != nil {
			return nil, fmt.Errorf("%w: invalid dial target %q", errOutboundBlocked, address)
		}
		ips, err := v.resolveAndCheckHost(ctx, host)
		if err != nil {
			return nil, err
		}
		var lastErr error
		for _, ip := range ips {
			conn, dialErr := directDialer.DialContext(ctx, network, net.JoinHostPort(ip.String(), port))
			if dialErr == nil {
				return conn, nil
			}
			lastErr = dialErr
		}
		return nil, lastErr
	}

	return transport, nil
}

func (v outboundValidator) newHTTPClient(timeout time.Duration, wrap func(http.RoundTripper) http.RoundTripper) (*http.Client, error) {
	transport, err := v.newTransport()
	if err != nil {
		return nil, err
	}
	return v.newHTTPClientWithBase(timeout, transport, wrap), nil
}

func (v outboundValidator) newHTTPClientWithBase(timeout time.Duration, base http.RoundTripper, wrap func(http.RoundTripper) http.RoundTripper) *http.Client {
	rt := http.RoundTripper(validatedOutboundTransport{base: base, validator: v})
	if wrap != nil {
		rt = wrap(rt)
	}
	maxRedir := v.maxRedirects
	if maxRedir <= 0 {
		maxRedir = 5
	}
	return &http.Client{
		Timeout:   timeout,
		Transport: rt,
		CheckRedirect: func(request *http.Request, via []*http.Request) error {
			if !v.allowRedirects {
				return fmt.Errorf("%w: %s redirects are not allowed", errOutboundBlocked, v.kind)
			}
			if len(via) >= maxRedir {
				return fmt.Errorf("%w: stopped after %d redirects", errOutboundBlocked, maxRedir)
			}
			if _, _, err := v.validateURL(request.Context(), request.URL.String()); err != nil {
				return err
			}
			if len(via) > 0 && !sameURLOrigin(request.URL, via[0].URL) {
				request.Header.Del("Authorization")
				request.Header.Del("Cookie")
				request.Header.Del("Proxy-Authorization")
			}
			return nil
		},
	}
}

func proxyHostPort(proxyURL *url.URL) string {
	if proxyURL == nil {
		return ""
	}
	port := proxyURL.Port()
	if port == "" {
		port = proxyDefaultPorts[strings.ToLower(proxyURL.Scheme)]
	}
	if proxyURL.Hostname() == "" || port == "" {
		return ""
	}
	return net.JoinHostPort(proxyURL.Hostname(), port)
}
