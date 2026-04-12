package policy

import (
	"net"
	"net/http"
	"net/url"
	"testing"
	"time"
)

func TestNewURLPolicyValid(t *testing.T) {
	p, err := NewURLPolicy("https://example.com/llms.txt", "")
	if err != nil {
		t.Fatalf("NewURLPolicy() error = %v", err)
	}
	if p == nil {
		t.Fatal("NewURLPolicy() returned nil")
	}
}

func TestNewURLPolicyAllowedHosts(t *testing.T) {
	p, err := NewURLPolicy("https://example.com/llms.txt", "cdn.example.com,other.com")
	if err != nil {
		t.Fatalf("NewURLPolicy() error = %v", err)
	}
	if err := p.Validate("https://cdn.example.com/doc.md"); err != nil {
		t.Fatalf("Validate(allowed host) error = %v", err)
	}
}

func TestNewURLPolicyIPLiteralRejected(t *testing.T) {
	_, err := NewURLPolicy("https://192.168.1.1/llms.txt", "")
	if err == nil {
		t.Fatal("NewURLPolicy() expected error for IP literal")
	}
}

func TestValidateHTTPSOnly(t *testing.T) {
	p, _ := NewURLPolicy("https://example.com/llms.txt", "")
	if err := p.Validate("http://example.com/doc.md"); err == nil {
		t.Fatal("Validate() expected error for http scheme")
	}
}

func TestValidateAllowedHost(t *testing.T) {
	p, _ := NewURLPolicy("https://example.com/llms.txt", "")
	if err := p.Validate("https://example.com/doc.md"); err != nil {
		t.Fatalf("Validate() error = %v", err)
	}
}

func TestValidateDisallowedHost(t *testing.T) {
	p, _ := NewURLPolicy("https://example.com/llms.txt", "")
	if err := p.Validate("https://evil.com/doc.md"); err == nil {
		t.Fatal("Validate() expected error for disallowed host")
	}
}

func TestValidateResolvedIPLoopback(t *testing.T) {
	if err := ValidateResolvedIP(net.ParseIP("127.0.0.1")); err == nil {
		t.Fatal("ValidateResolvedIP() expected error for loopback")
	}
}

func TestValidateResolvedIPPrivate(t *testing.T) {
	if err := ValidateResolvedIP(net.ParseIP("10.0.0.1")); err == nil {
		t.Fatal("ValidateResolvedIP() expected error for private IP")
	}
}

func TestValidateResolvedIPPublic(t *testing.T) {
	if err := ValidateResolvedIP(net.ParseIP("8.8.8.8")); err != nil {
		t.Fatalf("ValidateResolvedIP() error = %v", err)
	}
}

func TestHTTPClientRedirectRejectsHTTPDowngrade(t *testing.T) {
	p, err := NewURLPolicy("https://example.com/llms.txt", "")
	if err != nil {
		t.Fatalf("NewURLPolicy() error = %v", err)
	}
	client := NewHTTPClient(10*time.Second, p)

	redirectURL, _ := url.Parse("http://example.com/doc.md")
	req := &http.Request{URL: redirectURL}
	via := []*http.Request{{}} // one prior request

	err = client.CheckRedirect(req, via)
	if err == nil {
		t.Fatal("CheckRedirect should reject HTTP downgrade")
	}
}

func TestValidateResolvedIPBlocksIPv6(t *testing.T) {
	tests := []struct {
		ip      string
		blocked bool
	}{
		{"::1", true},                       // IPv6 loopback
		{"fe80::1", true},                   // link-local
		{"fc00::1", true},                   // ULA/private
		{"2001:db8::1", true},               // documentation range
		{"2607:f8b0:4004:800::200e", false}, // public Google IPv6
	}
	for _, tt := range tests {
		ip := net.ParseIP(tt.ip)
		if ip == nil {
			t.Fatalf("failed to parse IP %q", tt.ip)
		}
		err := ValidateResolvedIP(ip)
		if tt.blocked && err == nil {
			t.Errorf("ValidateResolvedIP(%s) = nil, want error (blocked)", tt.ip)
		}
		if !tt.blocked && err != nil {
			t.Errorf("ValidateResolvedIP(%s) = %v, want nil (allowed)", tt.ip, err)
		}
	}
}

func TestValidateTrailingDotHost(t *testing.T) {
	p, err := NewURLPolicy("https://example.com/llms.txt", "")
	if err != nil {
		t.Fatalf("NewURLPolicy() error = %v", err)
	}
	if err := p.Validate("https://example.com./doc.md"); err != nil {
		t.Fatalf("Validate(trailing dot) error = %v", err)
	}
}
