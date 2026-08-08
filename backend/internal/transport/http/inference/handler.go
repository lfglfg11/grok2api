package inference

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"html"
	"io"
	"math"
	"mime"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	clientkeyapp "github.com/chenyme/grok2api/backend/internal/application/clientkey"
	"github.com/chenyme/grok2api/backend/internal/application/gateway"
	modelapp "github.com/chenyme/grok2api/backend/internal/application/model"
	"github.com/chenyme/grok2api/backend/internal/domain/account"
	clientkeydomain "github.com/chenyme/grok2api/backend/internal/domain/clientkey"
	mediadomain "github.com/chenyme/grok2api/backend/internal/domain/media"
	modeldomain "github.com/chenyme/grok2api/backend/internal/domain/model"
	"github.com/chenyme/grok2api/backend/internal/pkg/neterror"
	"github.com/chenyme/grok2api/backend/internal/transport/http/middleware"
	"github.com/gin-gonic/gin"
)

type Handler struct {
	gateway          *gateway.Service
	models           *modelapp.Service
	maxBodyBytes     int64
	publicAPIBaseURL string
	publicBaseURL    func() string
}

const (
	responseCopyBufferBytes         = 32 << 10
	maxJSONMetadataInspectionBytes  = 8 << 20
	maxStreamEventInspectionBytes   = 8 << 20
	maxStreamFailureDiagnosticBytes = 64 << 10
	maxCredentialErrorInspectBytes  = 64 << 10
	maxJSONResponseTransferBytes    = 128 << 20
	maxStreamResponseTransferBytes  = 256 << 20
	maxMediaResponseTransferBytes   = int64(2) << 30
	responseWriteTimeout            = 30 * time.Second
)

var (
	errResponseTransferLimit    = errors.New("响应超过代理安全上限")
	errUpstreamStreamIncomplete = errors.New("上游流在终止事件前结束")
	errUpstreamStreamFailed     = errors.New("上游流返回失败终止事件")
	errUpstreamStreamRead       = errors.New("读取上游流失败")
)

type streamProtocol uint8

const (
	streamProtocolResponses streamProtocol = iota
	streamProtocolChat
	streamProtocolAnthropic
	streamProtocolImage
)

const mediaTransferErrorTrailer = "X-Grok2API-Transfer-Error"

func NewHandler(gatewayService *gateway.Service, models *modelapp.Service, maxBodyBytes int64, publicAPIBaseURL ...string) *Handler {
	baseURL := ""
	if len(publicAPIBaseURL) > 0 {
		baseURL = strings.TrimRight(strings.TrimSpace(publicAPIBaseURL[0]), "/")
	}
	return &Handler{gateway: gatewayService, models: models, maxBodyBytes: maxBodyBytes, publicAPIBaseURL: baseURL}
}

// SetPublicAPIBaseURLResolver makes video content URLs follow hot-updated runtime settings.
// Set it before Register; request handling only reads the resolver.
func (h *Handler) SetPublicAPIBaseURLResolver(resolve func() string) *Handler {
	h.publicBaseURL = resolve
	return h
}

func (h *Handler) Register(router *gin.RouterGroup) {
	router.GET("/models", h.listModels)
	router.POST("/responses", h.createResponse)
	router.POST("/chat/completions", h.createChatCompletion)
	router.POST("/messages", h.createMessage)
	router.POST("/images/generations", h.generateImage)
	router.POST("/images/edits", h.editImage)
	router.POST("/videos/generations", h.generateVideo)
	router.POST("/videos", h.createCompatibleVideo)
	router.GET("/videos/:requestId", h.getVideo)
	router.GET("/videos/:requestId/content", h.getVideoContent)
	router.POST("/responses/compact", h.compactResponse)
	router.GET("/responses/:responseId", h.getResponse)
	router.DELETE("/responses/:responseId", h.deleteResponse)
}

type responsesRequest struct {
	Model              string `json:"model"`
	Stream             bool   `json:"stream"`
	PromptCacheKey     string `json:"prompt_cache_key"`
	PreviousResponseID string `json:"previous_response_id"`
}

type chatCompletionRequest struct {
	Model          string `json:"model"`
	Stream         bool   `json:"stream"`
	PromptCacheKey string `json:"prompt_cache_key"`
}

type chatVideoMessage struct {
	Role    string          `json:"role"`
	Content json.RawMessage `json:"content"`
}
type chatVideoCompatibilityRequest struct {
	Model       string             `json:"model"`
	Messages    []chatVideoMessage `json:"messages"`
	Stream      bool               `json:"stream"`
	Duration    json.RawMessage    `json:"duration"`
	Seconds     json.RawMessage    `json:"seconds"`
	Size        string             `json:"size"`
	AspectRatio string             `json:"aspect_ratio"`
	Resolution  string             `json:"resolution"`
	VideoConfig *struct {
		Duration    json.RawMessage `json:"duration"`
		Seconds     json.RawMessage `json:"seconds"`
		Size        string          `json:"size"`
		AspectRatio string          `json:"aspect_ratio"`
		Resolution  string          `json:"resolution"`
	} `json:"video_config"`
}

type messagesRequest struct {
	Model          string          `json:"model"`
	MaxTokens      *int            `json:"max_tokens"`
	Messages       json.RawMessage `json:"messages"`
	Stream         bool            `json:"stream"`
	PromptCacheKey string          `json:"prompt_cache_key"`
}

type imageGenerationRequest struct {
	Model          string          `json:"model"`
	Prompt         string          `json:"prompt"`
	Count          *int            `json:"n"`
	PartialImages  *int            `json:"partial_images"`
	Size           string          `json:"size"`
	AspectRatio    string          `json:"aspect_ratio"`
	Resolution     string          `json:"resolution"`
	ResponseFormat string          `json:"response_format"`
	StorageOptions json.RawMessage `json:"storage_options"`
	Stream         bool            `json:"stream"`
}

type imageEditJSONImage struct {
	URL    string `json:"url"`
	FileID string `json:"file_id"`
}

type imageEditJSONRequest struct {
	Model          string               `json:"model"`
	Prompt         string               `json:"prompt"`
	Image          *imageEditJSONImage  `json:"image"`
	Images         []imageEditJSONImage `json:"images"`
	Count          *int                 `json:"n"`
	Size           string               `json:"size"`
	AspectRatio    string               `json:"aspect_ratio"`
	Resolution     string               `json:"resolution"`
	ResponseFormat string               `json:"response_format"`
	StorageOptions json.RawMessage      `json:"storage_options"`
	Stream         bool                 `json:"stream"`
	PartialImages  *int                 `json:"partial_images"`
}

type videoGenerationImage struct {
	URL    string `json:"url"`
	FileID string `json:"file_id"`
}

type videoGenerationRequest struct {
	Model           string                 `json:"model"`
	Prompt          string                 `json:"prompt"`
	User            *string                `json:"user"`
	Duration        json.RawMessage        `json:"duration"`
	AspectRatio     string                 `json:"aspect_ratio"`
	Resolution      string                 `json:"resolution"`
	Image           *videoGenerationImage  `json:"image"`
	ReferenceImages []videoGenerationImage `json:"reference_images"`
	Output          json.RawMessage        `json:"output"`
	StorageOptions  json.RawMessage        `json:"storage_options"`
}

type compatibleVideoRequest struct {
	Model           string          `json:"model"`
	Prompt          string          `json:"prompt"`
	Seconds         json.RawMessage `json:"seconds"`
	Duration        json.RawMessage `json:"duration"`
	Size            string          `json:"size"`
	Image           string          `json:"image"`
	Images          []string        `json:"images"`
	InputReference  string          `json:"input_reference"`
	ReferenceImages []string        `json:"reference_images"`
	AspectRatio     string          `json:"aspect_ratio"`
	Resolution      string          `json:"resolution"`
}

type modelListItem struct {
	ID         string                 `json:"id"`
	Object     string                 `json:"object"`
	Created    int64                  `json:"created"`
	OwnedBy    string                 `json:"owned_by"`
	Provider   account.Provider       `json:"-"`
	Capability modeldomain.Capability `json:"-"`
}

func (h *Handler) listModels(c *gin.Context) {
	allowAliases := false
	var clientKey clientkeydomain.Key
	hasClientKey := false
	if clientValue, exists := c.Get(middleware.ClientKey); exists {
		if value, ok := clientValue.(clientkeydomain.Key); ok {
			clientKey = value
			hasClientKey = true
			allowAliases = clientKey.AllowModelAliases
		}
	}
	var values []modeldomain.Route
	var err error
	if hasClientKey {
		values, err = h.models.ListEnabledForClientKey(c.Request.Context(), clientKey)
	} else {
		values, err = h.models.ListEnabled(c.Request.Context())
	}
	if err != nil {
		writeOpenAIError(c, http.StatusInternalServerError, "model_list_failed", "读取模型列表失败")
		return
	}
	if hasClientKey {
		values = filterModelRoutesForClientKey(values, clientKey)
	}
	items := newModelListItems(values)
	if allowAliases {
		items = appendReasoningModelAliases(items)
	}
	if clientVersion := strings.TrimSpace(c.Query("client_version")); clientVersion != "" {
		writeCodexModelCatalog(c, newCodexModelCatalog(items))
		return
	}
	c.JSON(http.StatusOK, gin.H{"object": "list", "data": items})
}

func filterModelRoutesForClientKey(values []modeldomain.Route, key clientkeydomain.Key) []modeldomain.Route {
	filtered := make([]modeldomain.Route, 0, len(values))
	scope := key.AccountScope()
	for _, value := range values {
		if scope.AllowsProvider(value.Provider) && key.AllowsModel(value.ID) {
			filtered = append(filtered, value)
		}
	}
	return filtered
}

// newModelListItems deduplicates by downstream public name and hides Provider prefixes used only for internal routing.
func newModelListItems(values []modeldomain.Route) []modelListItem {
	data := make([]modelListItem, 0, len(values))
	seen := make(map[string]bool, len(values))
	for _, value := range values {
		publicID := modeldomain.ExternalPublicID(value.Provider, value.PublicID)
		if seen[publicID] {
			continue
		}
		seen[publicID] = true
		data = append(data, modelListItem{ID: publicID, Object: "model", Created: value.CreatedAt.Unix(), OwnedBy: "grok2api", Provider: value.Provider, Capability: value.Capability})
	}
	return data
}

// appendReasoningModelAliases expands base models into effort-suffixed aliases using only
// levels each model actually supports (never a blanket none/low/medium/high/xhigh/max template).
func appendReasoningModelAliases(items []modelListItem) []modelListItem {
	if len(items) == 0 {
		return items
	}
	seen := make(map[string]bool, len(items)*2)
	result := make([]modelListItem, 0, len(items)*2)
	for _, item := range items {
		seen[item.ID] = true
		result = append(result, item)
	}
	for _, item := range items {
		for _, aliasID := range modeldomain.ReasoningAliasPublicIDsForProvider(item.Provider, item.ID) {
			if seen[aliasID] {
				continue
			}
			seen[aliasID] = true
			result = append(result, modelListItem{
				ID: aliasID, Object: "model", Created: item.Created, OwnedBy: item.OwnedBy,
				Provider: item.Provider, Capability: item.Capability,
			})
		}
	}
	return result
}

func (h *Handler) createResponse(c *gin.Context) {
	h.handleCreate(c, false)
}

func (h *Handler) compactResponse(c *gin.Context) {
	h.handleCreate(c, true)
}

func (h *Handler) createChatCompletion(c *gin.Context) {
	c.Request.Body = http.MaxBytesReader(c.Writer, c.Request.Body, h.maxBodyBytes)
	if !isJSONRequest(c) {
		writeOpenAIError(c, http.StatusUnsupportedMediaType, "invalid_request", "Chat Completions only supports application/json")
		return
	}
	body, err := io.ReadAll(c.Request.Body)
	if err != nil {
		writeOpenAIError(c, http.StatusRequestEntityTooLarge, "request_too_large", "请求体超过限制")
		return
	}
	var request chatCompletionRequest
	if json.Unmarshal(body, &request) != nil || strings.TrimSpace(request.Model) == "" {
		writeOpenAIError(c, http.StatusBadRequest, "invalid_request", "Chat Completions 请求缺少有效 model")
		return
	}
	clientValue, exists := c.Get(middleware.ClientKey)
	clientKey, ok := clientValue.(clientkeydomain.Key)
	if !exists || !ok {
		writeOpenAIError(c, http.StatusUnauthorized, "invalid_api_key", "客户端 API Key 无效")
		return
	}
	requestID, _ := c.Get(middleware.RequestIDKey)
	requestIDValue, _ := requestID.(string)
	if isConsoleVideoChatModel(request.Model) && h.createConsoleVideoChat(c, body, request, clientKey, requestIDValue) {
		return
	}
	result, err := h.gateway.CreateChatCompletion(c.Request.Context(), gateway.Input{
		RequestID: requestIDValue, ClientKey: clientKey, PublicModel: request.Model,
		Body: body, Streaming: request.Stream, PromptCacheKey: request.PromptCacheKey,
		PromptCacheSeed:           extractPromptCacheSeed(c.Request.Header, body),
		AllowClientToolCacheRoute: allowBuildClientToolCacheRoute(c.Request.Header),
		GrokTurnIndex:             c.GetHeader("x-grok-turn-idx"),
	})
	if err != nil {
		writeGatewayError(c, err)
		return
	}
	h.writeResult(c, result, request.Stream, streamProtocolChat)
}

func isConsoleVideoChatModel(value string) bool {
	value = strings.TrimPrefix(strings.TrimSpace(value), "Console/")
	return strings.EqualFold(value, "grok-imagine-video")
}

func (h *Handler) createConsoleVideoChat(c *gin.Context, body []byte, request chatCompletionRequest, clientKey clientkeydomain.Key, requestID string) bool {
	var input chatVideoCompatibilityRequest
	if err := json.Unmarshal(body, &input); err != nil || len(input.Messages) == 0 {
		writeOpenAIError(c, http.StatusBadRequest, "invalid_request", "grok-imagine-video Chat Completions requires valid messages")
		return true
	}
	prompt, references, err := extractChatVideoPrompt(input.Messages)
	if err != nil {
		writeOpenAIError(c, http.StatusBadRequest, "invalid_request", err.Error())
		return true
	}
	durationRaw, aspectRatio, resolution := input.Duration, input.AspectRatio, input.Resolution
	if len(bytes.TrimSpace(durationRaw)) == 0 {
		durationRaw = input.Seconds
	}
	if input.VideoConfig != nil {
		if len(bytes.TrimSpace(input.VideoConfig.Duration)) > 0 {
			durationRaw = input.VideoConfig.Duration
		} else if len(bytes.TrimSpace(input.VideoConfig.Seconds)) > 0 {
			durationRaw = input.VideoConfig.Seconds
		}
		if strings.TrimSpace(input.VideoConfig.AspectRatio) != "" {
			aspectRatio = input.VideoConfig.AspectRatio
		}
		if strings.TrimSpace(input.VideoConfig.Resolution) != "" {
			resolution = input.VideoConfig.Resolution
		}
		if strings.TrimSpace(input.VideoConfig.Size) != "" && strings.TrimSpace(aspectRatio) == "" {
			aspectRatio = input.VideoConfig.Size
		}
	}
	duration, err := parseConsoleVideoChatDuration(durationRaw)
	if err != nil {
		writeOpenAIError(c, http.StatusBadRequest, "invalid_request", err.Error())
		return true
	}
	if strings.Contains(aspectRatio, "x") {
		aspectRatio = videoAspectRatioFromChatSize(aspectRatio)
	}
	if strings.TrimSpace(aspectRatio) == "" {
		prompt, aspectRatio = inferChatVideoAspectRatio(prompt)
	}
	if strings.TrimSpace(aspectRatio) == "" {
		aspectRatio = "16:9"
	}
	if !validVideoAspectRatio(aspectRatio) {
		writeOpenAIError(c, http.StatusBadRequest, "invalid_request", "aspect_ratio is invalid")
		return true
	}
	resolution = strings.ToLower(strings.TrimSpace(resolution))
	if resolution == "" {
		resolution = "720p"
	}
	if resolution != "480p" && resolution != "720p" {
		writeOpenAIError(c, http.StatusBadRequest, "invalid_request", "grok-imagine-video only supports 480p or 720p")
		return true
	}
	if prompt == "" && len(references) == 0 {
		writeOpenAIError(c, http.StatusBadRequest, "invalid_request", "text-to-video requires a prompt; image-to-video may omit it")
		return true
	}
	job, err := h.gateway.CreateVideo(c.Request.Context(), gateway.VideoInput{RequestID: requestID, ClientKey: clientKey, PublicModel: request.Model, Compatibility: true, Prompt: prompt, Duration: duration, AspectRatio: aspectRatio, Resolution: resolution, ReferenceURLs: references})
	if err != nil {
		writeGatewayError(c, err)
		return true
	}
	if request.Stream {
		h.streamConsoleVideoChat(c, request.Model, job.ID, clientKey)
		return true
	}
	job, err = h.waitConsoleVideoChat(c.Request.Context(), job.ID, clientKey)
	if err != nil {
		writeOpenAIError(c, http.StatusBadGateway, "video_generation_failed", err.Error())
		return true
	}
	if job.Status == mediadomain.StatusFailed {
		writeOpenAIError(c, http.StatusBadGateway, videoChatErrorCode(job), videoChatErrorMessage(job))
		return true
	}
	publicURL, err := h.completedVideoPublicURL(job)
	if err != nil {
		writeOpenAIError(c, http.StatusBadGateway, "video_result_unavailable", err.Error())
		return true
	}
	writeConsoleVideoChatResult(c, request.Model, formatConsoleVideoChatContent(publicURL))
	return true
}

func extractChatVideoPrompt(messages []chatVideoMessage) (string, []string, error) {
	prompt := ""
	var references []string
	for _, message := range messages {
		if strings.ToLower(strings.TrimSpace(message.Role)) != "user" {
			continue
		}
		var text string
		if json.Unmarshal(message.Content, &text) == nil {
			prompt, references = strings.TrimSpace(text), nil
			continue
		}
		var parts []map[string]json.RawMessage
		if err := json.Unmarshal(message.Content, &parts); err != nil {
			return "", nil, errors.New("user message content must be a string or content array")
		}
		for _, part := range parts {
			var kind string
			_ = json.Unmarshal(part["type"], &kind)
			if kind == "text" || kind == "input_text" {
				var value string
				if json.Unmarshal(part["text"], &value) == nil {
					text += value
				}
				continue
			}
			if kind == "image_url" || kind == "input_image" {
				var value string
				if json.Unmarshal(part["image_url"], &value) != nil {
					var image struct {
						URL string `json:"url"`
					}
					_ = json.Unmarshal(part["image_url"], &image)
					value = image.URL
				}
				if strings.TrimSpace(value) != "" {
					references = append([]string{strings.TrimSpace(value)}, references...)
				}
			}
		}
		prompt = strings.TrimSpace(text)
	}
	return prompt, references, nil
}

var chatVideoAspectMappings = []struct {
	key   string
	ratio string
}{
	{key: "16:9", ratio: "16:9"},
	{key: "16/9", ratio: "16:9"},
	{key: "1280x720", ratio: "16:9"},
	{key: "1920x1080", ratio: "16:9"},
	{key: "\u6a2a\u5c4f", ratio: "16:9"},
	{key: "\u5bbd\u5c4f", ratio: "16:9"},
	{key: "9:16", ratio: "9:16"},
	{key: "9/16", ratio: "9:16"},
	{key: "720x1280", ratio: "9:16"},
	{key: "1080x1920", ratio: "9:16"},
	{key: "\u7ad6\u5c4f", ratio: "9:16"},
	{key: "\u7eb5\u5c4f", ratio: "9:16"},
	{key: "4:3", ratio: "4:3"},
	{key: "4/3", ratio: "4:3"},
	{key: "3:4", ratio: "3:4"},
	{key: "3/4", ratio: "3:4"},
	{key: "3:2", ratio: "3:2"},
	{key: "3/2", ratio: "3:2"},
	{key: "2:3", ratio: "2:3"},
	{key: "2/3", ratio: "2:3"},
	{key: "1:1", ratio: "1:1"},
	{key: "1/1", ratio: "1:1"},
	{key: "1024x1024", ratio: "1:1"},
	{key: "\u6b63\u65b9\u5f62", ratio: "1:1"},
}

func parseConsoleVideoChatDuration(raw json.RawMessage) (int, error) {
	if !hasJSONValue(raw) {
		return 15, nil
	}
	return parseVideoDuration(raw)
}

func inferChatVideoAspectRatio(prompt string) (string, string) {
	normalized := strings.ToLower(strings.TrimSpace(prompt))
	normalized = strings.NewReplacer("\uFF1A", ":", "\u6BD4", ":", "\uFF0F", "/").Replace(normalized)
	compact := strings.ReplaceAll(normalized, " ", "")
	for _, item := range chatVideoAspectMappings {
		if strings.HasSuffix(compact, item.key) {
			return strings.TrimSpace(prompt), item.ratio
		}
	}
	return strings.TrimSpace(prompt), ""
}

func videoAspectRatioFromChatSize(value string) string {
	switch strings.ToLower(strings.TrimSpace(value)) {
	case "1280x720", "1920x1080", "16:9":
		return "16:9"
	case "720x1280", "1080x1920", "9:16":
		return "9:16"
	case "1024x1024", "1:1":
		return "1:1"
	default:
		return value
	}
}

func (h *Handler) waitConsoleVideoChat(ctx context.Context, jobID string, clientKey clientkeydomain.Key) (mediadomain.Job, error) {
	waitCtx, cancel := context.WithTimeout(ctx, 2*time.Hour+5*time.Minute)
	defer cancel()
	ticker := time.NewTicker(time.Second)
	defer ticker.Stop()
	for {
		job, err := h.gateway.GetVideo(waitCtx, jobID, clientKey)
		if err != nil {
			return mediadomain.Job{}, err
		}
		if job.Status == mediadomain.StatusCompleted || job.Status == mediadomain.StatusFailed {
			return job, nil
		}
		select {
		case <-waitCtx.Done():
			return mediadomain.Job{}, waitCtx.Err()
		case <-ticker.C:
		}
	}
}

func (h *Handler) streamConsoleVideoChat(c *gin.Context, model, jobID string, clientKey clientkeydomain.Key) {
	c.Header("Content-Type", "text/event-stream")
	c.Header("Cache-Control", "no-cache")
	c.Header("X-Accel-Buffering", "no")
	c.Status(http.StatusOK)
	responseID := "chatcmpl_video_" + strconv.FormatInt(time.Now().UnixNano(), 36)
	created := time.Now().Unix()
	if !writeConsoleVideoChatChunk(c, responseID, model, created, gin.H{"role": "assistant"}, nil) {
		return
	}
	waitCtx, cancel := context.WithTimeout(c.Request.Context(), 2*time.Hour+5*time.Minute)
	defer cancel()
	ticker := time.NewTicker(time.Second)
	defer ticker.Stop()
	lastProgress := -1
	for {
		job, err := h.gateway.GetVideo(waitCtx, jobID, clientKey)
		if err != nil {
			writeConsoleVideoChatError(c, "video_generation_failed", err.Error())
			return
		}
		progress := max(0, min(100, job.Progress))
		if progress != lastProgress {
			if !writeConsoleVideoChatChunk(c, responseID, model, created, gin.H{"reasoning_content": fmt.Sprintf("Video generation progress: %d%%\n", progress)}, nil) {
				return
			}
			lastProgress = progress
		}
		switch job.Status {
		case mediadomain.StatusCompleted:
			publicURL, resultErr := h.completedVideoPublicURL(job)
			if resultErr != nil {
				writeConsoleVideoChatError(c, "video_result_unavailable", resultErr.Error())
				return
			}
			if !writeConsoleVideoChatChunk(c, responseID, model, created, gin.H{"content": formatConsoleVideoChatContent(publicURL)}, nil) {
				return
			}
			if !writeConsoleVideoChatChunk(c, responseID, model, created, gin.H{}, "stop") {
				return
			}
			writeConsoleVideoChatDone(c)
			return
		case mediadomain.StatusFailed:
			writeConsoleVideoChatError(c, videoChatErrorCode(job), videoChatErrorMessage(job))
			return
		}
		select {
		case <-waitCtx.Done():
			writeConsoleVideoChatError(c, "video_generation_timeout", waitCtx.Err().Error())
			return
		case <-ticker.C:
		}
	}
}

func (h *Handler) completedVideoPublicURL(job mediadomain.Job) (string, error) {
	if job.Status != mediadomain.StatusCompleted || strings.TrimSpace(job.ResultAssetID) == "" {
		return "", errors.New("completed video has no local public asset")
	}
	return h.publicURL("/v1/media/videos/" + url.PathEscape(job.ResultAssetID)), nil
}

func videoChatErrorCode(job mediadomain.Job) string {
	if value := strings.TrimSpace(job.ErrorCode); value != "" {
		return value
	}
	return "video_generation_failed"
}
func videoChatErrorMessage(job mediadomain.Job) string {
	if value := strings.TrimSpace(job.ErrorMessage); value != "" {
		return value
	}
	return "video generation failed"
}

func formatConsoleVideoChatContent(publicURL string) string {
	escapedURL := html.EscapeString(publicURL)
	return "<video controls preload=\"metadata\" src=\"" + escapedURL + "\"></video>\n\n[Download Video](" + publicURL + ")"
}

func writeConsoleVideoChatResult(c *gin.Context, model, content string) {
	id := "chatcmpl_video_" + strconv.FormatInt(time.Now().UnixNano(), 36)
	c.JSON(http.StatusOK, gin.H{"id": id, "object": "chat.completion", "created": time.Now().Unix(), "model": model, "choices": []any{gin.H{"index": 0, "message": gin.H{"role": "assistant", "content": content}, "finish_reason": "stop"}}, "usage": gin.H{"prompt_tokens": 0, "completion_tokens": 0, "total_tokens": 0}})
}

func writeConsoleVideoChatChunk(c *gin.Context, id, model string, created int64, delta gin.H, finishReason any) bool {
	payload := gin.H{
		"id": id, "object": "chat.completion.chunk", "created": created, "model": model,
		"choices": []any{gin.H{"index": 0, "delta": delta, "finish_reason": finishReason}},
	}
	data, err := json.Marshal(payload)
	if err != nil {
		return false
	}
	if _, err = c.Writer.Write([]byte("data: ")); err != nil {
		return false
	}
	if _, err = c.Writer.Write(data); err != nil {
		return false
	}
	if _, err = c.Writer.Write([]byte("\n\n")); err != nil {
		return false
	}
	c.Writer.Flush()
	return true
}

func writeConsoleVideoChatError(c *gin.Context, code, message string) {
	data, _ := json.Marshal(gin.H{"error": gin.H{"message": message, "type": "server_error", "code": code}})
	_, _ = c.Writer.Write([]byte("data: "))
	_, _ = c.Writer.Write(data)
	_, _ = c.Writer.Write([]byte("\n\n"))
	writeConsoleVideoChatDone(c)
}

func writeConsoleVideoChatDone(c *gin.Context) {
	_, _ = c.Writer.Write([]byte("data: [DONE]\n\n"))
	c.Writer.Flush()
}

func (h *Handler) createMessage(c *gin.Context) {
	c.Request.Body = http.MaxBytesReader(c.Writer, c.Request.Body, h.maxBodyBytes)
	if !isJSONRequest(c) {
		writeAnthropicError(c, http.StatusUnsupportedMediaType, "invalid_request_error", "Messages only supports application/json")
		return
	}
	if strings.TrimSpace(c.GetHeader("anthropic-version")) == "" {
		writeAnthropicError(c, http.StatusBadRequest, "invalid_request_error", "anthropic-version header is required")
		return
	}
	body, err := io.ReadAll(c.Request.Body)
	if err != nil {
		writeAnthropicError(c, http.StatusRequestEntityTooLarge, "invalid_request_error", "request body exceeds the configured limit")
		return
	}
	var request messagesRequest
	if json.Unmarshal(body, &request) != nil || strings.TrimSpace(request.Model) == "" || request.MaxTokens == nil || *request.MaxTokens <= 0 || len(bytes.TrimSpace(request.Messages)) == 0 {
		writeAnthropicError(c, http.StatusBadRequest, "invalid_request_error", "model, max_tokens, and messages are required")
		return
	}
	clientValue, exists := c.Get(middleware.ClientKey)
	clientKey, ok := clientValue.(clientkeydomain.Key)
	if !exists || !ok {
		writeAnthropicError(c, http.StatusUnauthorized, "authentication_error", "invalid API key")
		return
	}
	requestID, _ := c.Get(middleware.RequestIDKey)
	requestIDValue, _ := requestID.(string)
	result, err := h.gateway.CreateMessage(c.Request.Context(), gateway.Input{
		RequestID: requestIDValue, ClientKey: clientKey, PublicModel: request.Model,
		Body: body, Streaming: request.Stream, PromptCacheKey: request.PromptCacheKey,
		PromptCacheSeed:           extractPromptCacheSeed(c.Request.Header, body),
		AllowClientToolCacheRoute: allowBuildClientToolCacheRoute(c.Request.Header),
		GrokTurnIndex:             c.GetHeader("x-grok-turn-idx"),
	})
	if err != nil {
		writeGatewayAnthropicError(c, err)
		return
	}
	h.writeAnthropicResult(c, result, request.Stream)
}

func (h *Handler) generateImage(c *gin.Context) {
	c.Request.Body = http.MaxBytesReader(c.Writer, c.Request.Body, h.maxBodyBytes)
	if !isJSONRequest(c) {
		writeOpenAIError(c, http.StatusUnsupportedMediaType, "invalid_request", "图片生成仅支持 application/json")
		return
	}
	var request imageGenerationRequest
	if decodeSingleJSON(c.Request.Body, &request, false) != nil || strings.TrimSpace(request.Model) == "" || strings.TrimSpace(request.Prompt) == "" {
		writeOpenAIError(c, http.StatusBadRequest, "invalid_request", "图片请求缺少有效 model 或 prompt")
		return
	}
	if value := bytes.TrimSpace(request.StorageOptions); len(value) > 0 && !bytes.Equal(value, []byte("null")) {
		writeOpenAIError(c, http.StatusBadRequest, "unsupported_parameter", "当前兼容层暂不支持 storage_options")
		return
	}
	count := 1
	if request.Count != nil {
		if *request.Count < 1 || *request.Count > 10 {
			writeOpenAIError(c, http.StatusBadRequest, "invalid_parameter", "n 必须在 1 到 10 之间")
			return
		}
		count = *request.Count
	}
	if request.Stream && count != 1 {
		writeImageGenerationUserError(c, "unsupported_parameter", "input", "Streaming is only supported with n=1.")
		return
	}
	partialImages := 0
	if request.PartialImages != nil {
		if *request.PartialImages < 0 || *request.PartialImages > 3 {
			writeOpenAIError(c, http.StatusBadRequest, "invalid_parameter", "partial_images 必须在 0 到 3 之间")
			return
		}
		partialImages = *request.PartialImages
		if partialImages > 0 && !request.Stream {
			writeOpenAIError(c, http.StatusBadRequest, "invalid_parameter", "partial_images 仅可在 stream=true 时使用")
			return
		}
	}
	clientKey, requestID, ok := requestIdentity(c)
	if !ok {
		return
	}
	result, err := h.gateway.GenerateImage(c.Request.Context(), gateway.ImageGenerationInput{
		RequestID: requestID, ClientKey: clientKey, PublicModel: request.Model, Prompt: request.Prompt,
		Count: count, Size: request.Size, AspectRatio: request.AspectRatio,
		Resolution: request.Resolution, ResponseFormat: request.ResponseFormat,
		Streaming: request.Stream, PartialImages: partialImages,
	})
	if err != nil {
		writeGatewayError(c, err)
		return
	}
	h.writeResult(c, result, request.Stream, streamProtocolImage)
}

func (h *Handler) writeMediaResult(c *gin.Context, result *gateway.Result) {
	errorCode := ""
	defer result.Body.Close()
	defer func() { result.Finalize(gateway.Usage{}, "", errorCode) }()
	if isUpstreamCredentialStatus(result.StatusCode) {
		errorCode = "upstream_unavailable"
		clientCode := readCredentialErrorCode(result.StatusCode, result.Body)
		writeOpenAIError(c, http.StatusServiceUnavailable, clientCode, credentialErrorMessage(clientCode))
		return
	}
	contentLength, contentLengthErr := strconv.ParseInt(result.Header.Get("Content-Length"), 10, 64)
	if contentLengthErr == nil && contentLength > maxMediaResponseTransferBytes {
		errorCode = "response_too_large"
		writeOpenAIError(c, http.StatusBadGateway, "media_too_large", "上游媒体超过 2 GiB 安全上限")
		return
	}
	copyHeaders(c.Writer.Header(), result.Header)
	if contentLengthErr == nil && contentLength >= 0 {
		c.Header("Content-Length", strconv.FormatInt(contentLength, 10))
	} else {
		c.Header("Trailer", mediaTransferErrorTrailer)
	}
	c.Status(result.StatusCode)
	if err := copyMedia(responseDeadlineWriter{ResponseWriter: c.Writer}, result.Body, maxMediaResponseTransferBytes); err != nil {
		if errors.Is(err, errResponseTransferLimit) {
			errorCode = "response_too_large"
		} else {
			errorCode = "stream_interrupted"
		}
		if contentLengthErr != nil {
			c.Header(mediaTransferErrorTrailer, errorCode)
		}
	}
}

type responseDeadlineWriter struct{ http.ResponseWriter }

func (w responseDeadlineWriter) Write(payload []byte) (int, error) {
	if err := setResponseWriteDeadline(w.ResponseWriter); err != nil {
		return 0, err
	}
	return w.ResponseWriter.Write(payload)
}

func setResponseWriteDeadline(writer http.ResponseWriter) error {
	err := http.NewResponseController(writer).SetWriteDeadline(time.Now().Add(responseWriteTimeout))
	if errors.Is(err, http.ErrNotSupported) {
		return nil
	}
	return err
}

func copyMedia(writer io.Writer, source io.Reader, limit int64) error {
	buffer := make([]byte, 64<<10)
	var transferred int64
	for {
		n, readErr := source.Read(buffer)
		if n > 0 {
			remaining := limit - transferred
			if remaining <= 0 {
				return errResponseTransferLimit
			}
			writeSize := n
			if int64(writeSize) > remaining {
				writeSize = int(remaining)
			}
			written, writeErr := writer.Write(buffer[:writeSize])
			transferred += int64(written)
			if writeErr != nil {
				return writeErr
			}
			if written != writeSize {
				return io.ErrShortWrite
			}
			if writeSize != n {
				return errResponseTransferLimit
			}
		}
		if readErr != nil {
			if errors.Is(readErr, io.EOF) {
				return nil
			}
			return readErr
		}
	}
}

func (h *Handler) editImage(c *gin.Context) {
	c.Request.Body = http.MaxBytesReader(c.Writer, c.Request.Body, h.maxBodyBytes)
	if !isJSONRequest(c) {
		writeOpenAIError(c, http.StatusUnsupportedMediaType, "invalid_request", "图片编辑仅支持 application/json")
		return
	}
	var request imageEditJSONRequest
	if err := decodeSingleJSON(c.Request.Body, &request, false); err != nil {
		writeOpenAIError(c, http.StatusBadRequest, "invalid_request", "图片编辑 JSON 请求无效")
		return
	}
	if value := bytes.TrimSpace(request.StorageOptions); len(value) > 0 && !bytes.Equal(value, []byte("null")) {
		writeOpenAIError(c, http.StatusBadRequest, "unsupported_parameter", "当前兼容层暂不支持 storage_options")
		return
	}
	model := strings.TrimSpace(request.Model)
	prompt := strings.TrimSpace(request.Prompt)
	count := 1
	if request.Count != nil {
		count = *request.Count
	}
	inputs := append([]imageEditJSONImage(nil), request.Images...)
	if request.Image != nil {
		inputs = append([]imageEditJSONImage{*request.Image}, inputs...)
	}
	if len(inputs) == 0 || len(inputs) > 8 {
		writeOpenAIError(c, http.StatusBadRequest, "invalid_request", "image 或 images 数量必须在 1 到 8 之间")
		return
	}
	imageURLs := make([]string, 0, len(inputs))
	for _, input := range inputs {
		if strings.TrimSpace(input.FileID) != "" {
			writeOpenAIError(c, http.StatusBadRequest, "unsupported_parameter", "当前暂不支持 image.file_id，请使用 image.url")
			return
		}
		if value := strings.TrimSpace(input.URL); value != "" {
			imageURLs = append(imageURLs, value)
		}
	}
	if len(imageURLs) != len(inputs) {
		writeOpenAIError(c, http.StatusBadRequest, "invalid_request", "每个 image 都必须提供有效 url")
		return
	}
	if model == "" || prompt == "" {
		writeOpenAIError(c, http.StatusBadRequest, "invalid_request", "图片编辑缺少有效 model 或 prompt")
		return
	}
	if count < 1 || count > 10 {
		writeOpenAIError(c, http.StatusBadRequest, "invalid_parameter", "n 必须在 1 到 10 之间")
		return
	}
	partialImages := 0
	if request.PartialImages != nil {
		if *request.PartialImages < 0 || *request.PartialImages > 3 {
			writeOpenAIError(c, http.StatusBadRequest, "invalid_parameter", "partial_images 必须在 0 到 3 之间")
			return
		}
		partialImages = *request.PartialImages
		if partialImages > 0 && !request.Stream {
			writeOpenAIError(c, http.StatusBadRequest, "invalid_parameter", "partial_images 仅可在 stream=true 时使用")
			return
		}
	}
	aspectRatio := strings.ToLower(strings.TrimSpace(request.AspectRatio))
	size := strings.ToLower(strings.TrimSpace(request.Size))
	if aspectRatio != "" && !validImageAspectRatio(aspectRatio) {
		writeOpenAIError(c, http.StatusBadRequest, "invalid_parameter", "aspect_ratio 不受支持")
		return
	}
	if size != "" && !validImageEditSize(size) {
		writeOpenAIError(c, http.StatusBadRequest, "invalid_parameter", "size 必须是 auto、1024x1024、1024x1536 或 1536x1024")
		return
	}
	resolution := strings.ToLower(strings.TrimSpace(request.Resolution))
	if resolution == "" {
		resolution = "1k"
	}
	if resolution != "1k" && resolution != "2k" {
		writeOpenAIError(c, http.StatusBadRequest, "invalid_parameter", "resolution 必须是 1k 或 2k")
		return
	}
	clientKey, requestID, ok := requestIdentity(c)
	if !ok {
		return
	}
	result, err := h.gateway.EditImage(c.Request.Context(), gateway.ImageEditInput{
		RequestID: requestID, ClientKey: clientKey, PublicModel: model, Prompt: prompt,
		ImageURLs: imageURLs, Count: count, Size: size, AspectRatio: aspectRatio,
		Resolution: resolution, ResponseFormat: request.ResponseFormat,
		Streaming: request.Stream, PartialImages: partialImages,
	})
	if err != nil {
		writeGatewayError(c, err)
		return
	}
	h.writeResult(c, result, request.Stream, streamProtocolImage)
}

func requestIdentity(c *gin.Context) (clientkeydomain.Key, string, bool) {
	clientValue, exists := c.Get(middleware.ClientKey)
	clientKey, ok := clientValue.(clientkeydomain.Key)
	if !exists || !ok {
		writeOpenAIError(c, http.StatusUnauthorized, "invalid_api_key", "客户端 API Key 无效")
		return clientkeydomain.Key{}, "", false
	}
	requestID, _ := c.Get(middleware.RequestIDKey)
	requestIDValue, _ := requestID.(string)
	return clientKey, requestIDValue, true
}

func (h *Handler) generateVideo(c *gin.Context) {
	c.Request.Body = http.MaxBytesReader(c.Writer, c.Request.Body, h.maxBodyBytes)
	if !isJSONRequest(c) {
		writeOpenAIError(c, http.StatusUnsupportedMediaType, "invalid_request", "视频生成仅支持 application/json")
		return
	}
	var request videoGenerationRequest
	if err := decodeSingleJSON(c.Request.Body, &request, true); err != nil {
		writeOpenAIError(c, http.StatusBadRequest, "invalid_request", "视频生成 JSON 请求无效: "+err.Error())
		return
	}
	if hasJSONValue(request.Output) {
		writeOpenAIError(c, http.StatusBadRequest, "unsupported_parameter", "当前兼容层暂不支持 output.upload_url")
		return
	}
	if hasJSONValue(request.StorageOptions) {
		writeOpenAIError(c, http.StatusBadRequest, "unsupported_parameter", "当前兼容层暂不支持 storage_options")
		return
	}
	duration, err := parseVideoDuration(request.Duration)
	if err != nil {
		writeOpenAIError(c, http.StatusBadRequest, "invalid_request", err.Error())
		return
	}
	model := strings.TrimSpace(request.Model)
	prompt := strings.TrimSpace(request.Prompt)
	if model == "" {
		writeOpenAIError(c, http.StatusBadRequest, "invalid_request", "视频生成缺少有效 model")
		return
	}
	aspectRatio := strings.TrimSpace(request.AspectRatio)
	if aspectRatio == "" {
		aspectRatio = "16:9"
	}
	if !validVideoAspectRatio(aspectRatio) {
		writeOpenAIError(c, http.StatusBadRequest, "invalid_request", "aspect_ratio 必须是 1:1、16:9、9:16、4:3、3:4、3:2 或 2:3")
		return
	}
	resolution := strings.ToLower(strings.TrimSpace(request.Resolution))
	if resolution == "" {
		resolution = "720p"
	}
	if resolution != "480p" && resolution != "720p" && resolution != "1080p" {
		writeOpenAIError(c, http.StatusBadRequest, "invalid_request", "resolution 必须是 480p、720p 或 1080p")
		return
	}
	inputs := append([]videoGenerationImage(nil), request.ReferenceImages...)
	if request.Image != nil {
		inputs = append([]videoGenerationImage{*request.Image}, inputs...)
	}
	if len(inputs) > mediadomain.MaxInputImages {
		writeOpenAIError(c, http.StatusBadRequest, "invalid_request", fmt.Sprintf("image 与 reference_images 合计不能超过 %d 张", mediadomain.MaxInputImages))
		return
	}
	referenceURLs := make([]string, 0, len(inputs))
	for _, input := range inputs {
		urlValue := strings.TrimSpace(input.URL)
		fileID := strings.TrimSpace(input.FileID)
		if (urlValue == "") == (fileID == "") {
			writeOpenAIError(c, http.StatusBadRequest, "invalid_request", "每个 image 必须且只能提供 url 或 file_id")
			return
		}
		if fileID != "" {
			if !mediadomain.IsInputAssetID(fileID) {
				writeOpenAIError(c, http.StatusBadRequest, "invalid_request", "image.file_id 无效")
				return
			}
			referenceURLs = append(referenceURLs, gateway.VideoInputFileReference(fileID))
		} else {
			referenceURLs = append(referenceURLs, urlValue)
		}
	}
	if prompt == "" && len(referenceURLs) == 0 {
		writeOpenAIError(c, http.StatusBadRequest, "invalid_request", "文本生视频必须提供 prompt；图片生视频可以省略 prompt")
		return
	}
	clientKey, requestID, ok := requestIdentity(c)
	if !ok {
		return
	}
	job, err := h.gateway.CreateVideo(c.Request.Context(), gateway.VideoInput{
		RequestID: requestID, ClientKey: clientKey, PublicModel: model,
		Prompt: prompt, Duration: duration, AspectRatio: aspectRatio, Resolution: resolution,
		ReferenceURLs: referenceURLs,
	})
	if err != nil {
		writeGatewayError(c, err)
		return
	}
	c.JSON(http.StatusOK, gin.H{"request_id": job.ID})
}

func (h *Handler) createCompatibleVideo(c *gin.Context) {
	c.Request.Body = http.MaxBytesReader(c.Writer, c.Request.Body, h.maxBodyBytes)
	if !isJSONRequest(c) {
		writeOpenAIError(c, http.StatusUnsupportedMediaType, "invalid_request", "视频生成仅支持 application/json")
		return
	}
	var request compatibleVideoRequest
	if err := decodeSingleJSON(c.Request.Body, &request, true); err != nil {
		writeOpenAIError(c, http.StatusBadRequest, "invalid_request", "视频生成 JSON 请求无效: "+err.Error())
		return
	}
	model := strings.TrimSpace(request.Model)
	if model == "" {
		writeOpenAIError(c, http.StatusBadRequest, "invalid_request", "视频生成缺少有效 model")
		return
	}
	durationRaw := request.Duration
	if !hasJSONValue(durationRaw) {
		durationRaw = request.Seconds
	}
	duration, err := parseVideoDuration(durationRaw)
	if err != nil {
		writeOpenAIError(c, http.StatusBadRequest, "invalid_request", err.Error())
		return
	}
	aspectRatio := strings.TrimSpace(request.AspectRatio)
	if aspectRatio == "" && strings.TrimSpace(request.Size) != "" {
		aspectRatio, err = aspectRatioFromVideoSize(request.Size)
		if err != nil {
			writeOpenAIError(c, http.StatusBadRequest, "invalid_request", err.Error())
			return
		}
	}
	if aspectRatio == "" {
		aspectRatio = "16:9"
	}
	if !validVideoAspectRatio(aspectRatio) {
		writeOpenAIError(c, http.StatusBadRequest, "invalid_request", "aspect_ratio 无效")
		return
	}
	resolution := strings.ToLower(strings.TrimSpace(request.Resolution))
	if resolution == "" {
		resolution = resolutionFromVideoSize(request.Size)
	}
	if resolution == "" {
		resolution = "720p"
	}
	if resolution != "480p" && resolution != "720p" && resolution != "1080p" {
		writeOpenAIError(c, http.StatusBadRequest, "invalid_request", "resolution 必须是 480p、720p 或 1080p")
		return
	}
	references, err := normalizeCompatibleVideoImages(request)
	if err != nil {
		writeOpenAIError(c, http.StatusBadRequest, "invalid_request", err.Error())
		return
	}
	if strings.TrimSpace(request.Prompt) == "" && len(references) == 0 {
		writeOpenAIError(c, http.StatusBadRequest, "invalid_request", "文本生视频必须提供 prompt；图片生视频可以省略 prompt")
		return
	}
	clientKey, requestID, ok := requestIdentity(c)
	if !ok {
		return
	}
	job, err := h.gateway.CreateVideo(c.Request.Context(), gateway.VideoInput{RequestID: requestID, ClientKey: clientKey, PublicModel: model, Compatibility: true, Prompt: strings.TrimSpace(request.Prompt), Duration: duration, AspectRatio: aspectRatio, Resolution: resolution, ReferenceURLs: references})
	if err != nil {
		writeGatewayError(c, err)
		return
	}
	c.JSON(http.StatusOK, compatibleVideoResponse(job, h.videoContentURL(job.ID)))
}

func normalizeCompatibleVideoImages(request compatibleVideoRequest) ([]string, error) {
	for _, value := range append(append([]string{}, request.Images...), request.ReferenceImages...) {
		if strings.TrimSpace(value) == "" {
			return nil, errors.New("图片引用必须是非空字符串")
		}
	}
	var references []string
	switch {
	case len(request.ReferenceImages) > 0:
		references = append(references, request.ReferenceImages...)
	case len(request.Images) > 0:
		references = append(references, request.Images...)
	case strings.TrimSpace(request.InputReference) != "":
		references = []string{request.InputReference}
	}
	if strings.TrimSpace(request.Image) != "" {
		references = append([]string{strings.TrimSpace(request.Image)}, references...)
	}
	if len(references) > mediadomain.MaxInputImages {
		return nil, fmt.Errorf("参考图片不能超过 %d 张", mediadomain.MaxInputImages)
	}
	for index := range references {
		references[index] = strings.TrimSpace(references[index])
		if references[index] == "" {
			return nil, errors.New("图片引用必须是非空字符串")
		}
	}
	return references, nil
}

func aspectRatioFromVideoSize(value string) (string, error) {
	parts := strings.Split(strings.ToLower(strings.TrimSpace(value)), "x")
	if len(parts) != 2 {
		return "", errors.New("size 必须是 WIDTHxHEIGHT")
	}
	width, err1 := strconv.Atoi(parts[0])
	height, err2 := strconv.Atoi(parts[1])
	if err1 != nil || err2 != nil || width < 1 || height < 1 || width > 16384 || height > 16384 {
		return "", errors.New("size 必须包含合理的正整数宽高")
	}
	divisor := gcd(width, height)
	return strconv.Itoa(width/divisor) + ":" + strconv.Itoa(height/divisor), nil
}

func resolutionFromVideoSize(value string) string {
	parts := strings.Split(strings.ToLower(strings.TrimSpace(value)), "x")
	if len(parts) != 2 {
		return ""
	}
	width, err1 := strconv.Atoi(parts[0])
	height, err2 := strconv.Atoi(parts[1])
	if err1 != nil || err2 != nil || width < 1 || height < 1 {
		return ""
	}
	short := width
	if height < short {
		short = height
	}
	best, bestDistance := 480, abs(short-480)
	for _, candidate := range []int{720, 1080} {
		distance := abs(short - candidate)
		if distance < bestDistance {
			best, bestDistance = candidate, distance
		}
	}
	return strconv.Itoa(best) + "p"
}
func gcd(left, right int) int {
	for right != 0 {
		left, right = right, left%right
	}
	return left
}
func abs(value int) int {
	if value < 0 {
		return -value
	}
	return value
}

func (h *Handler) getVideo(c *gin.Context) {
	clientKey, _, ok := requestIdentity(c)
	if !ok {
		return
	}
	job, err := h.gateway.GetVideo(c.Request.Context(), strings.TrimSpace(c.Param("requestId")), clientKey)
	if err != nil {
		writeGatewayError(c, err)
		return
	}
	if strings.HasPrefix(job.ID, "video_sora_") {
		c.JSON(http.StatusOK, compatibleVideoResponse(job, h.videoResultURL(job)))
		return
	}
	c.JSON(http.StatusOK, videoGenerationResponse(job, h.videoContentURL(job.ID)))
}

func (h *Handler) videoContentURL(jobID string) string {
	return h.publicURL("/v1/videos/" + url.PathEscape(jobID) + "/content")
}

func (h *Handler) videoResultURL(job mediadomain.Job) string {
	if job.Status == mediadomain.StatusCompleted && strings.TrimSpace(job.ResultAssetID) != "" {
		return h.publicURL("/v1/media/videos/" + url.PathEscape(job.ResultAssetID))
	}
	return h.videoContentURL(job.ID)
}

func (h *Handler) publicURL(path string) string {
	baseURL := h.publicAPIBaseURL
	if h.publicBaseURL != nil {
		baseURL = strings.TrimRight(strings.TrimSpace(h.publicBaseURL()), "/")
	}
	if baseURL == "" {
		return path
	}
	return baseURL + path
}

func (h *Handler) getVideoContent(c *gin.Context) {
	clientKey, _, ok := requestIdentity(c)
	if !ok {
		return
	}
	body, contentType, size, err := h.gateway.OpenVideoContent(c.Request.Context(), strings.TrimSpace(c.Param("requestId")), clientKey)
	if err != nil {
		writeGatewayError(c, err)
		return
	}
	defer func() { _ = body.Close() }()
	if strings.TrimSpace(c.GetHeader("Range")) != "" {
		if seeker, ok := body.(io.ReadSeeker); ok {
			c.Header("Content-Type", contentType)
			c.Header("Content-Disposition", "inline")
			c.Header("Cache-Control", "private, no-store")
			http.ServeContent(c.Writer, c.Request, "video.mp4", time.Time{}, seeker)
			return
		}
	}
	writeVideoContent(c, body, contentType, size)
}

func writeVideoContent(c *gin.Context, body io.Reader, contentType string, size int64) {
	if size > maxMediaResponseTransferBytes {
		writeOpenAIError(c, http.StatusBadGateway, "media_too_large", "上游媒体超过 2 GiB 安全上限")
		return
	}
	c.Header("Content-Type", contentType)
	c.Header("Content-Disposition", "inline")
	c.Header("Cache-Control", "private, no-store")
	c.Header("X-Content-Type-Options", "nosniff")
	if size >= 0 {
		c.Header("Content-Length", strconv.FormatInt(size, 10))
	} else {
		c.Header("Trailer", mediaTransferErrorTrailer)
	}
	c.Status(http.StatusOK)
	if err := copyMedia(responseDeadlineWriter{ResponseWriter: c.Writer}, body, maxMediaResponseTransferBytes); err != nil && size < 0 {
		errorCode := "stream_interrupted"
		if errors.Is(err, errResponseTransferLimit) {
			errorCode = "response_too_large"
		}
		c.Header(mediaTransferErrorTrailer, errorCode)
	}
}

func parseVideoDuration(durationRaw json.RawMessage) (int, error) {
	duration, hasDuration, err := parseOptionalVideoInteger(durationRaw)
	if err != nil {
		return 0, fmt.Errorf("duration 必须是整数或整数字符串")
	}
	value := 8
	if hasDuration {
		value = duration
	}
	if value < 1 || value > 15 {
		return 0, fmt.Errorf("duration 必须在 1 到 15 秒之间")
	}
	return value, nil
}

func parseOptionalVideoInteger(raw json.RawMessage) (int, bool, error) {
	if len(bytes.TrimSpace(raw)) == 0 || bytes.Equal(bytes.TrimSpace(raw), []byte("null")) {
		return 0, false, nil
	}
	var number int
	if json.Unmarshal(raw, &number) != nil {
		var text string
		if json.Unmarshal(raw, &text) != nil {
			return 0, true, errors.New("必须是整数或整数字符串")
		}
		parsed, err := strconv.Atoi(strings.TrimSpace(text))
		if err != nil {
			return 0, true, errors.New("必须是整数或整数字符串")
		}
		number = parsed
	}
	return number, true, nil
}

func hasJSONValue(value json.RawMessage) bool {
	trimmed := bytes.TrimSpace(value)
	return len(trimmed) > 0 && !bytes.Equal(trimmed, []byte("null"))
}

func validVideoAspectRatio(value string) bool {
	switch value {
	case "1:1", "16:9", "9:16", "4:3", "3:4", "3:2", "2:3":
		return true
	default:
		return false
	}
}

func validImageAspectRatio(value string) bool {
	switch value {
	case "auto", "1:1", "16:9", "9:16", "4:3", "3:4", "3:2", "2:3", "2:1", "1:2", "19.5:9", "9:19.5", "20:9", "9:20":
		return true
	default:
		return false
	}
}

func validImageEditSize(value string) bool {
	switch value {
	case "auto", "1024x1024", "1024x1536", "1536x1024":
		return true
	default:
		return false
	}
}

func videoGenerationResponse(job mediadomain.Job, contentURLs ...string) gin.H {
	switch job.Status {
	case mediadomain.StatusCompleted:
		videoURL := job.UpstreamURL
		if len(contentURLs) > 0 && contentURLs[0] != "" {
			videoURL = contentURLs[0]
		}
		return gin.H{
			"status": "done", "model": job.Model, "progress": 100,
			"video": gin.H{"url": videoURL, "duration": job.Seconds, "respect_moderation": true},
		}
	case mediadomain.StatusFailed:
		return gin.H{
			"status": "failed",
			"error":  gin.H{"code": officialVideoErrorCode(job.ErrorCode), "message": job.ErrorMessage},
		}
	default:
		return gin.H{"status": "pending", "model": job.Model, "progress": min(99, max(0, job.Progress))}
	}
}

func compatibleVideoResponse(job mediadomain.Job, contentURL string) gin.H {
	response := gin.H{"id": job.ID, "task_id": job.ID, "object": "video", "model": job.Model, "status": "queued", "progress": max(0, min(99, job.Progress)), "created_at": job.CreatedAt.Unix(), "seconds": strconv.Itoa(job.Seconds), "size": job.Size}
	switch job.Status {
	case mediadomain.StatusInProgress:
		response["status"] = "in_progress"
	case mediadomain.StatusCompleted:
		response["status"] = "completed"
		response["progress"] = 100
		response["metadata"] = gin.H{"url": contentURL}
		if job.CompletedAt != nil {
			response["completed_at"] = job.CompletedAt.Unix()
		}
	case mediadomain.StatusFailed:
		response["status"] = "failed"
		response["progress"] = 0
		response["error"] = gin.H{"code": "video_generation_failed", "message": job.ErrorMessage}
	}
	return response
}

func officialVideoErrorCode(value string) string {
	switch value {
	case "account_unavailable", "provider_unavailable":
		return "service_unavailable"
	case "model_not_found":
		return "invalid_argument"
	default:
		return "internal_error"
	}
}

func (h *Handler) handleCreate(c *gin.Context, compact bool) {
	c.Request.Body = http.MaxBytesReader(c.Writer, c.Request.Body, h.maxBodyBytes)
	if !isJSONRequest(c) {
		writeOpenAIError(c, http.StatusUnsupportedMediaType, "invalid_request", "Responses only supports application/json")
		return
	}
	body, err := io.ReadAll(c.Request.Body)
	if err != nil {
		writeOpenAIError(c, http.StatusRequestEntityTooLarge, "request_too_large", "请求体超过限制")
		return
	}
	var request responsesRequest
	if err := json.Unmarshal(body, &request); err != nil || strings.TrimSpace(request.Model) == "" {
		writeOpenAIError(c, http.StatusBadRequest, "invalid_request", "Responses 请求缺少有效 model")
		return
	}
	if compact {
		body, err = forceJSONBoolean(body, "stream", false)
		if err != nil {
			writeOpenAIError(c, http.StatusBadRequest, "invalid_request", "Compact 请求格式无效")
			return
		}
		request.Stream = false
	}
	clientValue, exists := c.Get(middleware.ClientKey)
	clientKey, ok := clientValue.(clientkeydomain.Key)
	if !exists || !ok {
		writeOpenAIError(c, http.StatusUnauthorized, "invalid_api_key", "客户端 API Key 无效")
		return
	}
	requestID, _ := c.Get(middleware.RequestIDKey)
	requestIDValue, _ := requestID.(string)
	input := gateway.Input{
		RequestID: requestIDValue, ClientKey: clientKey, PublicModel: request.Model,
		Body: body, Streaming: request.Stream, PromptCacheKey: request.PromptCacheKey,
		PromptCacheSeed: extractPromptCacheSeed(c.Request.Header, body), PreviousResponseID: request.PreviousResponseID,
		AllowClientToolCacheRoute: allowBuildClientToolCacheRoute(c.Request.Header),
		GrokTurnIndex:             c.GetHeader("x-grok-turn-idx"),
	}
	var result *gateway.Result
	if compact {
		result, err = h.gateway.CompactResponse(c.Request.Context(), input)
	} else {
		result, err = h.gateway.CreateResponse(c.Request.Context(), input)
	}
	if err != nil {
		writeGatewayError(c, err)
		return
	}
	h.writeResult(c, result, request.Stream && !compact, streamProtocolResponses)
}

func isJSONRequest(c *gin.Context) bool {
	mediaType, _, err := mime.ParseMediaType(c.GetHeader("Content-Type"))
	return err == nil && strings.EqualFold(mediaType, "application/json")
}

func decodeSingleJSON(reader io.Reader, target any, disallowUnknown bool) error {
	decoder := json.NewDecoder(reader)
	if disallowUnknown {
		decoder.DisallowUnknownFields()
	}
	if err := decoder.Decode(target); err != nil {
		return err
	}
	if err := decoder.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		if err == nil {
			return errors.New("请求体只能包含一个 JSON 对象")
		}
		return err
	}
	return nil
}

func (h *Handler) getResponse(c *gin.Context) {
	h.handleOwnedResource(c, false)
}

func (h *Handler) deleteResponse(c *gin.Context) {
	h.handleOwnedResource(c, true)
}

func (h *Handler) handleOwnedResource(c *gin.Context, deleteResource bool) {
	clientValue, exists := c.Get(middleware.ClientKey)
	clientKey, ok := clientValue.(clientkeydomain.Key)
	if !exists || !ok {
		writeOpenAIError(c, http.StatusUnauthorized, "invalid_api_key", "客户端 API Key 无效")
		return
	}
	input := gateway.ResourceInput{ClientKey: clientKey, ResponseID: strings.TrimSpace(c.Param("responseId")), RawQuery: c.Request.URL.RawQuery}
	if input.ResponseID == "" {
		writeOpenAIError(c, http.StatusBadRequest, "invalid_request", "response_id 不能为空")
		return
	}
	var result *gateway.Result
	var err error
	if deleteResource {
		result, err = h.gateway.DeleteResponse(c.Request.Context(), input)
	} else {
		result, err = h.gateway.GetResponse(c.Request.Context(), input)
	}
	if err != nil {
		writeGatewayError(c, err)
		return
	}
	h.writeResult(c, result, false, streamProtocolResponses)
}

func (h *Handler) writeResult(c *gin.Context, result *gateway.Result, stream bool, protocol streamProtocol) {
	h.writeProtocolResult(c, result, stream, false, protocol)
}

func (h *Handler) writeAnthropicResult(c *gin.Context, result *gateway.Result, stream bool) {
	h.writeProtocolResult(c, result, stream, true, streamProtocolAnthropic)
}

func (h *Handler) writeProtocolResult(c *gin.Context, result *gateway.Result, stream, anthropic bool, protocol streamProtocol) {
	usage := gateway.Usage{}
	responseID := ""
	errorCode := ""
	defer result.Body.Close()
	defer func() { result.Finalize(usage, responseID, errorCode) }()
	if isUpstreamCredentialStatus(result.StatusCode) {
		errorCode = "upstream_unavailable"
		clientCode := readCredentialErrorCode(result.StatusCode, result.Body)
		if anthropic {
			writeAnthropicError(c, http.StatusServiceUnavailable, "overloaded_error", credentialErrorMessage(clientCode), clientCode)
		} else {
			writeOpenAIError(c, http.StatusServiceUnavailable, clientCode, credentialErrorMessage(clientCode))
		}
		return
	}
	transferLimit := int64(maxJSONResponseTransferBytes)
	if stream {
		transferLimit = maxStreamResponseTransferBytes
	}
	if contentLength, parseErr := strconv.ParseInt(result.Header.Get("Content-Length"), 10, 64); parseErr == nil && contentLength > transferLimit {
		errorCode = "response_too_large"
		writeOpenAIError(c, http.StatusBadGateway, "response_too_large", "上游响应超过代理安全上限")
		return
	}
	copyHeaders(c.Writer.Header(), result.Header)
	c.Status(result.StatusCode)
	if result.StatusCode >= 400 {
		errorCode = "upstream_error"
	}
	var err error
	if stream {
		metadata, copyErr := copyStream(c.Writer, result.Body, protocol, result.MarkFirstToken)
		usage, responseID, err = metadata.Usage, metadata.ResponseID, copyErr
		if metadata.StreamFailure != nil && result.RecordStreamFailure != nil {
			result.RecordStreamFailure(*metadata.StreamFailure)
		}
	} else {
		metadata, copyErr := copyJSON(c.Writer, result.Body, protocol)
		usage, responseID, err = metadata.Usage, metadata.ResponseID, copyErr
	}
	if err != nil {
		switch {
		case errors.Is(err, errResponseTransferLimit):
			errorCode = "response_too_large"
		case errors.Is(err, errUpstreamStreamFailed):
			errorCode = "upstream_stream_error"
		case errors.Is(err, errUpstreamStreamIncomplete):
			errorCode = "upstream_stream_incomplete"
		case errors.Is(err, neterror.ErrUpstreamStreamIdleTimeout):
			errorCode = "upstream_stream_idle_timeout"
		case errors.Is(err, errUpstreamStreamRead):
			errorCode = "upstream_stream_interrupted"
		default:
			errorCode = "stream_interrupted"
		}
	}
}

type responseMetadata struct {
	Usage                    gateway.Usage
	cacheCreationInputTokens int64
	ResponseID               string
	Model                    string
	StreamFailure            *gateway.StreamFailureDiagnostic
}

func copyStream(writer gin.ResponseWriter, source io.Reader, protocol streamProtocol, onFirstToken func()) (responseMetadata, error) {
	inspector := &responseInspector{protocol: protocol, onFirstToken: onFirstToken}
	buffer := make([]byte, responseCopyBufferBytes)
	transferred := 0
	for {
		n, readErr := source.Read(buffer)
		if n > 0 {
			if transferred+n > maxStreamResponseTransferBytes {
				return inspector.Metadata(), fmt.Errorf("%w: 流式响应超过 %d MiB", errResponseTransferLimit, maxStreamResponseTransferBytes>>20)
			}
			chunk := buffer[:n]
			inspector.Inspect(chunk)
			if err := setResponseWriteDeadline(writer); err != nil {
				return inspector.Metadata(), err
			}
			if _, err := writer.Write(chunk); err != nil {
				return inspector.Metadata(), err
			}
			writer.Flush()
			inspector.markFirstTokenForwarded()
			transferred += n
		}
		if readErr != nil {
			if errors.Is(readErr, io.EOF) {
				inspector.Finish()
				return inspector.Metadata(), inspector.TerminalError()
			}
			if inspector.terminalSuccess {
				return inspector.Metadata(), nil
			}
			return inspector.Metadata(), fmt.Errorf("%w: %w", errUpstreamStreamRead, readErr)
		}
	}
}

func copyJSON(writer gin.ResponseWriter, source io.Reader, protocol streamProtocol) (responseMetadata, error) {
	buffer := make([]byte, responseCopyBufferBytes)
	metadataBody := make([]byte, 0, responseCopyBufferBytes)
	metadataComplete := true
	transferred := 0
	for {
		n, readErr := source.Read(buffer)
		if n > 0 {
			if transferred+n > maxJSONResponseTransferBytes {
				return responseMetadata{}, fmt.Errorf("%w: 非流式响应超过 %d MiB", errResponseTransferLimit, maxJSONResponseTransferBytes>>20)
			}
			chunk := buffer[:n]
			if err := setResponseWriteDeadline(writer); err != nil {
				return responseMetadata{}, err
			}
			if _, err := writer.Write(chunk); err != nil {
				return responseMetadata{}, err
			}
			transferred += n
			if metadataComplete {
				if len(metadataBody)+len(chunk) <= maxJSONMetadataInspectionBytes {
					metadataBody = append(metadataBody, chunk...)
				} else {
					metadataBody = nil
					metadataComplete = false
				}
			}
		}
		if readErr != nil {
			if errors.Is(readErr, io.EOF) {
				if metadataComplete {
					return normalizeMetadataUsage(extractMetadata(metadataBody), protocol), nil
				}
				return responseMetadata{}, nil
			}
			return responseMetadata{}, readErr
		}
	}
}

type responseInspector struct {
	protocol        streamProtocol
	pending         []byte
	metadata        responseMetadata
	onFirstToken    func()
	firstTokenSeen  bool
	firstTokenReady bool
	terminalSuccess bool
	terminalFailure bool
}

func (i *responseInspector) Inspect(chunk []byte) {
	i.pending = append(i.pending, chunk...)
	for {
		index := bytes.IndexByte(i.pending, '\n')
		if index < 0 {
			if len(i.pending) > maxStreamEventInspectionBytes {
				i.pending = nil
			}
			return
		}
		line := bytes.TrimSpace(i.pending[:index])
		i.pending = i.pending[index+1:]
		if bytes.HasPrefix(line, []byte("data:")) {
			value := bytes.TrimSpace(bytes.TrimPrefix(line, []byte("data:")))
			i.observeFirstToken(value)
			i.observeTerminal(value)
			if !bytes.Equal(value, []byte("[DONE]")) {
				metadata := extractMetadata(value)
				if hasUsageSignal(metadata.Usage) {
					if metadata.Usage.ResponseModel == "" {
						metadata.Usage.ResponseModel = i.metadata.Model
					}
					i.metadata.Usage = mergeGatewayUsage(i.metadata.Usage, metadata.Usage)
				}
				if metadata.ResponseID != "" {
					i.metadata.ResponseID = metadata.ResponseID
				}
				if metadata.Model != "" {
					i.metadata.Model = metadata.Model
					i.metadata.Usage.ResponseModel = metadata.Model
				}
				if metadata.cacheCreationInputTokens > 0 {
					i.metadata.cacheCreationInputTokens = metadata.cacheCreationInputTokens
				}
			}
		}
	}
}

func (i *responseInspector) observeFirstToken(data []byte) {
	if i.firstTokenSeen || i.firstTokenReady || i.onFirstToken == nil || len(data) == 0 || bytes.Equal(data, []byte("[DONE]")) {
		return
	}
	if !containsGeneratedDelta(data, i.protocol) {
		return
	}
	i.firstTokenReady = true
}

func (i *responseInspector) markFirstTokenForwarded() {
	if i.firstTokenSeen || !i.firstTokenReady || i.onFirstToken == nil {
		return
	}
	i.firstTokenReady = false
	i.firstTokenSeen = true
	i.onFirstToken()
	i.onFirstToken = nil
}

func containsGeneratedDelta(data []byte, protocol streamProtocol) bool {
	switch protocol {
	case streamProtocolResponses:
		var event struct {
			Type  string `json:"type"`
			Delta string `json:"delta"`
		}
		if json.Unmarshal(data, &event) != nil || event.Delta == "" {
			return false
		}
		switch event.Type {
		case "response.output_text.delta", "response.reasoning_summary_text.delta", "response.reasoning_text.delta", "response.refusal.delta", "response.function_call_arguments.delta", "response.custom_tool_call_input.delta":
			return true
		}
	case streamProtocolChat:
		var event struct {
			Choices []struct {
				Delta struct {
					Content          string `json:"content"`
					Reasoning        string `json:"reasoning"`
					ReasoningContent string `json:"reasoning_content"`
					Refusal          string `json:"refusal"`
					ToolCalls        []struct {
						Function struct {
							Arguments string `json:"arguments"`
						} `json:"function"`
					} `json:"tool_calls"`
				} `json:"delta"`
			} `json:"choices"`
		}
		if json.Unmarshal(data, &event) != nil {
			return false
		}
		for _, choice := range event.Choices {
			delta := choice.Delta
			if delta.Content != "" || delta.Reasoning != "" || delta.ReasoningContent != "" || delta.Refusal != "" {
				return true
			}
			for _, call := range delta.ToolCalls {
				if call.Function.Arguments != "" {
					return true
				}
			}
		}
	case streamProtocolAnthropic:
		var event struct {
			Type  string `json:"type"`
			Delta struct {
				Type        string `json:"type"`
				Text        string `json:"text"`
				Thinking    string `json:"thinking"`
				PartialJSON string `json:"partial_json"`
			} `json:"delta"`
		}
		if json.Unmarshal(data, &event) != nil || event.Type != "content_block_delta" {
			return false
		}
		switch event.Delta.Type {
		case "text_delta":
			return event.Delta.Text != ""
		case "thinking_delta":
			return event.Delta.Thinking != ""
		case "input_json_delta":
			return event.Delta.PartialJSON != ""
		}
	}
	return false
}

func (i *responseInspector) Metadata() responseMetadata {
	return normalizeMetadataUsage(i.metadata, i.protocol)
}

func normalizeMetadataUsage(metadata responseMetadata, protocol streamProtocol) responseMetadata {
	if protocol != streamProtocolAnthropic {
		return metadata
	}
	inputTokens := saturatingUsageSum(metadata.Usage.InputTokens, metadata.Usage.CachedInputTokens, metadata.cacheCreationInputTokens)
	metadata.Usage.InputTokens = inputTokens
	metadata.Usage.TotalTokens = saturatingUsageSum(inputTokens, metadata.Usage.OutputTokens)
	return metadata
}

func saturatingUsageSum(values ...int64) int64 {
	var total int64
	for _, value := range values {
		if value <= 0 {
			continue
		}
		if value > math.MaxInt64-total {
			return math.MaxInt64
		}
		total += value
	}
	return total
}

func (i *responseInspector) TerminalError() error {
	if i.terminalFailure {
		return errUpstreamStreamFailed
	}
	if !i.terminalSuccess {
		return errUpstreamStreamIncomplete
	}
	return nil
}

func (i *responseInspector) observeTerminal(data []byte) {
	if bytes.Equal(data, []byte("[DONE]")) {
		if i.protocol == streamProtocolChat {
			i.terminalSuccess = true
		}
		return
	}
	var payload struct {
		Type string `json:"type"`
	}
	if json.Unmarshal(data, &payload) != nil {
		return
	}
	switch i.protocol {
	case streamProtocolResponses:
		switch payload.Type {
		case "response.completed":
			i.terminalSuccess = true
		case "response.failed", "response.incomplete", "response.error", "error":
			i.markTerminalFailure(data)
		}
	case streamProtocolChat:
		if payload.Type == "error" {
			i.markTerminalFailure(data)
		}
	case streamProtocolAnthropic:
		switch payload.Type {
		case "message_stop":
			i.terminalSuccess = true
		case "error":
			i.markTerminalFailure(data)
		}
	case streamProtocolImage:
		switch payload.Type {
		case "image_generation.completed":
			i.terminalSuccess = true
		case "image_generation.failed", "error":
			i.markTerminalFailure(data)
		}
	}
}

func (i *responseInspector) markTerminalFailure(data []byte) {
	i.terminalFailure = true
	if i.metadata.StreamFailure != nil {
		return
	}
	diagnostic := projectStreamFailureDiagnostic(data)
	if len(diagnostic.Body) > 0 {
		i.metadata.StreamFailure = &diagnostic
	}
}

func projectStreamFailureDiagnostic(data []byte) gateway.StreamFailureDiagnostic {
	var root map[string]json.RawMessage
	if json.Unmarshal(data, &root) != nil {
		return gateway.StreamFailureDiagnostic{}
	}
	projected := make(map[string]json.RawMessage)
	copySafeDiagnosticFields(projected, root, "type", "status", "code", "message", "param")
	if raw := projectSafeErrorValue(root["error"]); len(raw) > 0 {
		projected["error"] = raw
	}
	if responseRaw := root["response"]; len(responseRaw) > 0 {
		var response map[string]json.RawMessage
		if json.Unmarshal(responseRaw, &response) == nil {
			safeResponse := make(map[string]json.RawMessage)
			copySafeDiagnosticFields(safeResponse, response, "id", "status", "code", "message")
			if raw := projectSafeErrorValue(response["error"]); len(raw) > 0 {
				safeResponse["error"] = raw
			}
			if raw := projectSafeErrorValue(response["incomplete_details"]); len(raw) > 0 {
				safeResponse["incomplete_details"] = raw
			}
			if len(safeResponse) > 0 {
				if encoded, err := json.Marshal(safeResponse); err == nil {
					projected["response"] = encoded
				}
			}
		}
	}
	if len(projected) == 0 {
		return gateway.StreamFailureDiagnostic{}
	}
	encoded, err := json.Marshal(projected)
	if err != nil {
		return gateway.StreamFailureDiagnostic{}
	}
	diagnostic := gateway.StreamFailureDiagnostic{Body: encoded}
	if len(diagnostic.Body) > maxStreamFailureDiagnosticBytes {
		bounded := diagnostic.Body[:maxStreamFailureDiagnosticBytes]
		for len(bounded) > 0 && !utf8.Valid(bounded) {
			bounded = bounded[:len(bounded)-1]
		}
		diagnostic.Body = append([]byte(nil), bounded...)
		diagnostic.BodyTruncated = true
	} else {
		diagnostic.Body = append([]byte(nil), diagnostic.Body...)
	}
	return diagnostic
}

func copySafeDiagnosticFields(destination, source map[string]json.RawMessage, fields ...string) {
	for _, field := range fields {
		if raw := projectSafeScalar(source[field]); len(raw) > 0 {
			destination[field] = raw
		}
	}
}

func projectSafeErrorValue(raw json.RawMessage) json.RawMessage {
	if scalar := projectSafeScalar(raw); len(scalar) > 0 {
		return scalar
	}
	var value map[string]json.RawMessage
	if json.Unmarshal(raw, &value) != nil {
		return nil
	}
	projected := make(map[string]json.RawMessage)
	copySafeDiagnosticFields(projected, value, "type", "status", "code", "message", "param", "reason")
	if len(projected) == 0 {
		return nil
	}
	encoded, err := json.Marshal(projected)
	if err != nil {
		return nil
	}
	return encoded
}

func projectSafeScalar(raw json.RawMessage) json.RawMessage {
	if len(raw) == 0 {
		return nil
	}
	var value any
	if json.Unmarshal(raw, &value) != nil {
		return nil
	}
	switch value.(type) {
	case nil, string, bool, float64:
		return append(json.RawMessage(nil), raw...)
	default:
		return nil
	}
}

func (i *responseInspector) Finish() {
	if len(i.pending) == 0 {
		return
	}
	i.pending = append(i.pending, '\n')
	i.Inspect(nil)
}

func extractMetadata(data []byte) responseMetadata {
	var root responsePayloadDTO
	if json.Unmarshal(data, &root) != nil {
		return responseMetadata{}
	}
	metadata := responseMetadata{ResponseID: root.ID, Model: root.Model}
	usage := root.Usage
	if root.Response != nil {
		if metadata.ResponseID == "" {
			metadata.ResponseID = root.Response.ID
		}
		if metadata.Model == "" {
			metadata.Model = root.Response.Model
		}
		if usage == nil {
			usage = root.Response.Usage
		}
	}
	if usage == nil {
		return metadata
	}
	metadata.Usage = usage.toGatewayUsage(metadata.Model)
	metadata.cacheCreationInputTokens = usage.CacheCreationInputTokens
	return metadata
}

type responsePayloadDTO struct {
	ID       string              `json:"id"`
	Model    string              `json:"model"`
	Usage    *responseUsageDTO   `json:"usage"`
	Response *responsePayloadDTO `json:"response"`
}

type responseUsageDTO struct {
	InputTokens            int64 `json:"input_tokens"`
	InputTokensCamel       int64 `json:"inputTokens"`
	OutputTokens           int64 `json:"output_tokens"`
	OutputTokensCamel      int64 `json:"outputTokens"`
	TotalTokens            int64 `json:"total_tokens"`
	TotalTokensCamel       int64 `json:"totalTokens"`
	CostInUSDTicks         int64 `json:"cost_in_usd_ticks"`
	NumSourcesUsed         int64 `json:"num_sources_used"`
	NumServerSideToolsUsed int64 `json:"num_server_side_tools_used"`
	// Responses protocol: input_tokens_details.cached_tokens
	InputTokensDetails responseInputDetailsDTO `json:"input_tokens_details"`
	// OpenAI Chat Completions protocol: prompt_tokens_details.cached_tokens
	PromptTokensDetails responseInputDetailsDTO `json:"prompt_tokens_details"`
	// Anthropic Messages protocol: top-level cache_read_input_tokens
	CacheReadInputTokens     int64                    `json:"cache_read_input_tokens"`
	CacheCreationInputTokens int64                    `json:"cache_creation_input_tokens"`
	OutputTokensDetails      responseOutputDetailsDTO `json:"output_tokens_details"`
	// OpenAI Chat Completions protocol: completion_tokens_details.reasoning_tokens
	CompletionTokensDetails responseOutputDetailsDTO  `json:"completion_tokens_details"`
	ContextDetails          responseContextDetailsDTO `json:"context_details"`
	PromptTokens            int64                     `json:"prompt_tokens"`
	CompletionTokens        int64                     `json:"completion_tokens"`
}

type responseInputDetailsDTO struct {
	CachedTokens int64 `json:"cached_tokens"`
}

type responseOutputDetailsDTO struct {
	ReasoningTokens int64 `json:"reasoning_tokens"`
	ThinkingTokens  int64 `json:"thinking_tokens"`
}

type responseContextDetailsDTO struct {
	InputTokens  int64 `json:"input_tokens"`
	OutputTokens int64 `json:"output_tokens"`
}

func (value responseUsageDTO) toGatewayUsage(responseModel string) gateway.Usage {
	input := value.InputTokens
	if input == 0 {
		input = value.InputTokensCamel
	}
	if input == 0 {
		input = value.PromptTokens
	}
	output := value.OutputTokens
	if output == 0 {
		output = value.OutputTokensCamel
	}
	if output == 0 {
		output = value.CompletionTokens
	}
	total := value.TotalTokens
	if total == 0 {
		total = value.TotalTokensCamel
	}
	if total == 0 {
		total = input + output
	}
	// Unified cache hits: Responses / Chat Completions / Anthropic Messages
	cached := value.InputTokensDetails.CachedTokens
	if cached == 0 {
		cached = value.PromptTokensDetails.CachedTokens
	}
	if cached == 0 {
		cached = value.CacheReadInputTokens
	}
	reasoning := value.OutputTokensDetails.ReasoningTokens
	if reasoning == 0 {
		reasoning = value.CompletionTokensDetails.ReasoningTokens
	}
	if reasoning == 0 {
		reasoning = value.OutputTokensDetails.ThinkingTokens
	}
	return gateway.Usage{
		InputTokens: input, CachedInputTokens: cached,
		OutputTokens: output, ReasoningTokens: reasoning,
		TotalTokens: total, CostInUSDTicks: value.CostInUSDTicks,
		NumSourcesUsed: value.NumSourcesUsed, NumServerSideToolsUsed: value.NumServerSideToolsUsed,
		ContextInputTokens: value.ContextDetails.InputTokens, ContextOutputTokens: value.ContextDetails.OutputTokens,
		ResponseModel: responseModel,
	}
}

func hasUsageSignal(usage gateway.Usage) bool {
	return usage.InputTokens > 0 || usage.OutputTokens > 0 || usage.TotalTokens > 0 ||
		usage.CachedInputTokens > 0 || usage.ReasoningTokens > 0 || usage.CostInUSDTicks > 0 ||
		usage.NumSourcesUsed > 0 || usage.NumServerSideToolsUsed > 0 ||
		usage.ContextInputTokens > 0 || usage.ContextOutputTokens > 0
}

// mergeGatewayUsage merges usage from multiple streaming frames; non-zero fields overwrite,
// preventing a later partial frame from erasing an already parsed cache hit.
func mergeGatewayUsage(base, next gateway.Usage) gateway.Usage {
	if next.InputTokens > 0 {
		base.InputTokens = next.InputTokens
	}
	if next.OutputTokens > 0 {
		base.OutputTokens = next.OutputTokens
	}
	if next.TotalTokens > 0 {
		base.TotalTokens = next.TotalTokens
	}
	if next.CachedInputTokens > 0 {
		base.CachedInputTokens = next.CachedInputTokens
	}
	if next.ReasoningTokens > 0 {
		base.ReasoningTokens = next.ReasoningTokens
	}
	if next.CostInUSDTicks > 0 {
		base.CostInUSDTicks = next.CostInUSDTicks
	}
	if next.NumSourcesUsed > 0 {
		base.NumSourcesUsed = next.NumSourcesUsed
	}
	if next.NumServerSideToolsUsed > 0 {
		base.NumServerSideToolsUsed = next.NumServerSideToolsUsed
	}
	if next.ContextInputTokens > 0 {
		base.ContextInputTokens = next.ContextInputTokens
	}
	if next.ContextOutputTokens > 0 {
		base.ContextOutputTokens = next.ContextOutputTokens
	}
	if next.ResponseModel != "" {
		base.ResponseModel = next.ResponseModel
	}
	if base.TotalTokens == 0 && (base.InputTokens > 0 || base.OutputTokens > 0) {
		base.TotalTokens = base.InputTokens + base.OutputTokens
	}
	return base
}

func copyHeaders(destination, source http.Header) {
	excluded := map[string]struct{}{
		"connection": {}, "content-length": {}, "keep-alive": {}, "proxy-authenticate": {},
		"proxy-authorization": {}, "set-cookie": {}, "te": {}, "trailer": {},
		"transfer-encoding": {}, "upgrade": {}, "x-models-etag": {},
	}
	for _, value := range source.Values("Connection") {
		for name := range strings.SplitSeq(value, ",") {
			name = strings.ToLower(strings.TrimSpace(name))
			if name != "" {
				excluded[name] = struct{}{}
			}
		}
	}
	for name, values := range source {
		lower := strings.ToLower(name)
		if _, skip := excluded[lower]; skip {
			continue
		}
		for _, value := range values {
			destination.Add(name, value)
		}
	}
}

func writeOpenAIError(c *gin.Context, status int, code, message string) {
	errorType := "invalid_request_error"
	switch {
	case status == http.StatusUnauthorized:
		errorType = "authentication_error"
	case status == http.StatusTooManyRequests:
		errorType = "rate_limit_error"
	case status >= 500:
		errorType = "server_error"
	}
	c.AbortWithStatusJSON(status, gin.H{"error": gin.H{"message": message, "type": errorType, "code": code, "param": nil}})
}

func writeImageGenerationUserError(c *gin.Context, code, param, message string) {
	c.AbortWithStatusJSON(http.StatusBadRequest, gin.H{"error": gin.H{
		"message": message, "type": "image_generation_user_error", "param": param, "code": code,
	}})
}

func writeGatewayError(c *gin.Context, err error) {
	status, code := http.StatusBadGateway, "upstream_unavailable"
	message := "上游服务暂不可用"
	var upstreamFailure *gateway.UpstreamFailure
	var selectionFailure *gateway.SelectionUnavailableError
	switch {
	case errors.Is(err, gateway.ErrLedgerUnavailable):
		status, code = http.StatusServiceUnavailable, "ledger_unavailable"
		message = gateway.ErrLedgerUnavailable.Error()
	case errors.Is(err, clientkeyapp.ErrBillingLimit):
		status, code = http.StatusTooManyRequests, "billing_limit_exceeded"
		message = clientkeyapp.ErrBillingLimit.Error()
	case errors.Is(err, clientkeyapp.ErrModelNotAllowed):
		status, code = http.StatusForbidden, "model_not_allowed"
		message = clientkeyapp.ErrModelNotAllowed.Error()
	case errors.Is(err, gateway.ErrModelNotFound):
		status, code = http.StatusNotFound, "model_not_found"
		message = "模型不存在"
	case errors.Is(err, gateway.ErrResponseNotFound):
		status, code = http.StatusNotFound, "response_not_found"
		message = "Response 不存在或已过期"
	case errors.Is(err, gateway.ErrResponseStateUnsupported), errors.Is(err, gateway.ErrConversationUnsupported):
		status, code = http.StatusBadRequest, "unsupported_parameter"
		message = err.Error()
	case errors.Is(err, gateway.ErrVideoContentNotReady):
		status, code, message = http.StatusConflict, "video_not_ready", "视频内容尚未可用"
	case errors.Is(err, gateway.ErrVideoInputTooLarge), errors.Is(err, gateway.ErrVideoInputUnavailable):
		status, code = http.StatusBadRequest, "invalid_request"
		message = err.Error()
	case errors.As(err, &upstreamFailure):
		if isSanitizedUpstreamAvailabilityFailure(upstreamFailure) {
			// Gateway mid-tier behavior: never expose upstream upgrade/billing prompts to clients.
			code = upstreamFailure.ClientCredentialErrorCode()
			if upstreamFailure.QuotaExhausted || upstreamFailure.FreeQuotaExhausted || upstreamFailure.HTTPStatus == http.StatusPaymentRequired {
				code = "upstream_unavailable"
			}
			status, message = http.StatusServiceUnavailable, credentialErrorMessage(code)
		} else {
			status, code, message = upstreamFailure.HTTPStatus, upstreamFailure.Code, upstreamFailure.PublicMessage
		}
		if !isUpstreamCredentialStatus(upstreamFailure.HTTPStatus) && upstreamFailure.RetryAfter > 0 {
			c.Header("Retry-After", strconv.FormatInt(max(1, int64(upstreamFailure.RetryAfter.Round(time.Second)/time.Second)), 10))
		}
	case errors.As(err, &selectionFailure):
		status, code, message = selectionErrorResponse(c, selectionFailure)
	case errors.Is(err, gateway.ErrResponseAccountUnavailable), errors.Is(err, gateway.ErrNoAvailableAccount):
		status, code = http.StatusServiceUnavailable, "upstream_unavailable"
		message = "当前没有可用的上游账号"
	}
	writeOpenAIError(c, status, code, message)
}

func writeGatewayAnthropicError(c *gin.Context, err error) {
	status, errorType := http.StatusBadGateway, "api_error"
	message := "上游服务暂不可用"
	clientCode := ""
	var upstreamFailure *gateway.UpstreamFailure
	var selectionFailure *gateway.SelectionUnavailableError
	switch {
	case errors.Is(err, gateway.ErrLedgerUnavailable):
		status, errorType = http.StatusServiceUnavailable, "overloaded_error"
		message = gateway.ErrLedgerUnavailable.Error()
	case errors.Is(err, clientkeyapp.ErrBillingLimit):
		status, errorType = http.StatusTooManyRequests, "rate_limit_error"
		message = clientkeyapp.ErrBillingLimit.Error()
	case errors.Is(err, clientkeyapp.ErrModelNotAllowed):
		status, errorType, clientCode = http.StatusForbidden, "permission_error", "model_not_allowed"
		message = clientkeyapp.ErrModelNotAllowed.Error()
	case errors.Is(err, gateway.ErrModelNotFound):
		status, errorType = http.StatusNotFound, "not_found_error"
		message = "模型不存在"
	case errors.Is(err, gateway.ErrResponseStateUnsupported), errors.Is(err, gateway.ErrConversationUnsupported):
		status, errorType = http.StatusBadRequest, "invalid_request_error"
		message = err.Error()
	case errors.As(err, &upstreamFailure):
		if isSanitizedUpstreamAvailabilityFailure(upstreamFailure) {
			clientCode = upstreamFailure.ClientCredentialErrorCode()
			if upstreamFailure.QuotaExhausted || upstreamFailure.FreeQuotaExhausted || upstreamFailure.HTTPStatus == http.StatusPaymentRequired {
				clientCode = "upstream_unavailable"
			}
			status, errorType, message = http.StatusServiceUnavailable, "overloaded_error", credentialErrorMessage(clientCode)
		} else {
			status, message = upstreamFailure.HTTPStatus, upstreamFailure.PublicMessage
			if upstreamFailure.Code == "upstream_header_timeout" {
				errorType = "timeout_error"
			}
		}
		if !isUpstreamCredentialStatus(upstreamFailure.HTTPStatus) && upstreamFailure.RetryAfter > 0 {
			c.Header("Retry-After", strconv.FormatInt(max(1, int64(upstreamFailure.RetryAfter.Round(time.Second)/time.Second)), 10))
		}
		if status == http.StatusTooManyRequests {
			errorType = "rate_limit_error"
		}
	case errors.As(err, &selectionFailure):
		status, clientCode, message = selectionErrorResponse(c, selectionFailure)
		if status == http.StatusTooManyRequests {
			errorType = "rate_limit_error"
		} else {
			errorType = "overloaded_error"
		}
	case errors.Is(err, gateway.ErrResponseAccountUnavailable), errors.Is(err, gateway.ErrNoAvailableAccount):
		status, errorType = http.StatusServiceUnavailable, "overloaded_error"
		message = "当前没有可用的上游账号"
	}
	writeAnthropicError(c, status, errorType, message, clientCode)
}

func isUpstreamCredentialStatus(status int) bool {
	// Include 402 so official "add credits / upgrade SuperGrok" bodies never reach clients (Grok CLI, etc.).
	return status == http.StatusUnauthorized || status == http.StatusForbidden || status == http.StatusPaymentRequired
}

func isSanitizedUpstreamAvailabilityFailure(failure *gateway.UpstreamFailure) bool {
	return failure != nil && (isUpstreamCredentialStatus(failure.HTTPStatus) || failure.QuotaExhausted || failure.FreeQuotaExhausted)
}

func selectionErrorResponse(c *gin.Context, failure *gateway.SelectionUnavailableError) (int, string, string) {
	status, code, message := http.StatusServiceUnavailable, "upstream_unavailable", "当前没有可用的上游账号"
	if failure == nil {
		return status, code, message
	}
	status, code = failure.HTTPStatus(), failure.Code()
	if failure.Scope.IsRestricted() {
		message = failure.Error()
	} else {
		switch failure.Reason {
		case gateway.SelectionCooling:
			message = "上游账号正在冷却"
		case gateway.SelectionModelCooling:
			message = "上游账号的目标模型正在冷却"
		case gateway.SelectionQuotaExhausted:
			message = "上游账号额度等待恢复"
		case gateway.SelectionSaturated:
			message = "上游账号当前均达到并发上限"
		case gateway.SelectionUnsupportedModel:
			message = "当前账号池不支持该模型"
		}
	}
	if failure.RetryAfter > 0 {
		seconds := max(int64(1), int64((failure.RetryAfter+time.Second-1)/time.Second))
		c.Header("Retry-After", strconv.FormatInt(seconds, 10))
	}
	return status, code, message
}

func writeAnthropicError(c *gin.Context, status int, errorType, message string, errorCode ...string) {
	errorPayload := gin.H{"type": errorType, "message": message}
	if len(errorCode) > 0 && errorCode[0] != "" && errorCode[0] != "upstream_unavailable" {
		errorPayload["code"] = errorCode[0]
	}
	c.AbortWithStatusJSON(status, gin.H{"type": "error", "error": errorPayload})
}

func readCredentialErrorCode(status int, source io.Reader) string {
	body, err := io.ReadAll(io.LimitReader(source, maxCredentialErrorInspectBytes+1))
	if err != nil || len(body) > maxCredentialErrorInspectBytes {
		return "upstream_unavailable"
	}
	return gateway.ClientCredentialErrorCodeFromBody(status, body)
}

func credentialErrorMessage(code string) string {
	if code == "permission-denied" {
		return "上游服务暂不可用，聊天端点访问被拒绝"
	}
	return "上游服务暂不可用"
}

func forceJSONBoolean(body []byte, key string, value bool) ([]byte, error) {
	var payload map[string]json.RawMessage
	if err := json.Unmarshal(body, &payload); err != nil {
		return nil, err
	}
	payload[key] = json.RawMessage("false")
	if value {
		payload[key] = json.RawMessage("true")
	}
	return json.Marshal(payload)
}
