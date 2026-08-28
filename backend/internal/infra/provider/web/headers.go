package web

import (
	"crypto/rand"
	"encoding/hex"
	"net/http"
	"strings"

	infraegress "github.com/chenyme/grok2api/backend/internal/infra/egress"
	"github.com/google/uuid"
)

func buildHeaders(token string, lease *infraegress.Lease, contentType string) http.Header {
	if contentType == "" {
		contentType = "application/json"
	}
	value := http.Header{}
	value.Set("Content-Type", contentType)
	value.Set("Accept", "*/*")
	value.Set("Accept-Encoding", "gzip, deflate, br, zstd")
	value.Set("Accept-Language", "zh-CN,zh;q=0.9,en;q=0.8")
	value.Set("User-Agent", lease.UserAgent)
	value.Set("Cookie", infraegress.BuildSSOCookie(token, lease.CFCookies))
	value.Set("x-xai-request-id", newRequestUUID())
	return value
}

// applyRuntimeIdentityCookies adds the authenticated account identity resolved
// by this process. A browser-captured Grok device ID stored with the account is
// preserved because Grok binds Imagine uploads and generation to that durable
// browser identity. Legacy accounts without one retain the deterministic
// per-account fallback.
func applyRuntimeIdentityCookies(value http.Header, userID string) {
	userID = strings.TrimSpace(userID)
	if userID == "" {
		return
	}
	cookie := strings.TrimSpace(value.Get("Cookie"))
	if cookie != "" {
		cookie += "; "
	}
	deviceID := cookieValue(cookie, "grok_device_id")
	if deviceID == "" {
		deviceID = uuid.NewSHA1(uuid.NameSpaceURL, []byte("grok2api:web-device:"+userID)).String()
	}
	cookie = removeCookie(cookie, "grok_device_id")
	if cookie != "" {
		cookie += "; "
	}
	value.Set("Cookie", cookie+"grok_device_id="+deviceID+"; x-userid="+userID)
}

func cookieValue(cookie, target string) string {
	for part := range strings.SplitSeq(cookie, ";") {
		name, value, ok := strings.Cut(strings.TrimSpace(part), "=")
		if ok && strings.EqualFold(strings.TrimSpace(name), target) {
			return strings.TrimSpace(value)
		}
	}
	return ""
}

func removeCookie(cookie, target string) string {
	parts := make([]string, 0, 8)
	for part := range strings.SplitSeq(cookie, ";") {
		part = strings.TrimSpace(part)
		if part == "" {
			continue
		}
		name, _, ok := strings.Cut(part, "=")
		if ok && strings.EqualFold(strings.TrimSpace(name), target) {
			continue
		}
		parts = append(parts, part)
	}
	return strings.Join(parts, "; ")
}

// applyAppHeaders 补齐真实浏览器同源 fetch 会携带的稳定请求头，不伪造 Sentry 或 Client Hints。
func applyAppHeaders(value http.Header, origin, referer string) {
	value.Set("Origin", origin)
	value.Set("Referer", referer)
	value.Set("Cache-Control", "no-cache")
	value.Set("Pragma", "no-cache")
	value.Set("Priority", "u=1, i")
	value.Set("Sec-Fetch-Dest", "empty")
	value.Set("Sec-Fetch-Mode", "cors")
	value.Set("Sec-Fetch-Site", "same-origin")
}

func newRequestUUID() string {
	value := make([]byte, 16)
	if _, err := rand.Read(value); err != nil {
		return newWebID("req")
	}
	value[6] = (value[6] & 0x0f) | 0x40
	value[8] = (value[8] & 0x3f) | 0x80
	encoded := hex.EncodeToString(value)
	return encoded[:8] + "-" + encoded[8:12] + "-" + encoded[12:16] + "-" + encoded[16:20] + "-" + encoded[20:]
}
