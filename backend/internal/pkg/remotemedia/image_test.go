package remotemedia

import (
	"context"
	"crypto/tls"
	"errors"
	"net/http"
	"net/netip"
	"net/url"
	"testing"
)

type resolverFunc func(context.Context, string, string) ([]netip.Addr, error)

func (fn resolverFunc) LookupNetIP(ctx context.Context, network, host string) ([]netip.Addr, error) {
	return fn(ctx, network, host)
}

func TestResolveTargetRejectsAnyPrivateDNSAnswer(t *testing.T) {
	parsed, err := url.Parse("https://images.example.test/reference.png")
	if err != nil {
		t.Fatal(err)
	}
	resolver := resolverFunc(func(context.Context, string, string) ([]netip.Addr, error) {
		return []netip.Addr{netip.MustParseAddr("93.184.216.34"), netip.MustParseAddr("127.0.0.1")}, nil
	})
	if _, err := resolveTarget(context.Background(), parsed, resolver); !errors.Is(err, ErrFetchBlocked) {
		t.Fatalf("mixed public/private DNS answer was not blocked: %v", err)
	}
}

func TestResolveTargetPinsValidatedPublicAddress(t *testing.T) {
	parsed, err := url.Parse("https://Images.Example.test/photo.png?size=large#ignored")
	if err != nil {
		t.Fatal(err)
	}
	resolver := resolverFunc(func(_ context.Context, network, host string) ([]netip.Addr, error) {
		if network != "ip" || host != "images.example.test" {
			t.Fatalf("lookup network=%q host=%q", network, host)
		}
		return []netip.Addr{netip.MustParseAddr("93.184.216.34")}, nil
	})
	target, err := resolveTarget(context.Background(), parsed, resolver)
	if err != nil {
		t.Fatal(err)
	}
	if target.fetchURL.Host != "93.184.216.34:443" || target.fetchURL.Path != "/photo.png" || target.fetchURL.RawQuery != "size=large" || target.fetchURL.Fragment != "" {
		t.Fatalf("fetch URL = %s", target.fetchURL)
	}
	if target.hostHeader != "Images.Example.test" || target.serverName != "images.example.test" {
		t.Fatalf("host header=%q server name=%q", target.hostHeader, target.serverName)
	}
	client, transport := newClient(target)
	defer transport.CloseIdleConnections()
	if client.CheckRedirect == nil || transport.TLSClientConfig == nil || transport.TLSClientConfig.ServerName != "images.example.test" || transport.TLSClientConfig.MinVersion != tls.VersionTLS12 {
		t.Fatal("pinned HTTPS client did not preserve strict TLS verification")
	}
}

func TestResolveTargetDoesNotClassifyDNSFailureAsPolicyBlock(t *testing.T) {
	parsed, _ := url.Parse("https://missing.example.test/photo.png")
	lookupErr := errors.New("temporary DNS failure")
	resolver := resolverFunc(func(context.Context, string, string) ([]netip.Addr, error) { return nil, lookupErr })
	_, err := resolveTarget(context.Background(), parsed, resolver)
	if !errors.Is(err, lookupErr) || errors.Is(err, ErrFetchBlocked) {
		t.Fatalf("DNS failure classification=%v", err)
	}
}

func TestFetchImageRetriesTransientFailureThreeTimes(t *testing.T) {
	calls := 0
	want := errors.New("connection reset")
	_, err := fetchImageWith(context.Background(), "https://example.com/image.png", func(context.Context, string) ([]byte, error) {
		calls++
		return nil, want
	})
	if !errors.Is(err, want) || calls != transportAttempts {
		t.Fatalf("err=%v calls=%d", err, calls)
	}
	calls = 0
	_, err = fetchImageWith(context.Background(), "https://example.com/image.png", func(context.Context, string) ([]byte, error) {
		calls++
		return nil, statusError{status: http.StatusForbidden}
	})
	if err == nil || calls != fetchAttempts {
		t.Fatalf("header-strategy err=%v calls=%d", err, calls)
	}
	calls = 0
	_, err = fetchImageWith(context.Background(), "https://example.com/image.png", func(context.Context, string) ([]byte, error) {
		calls++
		return nil, statusError{status: http.StatusNotFound}
	})
	if err == nil || calls != 1 {
		t.Fatalf("non-retryable err=%v calls=%d", err, calls)
	}
}

func TestValidURLRejectsCredentialsAndUnexpectedPorts(t *testing.T) {
	for _, raw := range []string{"file:///tmp/a.png", "https://user:pass@example.com/a.png", "https://example.com:8443/a.png"} {
		parsed, _ := url.Parse(raw)
		if validURL(parsed) {
			t.Errorf("URL %q was allowed", raw)
		}
	}
}
