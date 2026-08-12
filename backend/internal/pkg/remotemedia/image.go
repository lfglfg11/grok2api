package remotemedia

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/netip"
	"net/url"
	"strings"
	"syscall"
	"time"

	"github.com/chenyme/grok2api/backend/internal/pkg/netguard"
)

const (
	MaxImageBytes     = 20 << 20
	fetchAttempts     = 6
	transportAttempts = 3
	fetchTimeout      = 20 * time.Second
	resolveTimeout    = 3 * time.Second
	maxRedirects      = 5
)

var (
	ErrImageTooLarge = errors.New("remote image exceeds size limit")
	ErrFetchBlocked  = errors.New("remote image URL is not allowed")
	ErrInvalidImage  = errors.New("remote response is empty or not an image")
)

type resolver interface {
	LookupNetIP(ctx context.Context, network, host string) ([]netip.Addr, error)
}

type target struct {
	fetchURL   *url.URL
	hostHeader string
	serverName string
}

type statusError struct{ status int }

type PinnedHTTPSDoer func(request *http.Request, serverName string) (*http.Response, error)

func (e statusError) Error() string { return fmt.Sprintf("remote image returned HTTP %d", e.status) }

// FetchImage downloads a public HTTP(S) image while pinning every validated
// redirect target to prevent DNS rebinding and other SSRF bypasses.
func FetchImage(ctx context.Context, rawURL string) ([]byte, error) {
	attempt := 0
	return fetchImageWith(ctx, rawURL, func(ctx context.Context, rawURL string) ([]byte, error) {
		data, err := fetchImageOnceWith(ctx, rawURL, attempt, nil)
		attempt++
		return data, err
	})
}

// FetchImageWithPinnedHTTPS downloads through a caller-owned managed egress
// lease while retaining DNS pinning, redirect validation, size limits, and the
// same three-attempt header strategy as direct downloads.
func FetchImageWithPinnedHTTPS(ctx context.Context, rawURL string, do PinnedHTTPSDoer) ([]byte, error) {
	if do == nil {
		return nil, errors.New("managed media transport is nil")
	}
	attempt := 0
	return fetchImageWith(ctx, rawURL, func(ctx context.Context, rawURL string) ([]byte, error) {
		data, err := fetchImageOnceWith(ctx, rawURL, attempt, do)
		attempt++
		return data, err
	})
}

func fetchImageWith(ctx context.Context, rawURL string, fetch func(context.Context, string) ([]byte, error)) ([]byte, error) {
	var lastErr error
	transportFailures := 0
	for attempt := 0; attempt < fetchAttempts; attempt++ {
		data, err := fetch(ctx, rawURL)
		if err == nil {
			return data, nil
		}
		lastErr = err
		if !retryable(err) || attempt+1 >= fetchAttempts {
			break
		}
		var status statusError
		if !errors.As(err, &status) && !errors.Is(err, ErrInvalidImage) {
			transportFailures++
			if transportFailures >= transportAttempts {
				break
			}
		}
		if err := waitRetry(ctx, attempt); err != nil {
			return nil, err
		}
	}
	return nil, lastErr
}

func fetchImageOnceWith(ctx context.Context, rawURL string, attempt int, managed PinnedHTTPSDoer) ([]byte, error) {
	parsed, err := url.Parse(strings.TrimSpace(rawURL))
	if err != nil || !validURL(parsed) {
		return nil, ErrFetchBlocked
	}
	fetchCtx, cancel := context.WithTimeout(ctx, fetchTimeout)
	defer cancel()

	for redirects := 0; ; redirects++ {
		target, err := resolveTarget(fetchCtx, parsed, net.DefaultResolver)
		if err != nil {
			return nil, err
		}
		req, err := http.NewRequestWithContext(fetchCtx, http.MethodGet, target.fetchURL.String(), nil)
		if err != nil {
			return nil, err
		}
		req.Host = target.hostHeader
		applyImageRequestHeaders(req, parsed, attempt)

		var resp *http.Response
		var closeIdle func()
		if managed != nil {
			if parsed.Scheme != "https" || parsed.Port() != "" && parsed.Port() != "443" {
				return nil, fmt.Errorf("managed media download requires HTTPS 443: %w", ErrFetchBlocked)
			}
			resp, err = managed(req, target.serverName)
			closeIdle = func() {}
		} else {
			client, transport := newClient(target)
			resp, err = client.Do(req)
			closeIdle = transport.CloseIdleConnections
		}
		if err != nil {
			closeIdle()
			if errors.Is(err, ErrFetchBlocked) {
				return nil, ErrFetchBlocked
			}
			return nil, err
		}
		if isRedirect(resp.StatusCode) && resp.Header.Get("Location") != "" {
			_ = resp.Body.Close()
			closeIdle()
			if redirects >= maxRedirects {
				return nil, errors.New("remote image has too many redirects")
			}
			next, err := parsed.Parse(resp.Header.Get("Location"))
			if err != nil || !validURL(next) {
				return nil, fmt.Errorf("invalid remote image redirect: %w", ErrFetchBlocked)
			}
			parsed = next
			continue
		}

		data, readErr := readImage(resp)
		closeIdle()
		return data, readErr
	}
}

func applyImageRequestHeaders(request *http.Request, parsed *url.URL, attempt int) {
	request.Header.Set("Accept", "text/html,application/xhtml+xml,application/xml;q=0.9,image/avif,image/webp,image/apng,*/*;q=0.8")
	request.Header.Set("Accept-Language", "zh-CN,zh;q=0.9,en;q=0.8")
	request.Header.Set("Cache-Control", "no-cache")
	request.Header.Set("Pragma", "no-cache")
	request.Header.Set("Sec-Ch-Ua", `"Google Chrome";v="143", "Chromium";v="143", "Not A(Brand";v="24"`)
	request.Header.Set("Sec-Ch-Ua-Mobile", "?0")
	request.Header.Set("Sec-Ch-Ua-Platform", `"Windows"`)
	request.Header.Set("Upgrade-Insecure-Requests", "1")
	request.Header.Set("User-Agent", "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/143.0.0.0 Safari/537.36")
	switch attempt % 6 {
	case 0: // Browser navigation works for hosts that reject bare image clients.
		request.Header.Set("Sec-Fetch-Site", "none")
		request.Header.Set("Sec-Fetch-Mode", "navigate")
		request.Header.Set("Sec-Fetch-Dest", "document")
		request.Header.Set("Sec-Fetch-User", "?1")
	case 1: // Ordinary cross-site image resource.
		request.Header.Set("Sec-Fetch-Site", "cross-site")
		request.Header.Set("Sec-Fetch-Mode", "no-cors")
		request.Header.Set("Sec-Fetch-Dest", "image")
	case 2: // Same-origin Referer/Origin fallback for hotlink-protected hosts.
		origin := parsed.Scheme + "://" + parsed.Host
		request.Header.Set("Referer", origin+"/")
		request.Header.Set("Origin", origin)
		request.Header.Set("Sec-Fetch-Site", "same-origin")
		request.Header.Set("Sec-Fetch-Mode", "no-cors")
		request.Header.Set("Sec-Fetch-Dest", "image")
	case 3: // Origin only; some hosts reject Referer while requiring Origin.
		origin := parsed.Scheme + "://" + parsed.Host
		request.Header.Set("Origin", origin)
		request.Header.Set("Sec-Fetch-Site", "cross-site")
		request.Header.Set("Sec-Fetch-Mode", "no-cors")
		request.Header.Set("Sec-Fetch-Dest", "image")
	case 4: // Explicitly no Referer or Origin.
		request.Header.Set("Accept", "image/avif,image/webp,image/apng,image/*,*/*;q=0.8")
	case 5: // Self Referer fallback.
		request.Header.Set("Referer", parsed.String())
		request.Header.Set("Sec-Fetch-Site", "cross-site")
		request.Header.Set("Sec-Fetch-Mode", "no-cors")
		request.Header.Set("Sec-Fetch-Dest", "image")
	}
}

func retryable(err error) bool {
	if errors.Is(err, ErrFetchBlocked) || errors.Is(err, ErrImageTooLarge) {
		return false
	}
	var status statusError
	if errors.As(err, &status) {
		return status.status == http.StatusUnauthorized || status.status == http.StatusForbidden || status.status == http.StatusNotAcceptable ||
			status.status == http.StatusRequestTimeout || status.status == http.StatusTooEarly || status.status == http.StatusTooManyRequests || status.status >= 500
	}
	return true
}

func waitRetry(ctx context.Context, attempt int) error {
	delays := [...]time.Duration{200 * time.Millisecond, 750 * time.Millisecond}
	timer := time.NewTimer(delays[min(attempt, len(delays)-1)])
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}

func validURL(parsed *url.URL) bool {
	if parsed == nil || (parsed.Scheme != "http" && parsed.Scheme != "https") || parsed.Hostname() == "" || parsed.User != nil {
		return false
	}
	port := parsed.Port()
	return port == "" || port == "80" || port == "443"
}

func resolveTarget(ctx context.Context, parsed *url.URL, resolver resolver) (*target, error) {
	if !validURL(parsed) {
		return nil, ErrFetchBlocked
	}
	host := strings.TrimSuffix(strings.ToLower(parsed.Hostname()), ".")
	if host == "localhost" || strings.HasSuffix(host, ".localhost") || strings.HasSuffix(host, ".local") || strings.HasSuffix(host, ".internal") || strings.Contains(host, "%") {
		return nil, ErrFetchBlocked
	}

	var addresses []netip.Addr
	if address, err := netip.ParseAddr(host); err == nil {
		addresses = []netip.Addr{address.Unmap()}
	} else {
		resolveCtx, cancel := context.WithTimeout(ctx, resolveTimeout)
		defer cancel()
		resolved, err := resolver.LookupNetIP(resolveCtx, "ip", host)
		if err != nil {
			return nil, fmt.Errorf("resolve remote image host: %w", err)
		}
		if len(resolved) == 0 {
			return nil, errors.New("resolve remote image host: DNS returned no addresses")
		}
		for _, address := range resolved {
			addresses = append(addresses, address.Unmap())
		}
	}
	for _, address := range addresses {
		if !netguard.IsPublicAddress(address) {
			return nil, fmt.Errorf("remote image host resolved to non-public address %s: %w", address, ErrFetchBlocked)
		}
	}

	port := parsed.Port()
	if port == "" {
		if parsed.Scheme == "https" {
			port = "443"
		} else {
			port = "80"
		}
	}
	fetchURL := *parsed
	fetchURL.Host = net.JoinHostPort(addresses[0].String(), port)
	fetchURL.Fragment = ""
	return &target{fetchURL: &fetchURL, hostHeader: parsed.Host, serverName: host}, nil
}

func newClient(target *target) (*http.Client, *http.Transport) {
	transport := &http.Transport{
		Proxy:                  nil,
		DialContext:            (&net.Dialer{Timeout: 10 * time.Second, KeepAlive: 10 * time.Second, Control: safeControl}).DialContext,
		TLSClientConfig:        &tls.Config{MinVersion: tls.VersionTLS12, ServerName: target.serverName},
		TLSHandshakeTimeout:    10 * time.Second,
		ResponseHeaderTimeout:  15 * time.Second,
		MaxResponseHeaderBytes: 1 << 20,
		MaxIdleConns:           1,
		MaxConnsPerHost:        1,
		IdleConnTimeout:        30 * time.Second,
		ForceAttemptHTTP2:      true,
	}
	return &http.Client{Transport: transport, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}, transport
}

func safeControl(network, address string, _ syscall.RawConn) error {
	if network != "tcp" && network != "tcp4" && network != "tcp6" {
		return fmt.Errorf("unsupported network %q: %w", network, ErrFetchBlocked)
	}
	host, _, err := net.SplitHostPort(address)
	if err != nil {
		return fmt.Errorf("parse target address: %w", ErrFetchBlocked)
	}
	ip, err := netip.ParseAddr(host)
	if err != nil || !netguard.IsPublicAddress(ip.Unmap()) {
		return fmt.Errorf("target is not a public address: %w", ErrFetchBlocked)
	}
	return nil
}

func isRedirect(status int) bool {
	return status == http.StatusMovedPermanently || status == http.StatusFound || status == http.StatusSeeOther || status == http.StatusTemporaryRedirect || status == http.StatusPermanentRedirect
}

func readImage(resp *http.Response) ([]byte, error) {
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, statusError{status: resp.StatusCode}
	}
	if resp.ContentLength > MaxImageBytes {
		return nil, ErrImageTooLarge
	}
	data, err := io.ReadAll(io.LimitReader(resp.Body, MaxImageBytes+1))
	if err != nil {
		return nil, err
	}
	if len(data) > MaxImageBytes {
		return nil, ErrImageTooLarge
	}
	if len(data) == 0 {
		return nil, ErrInvalidImage
	}
	switch http.DetectContentType(data) {
	case "image/jpeg", "image/png", "image/gif", "image/webp":
	default:
		return nil, ErrInvalidImage
	}
	return data, nil
}
