package policy

import (
	"net"
	"testing"
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
