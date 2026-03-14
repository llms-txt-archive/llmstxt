// Package policy enforces URL allowlisting and SSRF protection for outbound HTTP requests.
package policy

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/netip"
	"net/url"
	"strings"
	"time"
)

var blockedIPPfx = []netip.Prefix{
	netip.MustParsePrefix("0.0.0.0/8"),
	netip.MustParsePrefix("100.64.0.0/10"),
	netip.MustParsePrefix("192.0.0.0/24"),
	netip.MustParsePrefix("192.0.2.0/24"),
	netip.MustParsePrefix("198.18.0.0/15"),
	netip.MustParsePrefix("198.51.100.0/24"),
	netip.MustParsePrefix("203.0.113.0/24"),
	netip.MustParsePrefix("240.0.0.0/4"),
	netip.MustParsePrefix("2001:db8::/32"),
}

// URLPolicy enforces host-based allowlisting for outbound HTTP requests.
type URLPolicy struct {
	allowedHosts map[string]struct{}
}

// NewURLPolicy creates a URLPolicy that allows the source URL's host plus any additional comma-separated hosts.
func NewURLPolicy(sourceURL string, allowedHostsCSV string) (*URLPolicy, error) {
	parsedSource, err := url.Parse(sourceURL)
	if err != nil {
		return nil, fmt.Errorf("parse source URL: %w", err)
	}

	sourceHost, err := normalizeHost(parsedSource.Hostname())
	if err != nil {
		return nil, fmt.Errorf("source host: %w", err)
	}

	allowedHosts := map[string]struct{}{sourceHost: {}}
	for _, field := range strings.Split(allowedHostsCSV, ",") {
		if strings.TrimSpace(field) == "" {
			continue
		}
		host, err := normalizeHost(field)
		if err != nil {
			return nil, fmt.Errorf("allowed host %q: %w", field, err)
		}
		allowedHosts[host] = struct{}{}
	}

	return &URLPolicy{allowedHosts: allowedHosts}, nil
}

func normalizeHost(host string) (string, error) {
	host = strings.ToLower(strings.TrimSpace(strings.TrimSuffix(host, ".")))
	if host == "" {
		return "", errors.New("missing host")
	}
	if strings.Contains(host, "://") {
		return "", errors.New("expected hostname, not URL")
	}
	if ip := net.ParseIP(host); ip != nil {
		return "", errors.New("IP literal hosts are not allowed")
	}
	if strings.Contains(host, "/") {
		return "", errors.New("invalid host")
	}
	return host, nil
}

// Validate parses rawURL and checks that its scheme and host are allowed.
func (p *URLPolicy) Validate(rawURL string) error {
	parsedURL, err := url.Parse(rawURL)
	if err != nil {
		return fmt.Errorf("parse URL: %w", err)
	}
	return p.ValidateURL(parsedURL)
}

// ValidateURL checks that parsedURL uses HTTPS and targets an allowed host.
func (p *URLPolicy) ValidateURL(parsedURL *url.URL) error {
	if parsedURL == nil {
		return errors.New("missing URL")
	}
	if !strings.EqualFold(parsedURL.Scheme, "https") {
		return fmt.Errorf("scheme %q is not allowed", parsedURL.Scheme)
	}

	host, err := normalizeHost(parsedURL.Hostname())
	if err != nil {
		return err
	}
	if _, ok := p.allowedHosts[host]; !ok {
		return fmt.Errorf("host %q is not allowed", host)
	}
	return nil
}

// ValidateResolvedHost resolves the host via DNS and returns dial addresses after validating each resolved IP.
func (p *URLPolicy) ValidateResolvedHost(ctx context.Context, host string) ([]string, error) {
	normalizedHost, err := normalizeHost(host)
	if err != nil {
		return nil, err
	}
	if _, ok := p.allowedHosts[normalizedHost]; !ok {
		return nil, fmt.Errorf("host %q is not allowed", normalizedHost)
	}

	addrs, err := net.DefaultResolver.LookupIPAddr(ctx, normalizedHost)
	if err != nil {
		return nil, fmt.Errorf("resolve %s: %w", normalizedHost, err)
	}
	if len(addrs) == 0 {
		return nil, fmt.Errorf("resolve %s: no addresses returned", normalizedHost)
	}

	dialAddrs := make([]string, 0, len(addrs))
	for _, addr := range addrs {
		if err := ValidateResolvedIP(addr.IP); err != nil {
			return nil, fmt.Errorf("resolve %s: %w", normalizedHost, err)
		}
		dialAddrs = append(dialAddrs, addr.IP.String())
	}

	return dialAddrs, nil
}

// ValidateResolvedIP returns an error if ip is a loopback, private, link-local, or otherwise blocked address.
func ValidateResolvedIP(ip net.IP) error {
	addr, ok := netip.AddrFromSlice(ip)
	if !ok {
		return errors.New("invalid resolved IP")
	}
	addr = addr.Unmap()

	if addr.IsLoopback() ||
		addr.IsPrivate() ||
		addr.IsLinkLocalUnicast() ||
		addr.IsLinkLocalMulticast() ||
		addr.IsMulticast() ||
		addr.IsUnspecified() {
		return fmt.Errorf("resolved to blocked IP %s", addr)
	}
	for _, prefix := range blockedIPPfx {
		if prefix.Contains(addr) {
			return fmt.Errorf("resolved to blocked IP %s", addr)
		}
	}
	return nil
}

// NewHTTPClient returns an http.Client that validates every dial and redirect against the given URLPolicy.
func NewHTTPClient(timeout time.Duration, urlPolicy *URLPolicy) *http.Client {
	baseTransport, ok := http.DefaultTransport.(*http.Transport)
	if !ok {
		baseTransport = &http.Transport{}
	}
	transport := baseTransport.Clone()
	dialer := &net.Dialer{Timeout: timeout, KeepAlive: 30 * time.Second}

	transport.DialContext = func(ctx context.Context, network string, address string) (net.Conn, error) {
		host, port, err := net.SplitHostPort(address)
		if err != nil {
			return nil, err
		}

		if ip := net.ParseIP(host); ip != nil {
			if err := ValidateResolvedIP(ip); err != nil {
				return nil, err
			}
			return dialer.DialContext(ctx, network, address)
		}

		dialAddrs, err := urlPolicy.ValidateResolvedHost(ctx, host)
		if err != nil {
			return nil, err
		}

		var lastErr error
		for _, dialAddr := range dialAddrs {
			conn, err := dialer.DialContext(ctx, network, net.JoinHostPort(dialAddr, port))
			if err == nil {
				return conn, nil
			}
			lastErr = err
		}
		if lastErr == nil {
			lastErr = fmt.Errorf("no dialable addresses for %s", host)
		}
		return nil, lastErr
	}

	return &http.Client{
		Timeout:   timeout,
		Transport: transport,
		CheckRedirect: func(req *http.Request, via []*http.Request) error {
			if len(via) >= 10 {
				return errors.New("stopped after 10 redirects")
			}
			return urlPolicy.ValidateURL(req.URL)
		},
	}
}
