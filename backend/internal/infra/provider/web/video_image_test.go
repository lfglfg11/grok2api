package web

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/chenyme/grok2api/backend/internal/domain/account"
	infraegress "github.com/chenyme/grok2api/backend/internal/infra/egress"
	"github.com/chenyme/grok2api/backend/internal/infra/provider"
	"github.com/chenyme/grok2api/backend/internal/infra/security"
)

// 1x1 PNG：loadChatImage 只接受真实可识别的图片字节。
const testFirstFrameDataURI = "data:image/png;base64,iVBORw0KGgoAAAANSUhEUgAAAAEAAAABCAYAAAAfFcSJAAAADUlEQVR42mP8z8DwHwAFAAH/q842iQAAAABJRU5ErkJggg=="

// TestImageToVideoPayloadMatchesCapturedMediaGenInputShape 锁定 grok.com 前端
// buildImageToVideoInput 的字段集合：单张首帧走 mediaGenInput.imageToVideo，
// 而不是 textToVideo，也不是 Console 的 image 字段。
func TestImageToVideoPayloadMatchesCapturedMediaGenInputShape(t *testing.T) {
	payload := imageToVideoPayload("让画面动起来", "16:9", "720p", 6, "metadata-1")

	if len(payload) != 8 || payload["modelName"] != "imagine-video-gen" ||
		payload["message"] != "让画面动起来 --mode=custom" ||
		payload["enableImageStreaming"] != true || payload["enableSideBySide"] != true ||
		payload["sendFinalMetadata"] != true || payload["kind"] != "CONVERSATION_KIND_IMAGINE" {
		t.Fatalf("payload = %#v", payload)
	}
	mediaGenInput, ok := payload["mediaGenInput"].(map[string]any)
	if !ok || len(mediaGenInput) != 1 {
		t.Fatalf("mediaGenInput = %#v", payload["mediaGenInput"])
	}
	if _, exists := mediaGenInput["textToVideo"]; exists {
		t.Fatalf("image-to-video must not emit textToVideo: %#v", mediaGenInput)
	}
	imageToVideo, ok := mediaGenInput["imageToVideo"].(map[string]any)
	if !ok {
		t.Fatalf("imageToVideo = %#v", mediaGenInput["imageToVideo"])
	}
	if imageToVideo["prompt"] != "让画面动起来" || imageToVideo["aspectRatio"] != "16:9" ||
		imageToVideo["duration"] != 6 || imageToVideo["resolutionName"] != "720p" {
		t.Fatalf("imageToVideo = %#v", imageToVideo)
	}
	assets, ok := imageToVideo["inputAssets"].([]string)
	if !ok || len(assets) != 1 || assets[0] != "metadata-1" {
		t.Fatalf("inputAssets = %#v", imageToVideo["inputAssets"])
	}
	// 首帧模式必须保留 useFirstFrame 的默认语义（前端只在 false 时下发）。
	if _, exists := imageToVideo["useFirstFrame"]; exists {
		t.Fatalf("useFirstFrame must be omitted for first-frame mode: %#v", imageToVideo)
	}
}

// TestImageToVideoPayloadDropsEmptyPrompt 前端仅在调用方提供 prompt 时下发该字段，
// 图生视频允许省略 prompt，此时不能把空字符串发给上游。
func TestImageToVideoPayloadDropsEmptyPrompt(t *testing.T) {
	payload := imageToVideoPayload("", "16:9", "720p", 6, "metadata-9")
	imageToVideo := payload["mediaGenInput"].(map[string]any)["imageToVideo"].(map[string]any)
	if _, exists := imageToVideo["prompt"]; exists {
		t.Fatalf("empty prompt leaked: %#v", imageToVideo)
	}
	if payload["message"] != "--mode=custom" {
		t.Fatalf("message = %#v", payload["message"])
	}
	if imageToVideo["duration"] != 6 || imageToVideo["resolutionName"] != "720p" {
		t.Fatalf("imageToVideo = %#v", imageToVideo)
	}
}

// TestGenerateVideoImageToVideoUploadsFirstFrame 验证适配器端到端：首帧先经
// Imagine 自上传换成 fileMetadataId，再作为 imageToVideo.inputAssets 发给上游。
func TestGenerateVideoImageToVideoUploadsFirstFrame(t *testing.T) {
	var (
		uploadCalls int
		createCalls int
		createBody  map[string]any
		uploadRef   string
	)
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		switch request.URL.Path {
		case "/sign":
			value := base64.RawStdEncoding.EncodeToString(make([]byte, 70))
			writer.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(writer).Encode(map[string]string{"x-statsig-id": value})
		case "/http/upload-file-v2/direct":
			uploadCalls++
			uploadRef = request.Header.Get("Referer")
			writer.Header().Set("Content-Type", "application/json")
			_, _ = io.WriteString(writer, `{"uploadId":"upload-1","fileMetadata":{"fileMetadataId":"metadata-7","fileUri":"users/11111111-1111-1111-1111-111111111111/reference/content","fileSource":"IMAGINE_SELF_UPLOAD_FILE_SOURCE"}}`)
		case "/rest/app-chat/conversations/new":
			createCalls++
			raw, _ := io.ReadAll(request.Body)
			_ = json.Unmarshal(raw, &createBody)
			writer.Header().Set("Content-Type", "text/event-stream")
			_, _ = io.WriteString(writer, `data: {"result":{"response":{"streamingVideoGenerationResponse":{"progress":100,"videoPostId":"post_1","videoUrl":"/videos/i2v.mp4"}}}}`+"\n\n")
		default:
			http.NotFound(writer, request)
		}
	}))
	defer server.Close()

	cipher, err := security.NewCipher(base64.StdEncoding.EncodeToString(make([]byte, 32)))
	if err != nil {
		t.Fatal(err)
	}
	encryptedToken, err := cipher.Encrypt("test-sso")
	if err != nil {
		t.Fatal(err)
	}
	adapter := NewAdapter(Config{
		BaseURL: server.URL, StatsigMode: "url", StatsigSignerURL: server.URL + "/sign", VideoTimeoutSeconds: 5,
	}, infraegress.NewManager(egressRepositoryStub{}, cipher), cipher, nil, nil)
	adapter.statsig.fetchMeta = func(context.Context, string, string, *infraegress.Lease) (string, error) {
		return "current-page-meta", nil
	}
	adapter.statsig.fetchMetaForPath = func(context.Context, string, string, *infraegress.Lease, string) (string, error) {
		return "current-page-meta", nil
	}
	adapter.statsig.validateEndpoint = func(context.Context, string) error { return nil }

	result, err := adapter.GenerateVideo(context.Background(), provider.VideoRequest{
		Credential: account.Credential{
			ID: 1, Provider: account.ProviderWeb, EncryptedAccessToken: encryptedToken,
			UserID: "11111111-1111-1111-1111-111111111111",
		},
		Prompt: "gentle camera push in", Duration: 6, AspectRatio: "16:9", Resolution: "720p",
		ImageURL: testFirstFrameDataURI,
	})
	if err != nil {
		t.Fatalf("GenerateVideo error = %v", err)
	}
	if result.URL != "https://assets.grok.com/videos/i2v.mp4" || result.ContentType != "video/mp4" {
		t.Fatalf("result = %#v", result)
	}
	if uploadCalls != 1 || createCalls != 1 {
		t.Fatalf("upload calls=%d create calls=%d", uploadCalls, createCalls)
	}
	if !strings.Contains(uploadRef, "/imagine") {
		t.Fatalf("upload referer = %q", uploadRef)
	}
	if createBody["modelName"] != "imagine-video-gen" {
		t.Fatalf("create body = %#v", createBody)
	}
	imageToVideo, ok := createBody["mediaGenInput"].(map[string]any)["imageToVideo"].(map[string]any)
	if !ok {
		t.Fatalf("mediaGenInput = %#v", createBody["mediaGenInput"])
	}
	assets, ok := imageToVideo["inputAssets"].([]any)
	if !ok || len(assets) != 1 || assets[0] != "metadata-7" {
		t.Fatalf("inputAssets = %#v", imageToVideo["inputAssets"])
	}
	if imageToVideo["duration"] != float64(6) || imageToVideo["resolutionName"] != "720p" {
		t.Fatalf("imageToVideo = %#v", imageToVideo)
	}
}

// TestGenerateVideoReferenceImagesStillRejected 参考图生视频对应前端的
// referenceToVideo，需要 omni 实验与多资产上传，尚未实现时必须显式拒绝，
// 不能静默退化成文生视频。
func TestGenerateVideoReferenceImagesStillRejected(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		t.Fatalf("unexpected upstream call: %s", request.URL.Path)
	}))
	defer server.Close()

	cipher, err := security.NewCipher(base64.StdEncoding.EncodeToString(make([]byte, 32)))
	if err != nil {
		t.Fatal(err)
	}
	encryptedToken, err := cipher.Encrypt("test-sso")
	if err != nil {
		t.Fatal(err)
	}
	adapter := NewAdapter(Config{BaseURL: server.URL, VideoTimeoutSeconds: 5},
		infraegress.NewManager(egressRepositoryStub{}, cipher), cipher, nil, nil)

	_, err = adapter.GenerateVideo(context.Background(), provider.VideoRequest{
		Credential:    account.Credential{ID: 1, Provider: account.ProviderWeb, EncryptedAccessToken: encryptedToken},
		Prompt:        "animate",
		ReferenceURLs: []string{"https://example.com/ref.png"},
	})
	if err == nil || !strings.Contains(err.Error(), "参考图生视频") {
		t.Fatalf("reference-to-video error = %v", err)
	}
}
