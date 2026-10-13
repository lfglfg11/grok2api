package web

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strings"

	"github.com/chenyme/grok2api/backend/internal/domain/account"
	domainegress "github.com/chenyme/grok2api/backend/internal/domain/egress"
)

// 临时诊断开关：仅当数据目录存在 media-debug.json 时生效，用于对比图生视频
// 负载变体并抓取上游 SSE 原文。文件不存在时返回零值，行为与改动前完全一致。
const mediaDebugFile = "/app/data/media-debug.json"

type mediaDebugConfig struct {
	// Variant 选择 imageToVideo 负载的构造方式。
	Variant string `json:"variant"`
	// LogStream 为真时把上游 SSE 原文（有界）写入日志。
	LogStream bool `json:"logStream"`
}

func mediaDebug() mediaDebugConfig {
	value := mediaDebugConfig{}
	raw, err := os.ReadFile(filepath.Clean(mediaDebugFile))
	if err != nil {
		return value
	}
	_ = json.Unmarshal(raw, &value)
	value.Variant = strings.TrimSpace(value.Variant)
	return value
}

var mediaDebugThumbnailPattern = regexp.MustCompile(`"thumbnailImageUrl":"([^"]+)"`)

// debugThumbnailURL 从上游 SSE 原文里取出生成结果的预览图地址。
func debugThumbnailURL(body string) string {
	match := mediaDebugThumbnailPattern.FindStringSubmatch(body)
	if len(match) < 2 {
		return ""
	}
	value := strings.TrimSpace(match[1])
	if value == "" {
		return ""
	}
	if strings.HasPrefix(value, "https://") {
		return value
	}
	return "https://assets.grok.com/" + strings.TrimLeft(value, "/")
}

// debugFetchThumbnail 用账号自身凭据取预览图并返回 base64，仅供诊断对比；
// assets.grok.com 的资源不接受无凭据访问。
func (a *Adapter) debugFetchThumbnail(ctx context.Context, cfg Config, credential account.Credential, rawURL string) string {
	parsed, err := url.Parse(strings.TrimSpace(rawURL))
	if err != nil || parsed.Scheme != "https" || parsed.User != nil || !trustedImageAssetHost(parsed.Hostname()) {
		return ""
	}
	token, err := a.cipher.Decrypt(credential.EncryptedAccessToken)
	if err != nil {
		return ""
	}
	lease, err := a.egress.AcquireCredential(ctx, domainegress.ScopeWebAsset, credential)
	if err != nil {
		return ""
	}
	defer lease.Release()
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, parsed.String(), nil)
	if err != nil {
		return ""
	}
	request.Header = buildHeaders(token, lease, "")
	request.Header.Del("Content-Type")
	response, err := lease.Do(request)
	if err != nil {
		return ""
	}
	defer response.Body.Close()
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		return ""
	}
	data, err := io.ReadAll(io.LimitReader(response.Body, 2<<20))
	if err != nil || len(data) == 0 {
		return ""
	}
	return base64.StdEncoding.EncodeToString(data)
}
