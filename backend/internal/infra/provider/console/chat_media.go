package console

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"

	modeldomain "github.com/chenyme/grok2api/backend/internal/domain/model"
	"github.com/chenyme/grok2api/backend/internal/infra/provider"
	"github.com/chenyme/grok2api/backend/internal/infra/provider/conversation"
)

type consoleMediaChatInput struct {
	Stream      bool            `json:"stream"`
	Duration    json.RawMessage `json:"duration"`
	Seconds     json.RawMessage `json:"seconds"`
	Size        string          `json:"size"`
	AspectRatio string          `json:"aspect_ratio"`
	Resolution  string          `json:"resolution"`
	ImageConfig *struct {
		Count          *int   `json:"n"`
		Size           string `json:"size"`
		AspectRatio    string `json:"aspect_ratio"`
		Resolution     string `json:"resolution"`
		ResponseFormat string `json:"response_format"`
	} `json:"image_config"`
	VideoConfig *struct {
		Duration    json.RawMessage `json:"duration"`
		Seconds     json.RawMessage `json:"seconds"`
		Size        string          `json:"size"`
		AspectRatio string          `json:"aspect_ratio"`
		Resolution  string          `json:"resolution"`
	} `json:"video_config"`
}

type consoleMediaChatMessage struct {
	Type    string          `json:"type"`
	Role    string          `json:"role"`
	Content json.RawMessage `json:"content"`
}

func (a *Adapter) forwardMediaChatCompletion(ctx context.Context, request provider.ResponseResourceRequest) (*provider.Response, error) {
	input, prompt, imageURLs, err := parseConsoleMediaChatInput(request.Body, request.Model)
	if err != nil {
		return invalidConversationResponse(conversation.OperationChat, err), nil
	}
	streaming := input.Stream || request.Streaming
	if ResolveMedia(request.Model, modeldomain.CapabilityVideo) {
		return a.forwardVideoChatCompletion(ctx, request, input, prompt, imageURLs, streaming)
	}
	return a.forwardImageChatCompletion(ctx, request, input, prompt, imageURLs, streaming)
}

func parseConsoleMediaChatInput(body []byte, model string) (consoleMediaChatInput, string, []string, error) {
	var input consoleMediaChatInput
	if err := json.Unmarshal(body, &input); err != nil {
		return input, "", nil, fmt.Errorf("解析 Chat Completions 请求: %w", err)
	}
	converted, err := conversation.ConvertRequest(body, model, conversation.OperationChat)
	if err != nil {
		return input, "", nil, err
	}
	var envelope struct {
		Input []json.RawMessage `json:"input"`
	}
	if err := json.Unmarshal(converted, &envelope); err != nil {
		return input, "", nil, fmt.Errorf("解析媒体对话输入: %w", err)
	}
	var prompt string
	var imageURLs []string
	for _, raw := range envelope.Input {
		var message consoleMediaChatMessage
		if json.Unmarshal(raw, &message) != nil || message.Type != "message" || message.Role != "user" {
			continue
		}
		text, images, parseErr := parseConsoleMediaChatContent(message.Content)
		if parseErr != nil {
			return input, "", nil, parseErr
		}
		prompt = text
		imageURLs = images
	}
	if strings.TrimSpace(prompt) == "" && len(imageURLs) == 0 {
		return input, "", nil, errors.New("最后一条 user 消息必须包含文本或图片")
	}
	return input, strings.TrimSpace(prompt), imageURLs, nil
}

func parseConsoleMediaChatContent(raw json.RawMessage) (string, []string, error) {
	var text string
	if json.Unmarshal(raw, &text) == nil {
		return strings.TrimSpace(text), nil, nil
	}
	var parts []map[string]json.RawMessage
	if err := json.Unmarshal(raw, &parts); err != nil {
		return "", nil, errors.New("user 消息 content 必须是字符串或内容数组")
	}
	texts := make([]string, 0, len(parts))
	images := make([]string, 0, len(parts))
	for _, part := range parts {
		var kind string
		_ = json.Unmarshal(part["type"], &kind)
		switch kind {
		case "input_text", "text", "output_text":
			var value string
			if json.Unmarshal(part["text"], &value) != nil {
				return "", nil, errors.New("文本内容无效")
			}
			if value = strings.TrimSpace(value); value != "" {
				texts = append(texts, value)
			}
		case "input_image", "image_url":
			var value string
			if json.Unmarshal(part["image_url"], &value) != nil {
				var image struct {
					URL string `json:"url"`
				}
				if json.Unmarshal(part["image_url"], &image) != nil {
					return "", nil, errors.New("图片内容缺少 image_url")
				}
				value = image.URL
			}
			if value = strings.TrimSpace(value); value == "" {
				return "", nil, errors.New("图片内容缺少 image_url")
			}
			images = append(images, value)
		}
	}
	return strings.Join(texts, "\n"), images, nil
}

func (a *Adapter) forwardImageChatCompletion(ctx context.Context, request provider.ResponseResourceRequest, input consoleMediaChatInput, prompt string, imageURLs []string, streaming bool) (*provider.Response, error) {
	count, format, size, ratio, resolution := consoleImageChatOptions(input)
	var response *provider.Response
	var err error
	if len(imageURLs) > 0 {
		response, err = a.EditImage(ctx, provider.ImageEditRequest{
			Credential: request.Credential, Model: request.Model, Prompt: prompt, ImageURLs: imageURLs,
			Count: count, Size: size, AspectRatio: ratio, Resolution: resolution, ResponseFormat: format,
		})
	} else {
		response, err = a.GenerateImage(ctx, provider.ImageGenerationRequest{
			Credential: request.Credential, Model: request.Model, Prompt: prompt,
			Count: count, Size: size, AspectRatio: ratio, Resolution: resolution, ResponseFormat: format,
		})
	}
	if err != nil || response == nil {
		return response, err
	}
	data, readErr := io.ReadAll(response.Body)
	_ = response.Body.Close()
	if readErr != nil {
		return nil, readErr
	}
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		response.Body = io.NopCloser(bytes.NewReader(data))
		response.Header.Set("Content-Length", strconv.Itoa(len(data)))
		return response, nil
	}
	content, parseErr := consoleImageChatMarkdown(data)
	if parseErr != nil {
		return nil, parseErr
	}
	return consoleChatCompletionResponse(request.Model, content, streaming, response.QuotaUnits), nil
}

func consoleImageChatOptions(input consoleMediaChatInput) (int, string, string, string, string) {
	count := 1
	format := "url"
	size, ratio, resolution := input.Size, input.AspectRatio, input.Resolution
	if input.ImageConfig != nil {
		if input.ImageConfig.Count != nil {
			count = *input.ImageConfig.Count
		}
		if value := strings.TrimSpace(input.ImageConfig.ResponseFormat); value != "" {
			format = value
		}
		if value := strings.TrimSpace(input.ImageConfig.Size); value != "" {
			size = value
		}
		if value := strings.TrimSpace(input.ImageConfig.AspectRatio); value != "" {
			ratio = value
		}
		if value := strings.TrimSpace(input.ImageConfig.Resolution); value != "" {
			resolution = value
		}
	}
	return count, format, size, ratio, resolution
}

func consoleImageChatMarkdown(data []byte) (string, error) {
	var envelope struct {
		Data []map[string]any `json:"data"`
	}
	if err := json.Unmarshal(data, &envelope); err != nil {
		return "", fmt.Errorf("解析 Console 图片响应: %w", err)
	}
	values := make([]string, 0, len(envelope.Data))
	for _, item := range envelope.Data {
		if value, _ := item["url"].(string); strings.TrimSpace(value) != "" {
			values = append(values, "![image]("+strings.TrimSpace(value)+")")
			continue
		}
		if value, _ := item["b64_json"].(string); strings.TrimSpace(value) != "" {
			mimeType, _ := item["mime_type"].(string)
			if mimeType == "" {
				mimeType = "image/jpeg"
			}
			values = append(values, "![image](data:"+mimeType+";base64,"+value+")")
		}
	}
	if len(values) == 0 {
		return "", errors.New("Console 图片响应没有可用图片")
	}
	return strings.Join(values, "\n\n"), nil
}

func (a *Adapter) forwardVideoChatCompletion(ctx context.Context, request provider.ResponseResourceRequest, input consoleMediaChatInput, prompt string, imageURLs []string, streaming bool) (*provider.Response, error) {
	if len(imageURLs) > provider.ConsoleVideoMaxReferenceImages {
		return invalidConversationResponse(conversation.OperationChat, fmt.Errorf("Console grok-imagine-video 最多支持 %d 张参考图", provider.ConsoleVideoMaxReferenceImages)), nil
	}
	duration, ratio, resolution, err := consoleVideoChatOptions(input)
	if err != nil {
		return invalidConversationResponse(conversation.OperationChat, err), nil
	}
	result, err := a.GenerateVideo(ctx, provider.VideoRequest{
		Credential: request.Credential, Billing: request.Billing, Prompt: prompt, Duration: duration,
		AspectRatio: ratio, Resolution: resolution, ReferenceURLs: imageURLs,
	})
	if err != nil {
		return nil, err
	}
	publicURL, err := a.localizeConsoleChatVideo(ctx, request, result)
	if err != nil {
		return nil, err
	}
	content := "[播放或下载视频](" + publicURL + ")\n\n" + publicURL
	return consoleChatCompletionResponse(request.Model, content, streaming, duration), nil
}

func consoleVideoChatOptions(input consoleMediaChatInput) (int, string, string, error) {
	durationRaw := input.Duration
	if len(bytes.TrimSpace(durationRaw)) == 0 {
		durationRaw = input.Seconds
	}
	ratio, resolution, size := input.AspectRatio, input.Resolution, input.Size
	if input.VideoConfig != nil {
		if len(bytes.TrimSpace(input.VideoConfig.Duration)) > 0 {
			durationRaw = input.VideoConfig.Duration
		} else if len(bytes.TrimSpace(input.VideoConfig.Seconds)) > 0 {
			durationRaw = input.VideoConfig.Seconds
		}
		if value := strings.TrimSpace(input.VideoConfig.AspectRatio); value != "" {
			ratio = value
		}
		if value := strings.TrimSpace(input.VideoConfig.Resolution); value != "" {
			resolution = value
		}
		if value := strings.TrimSpace(input.VideoConfig.Size); value != "" {
			size = value
		}
	}
	duration := 8
	if len(bytes.TrimSpace(durationRaw)) > 0 && !bytes.Equal(bytes.TrimSpace(durationRaw), []byte("null")) {
		if json.Unmarshal(durationRaw, &duration) != nil {
			var value string
			if json.Unmarshal(durationRaw, &value) != nil {
				return 0, "", "", errors.New("video_config.duration 必须是整数或整数字符串")
			}
			parsed, parseErr := strconv.Atoi(strings.TrimSpace(value))
			if parseErr != nil {
				return 0, "", "", errors.New("video_config.duration 必须是整数或整数字符串")
			}
			duration = parsed
		}
	}
	if duration < 1 || duration > 15 {
		return 0, "", "", errors.New("video_config.duration 必须在 1 到 15 秒之间")
	}
	if strings.TrimSpace(ratio) == "" {
		ratio = consoleVideoRatioFromSize(size)
	}
	if strings.TrimSpace(ratio) == "" {
		ratio = "16:9"
	}
	if strings.TrimSpace(resolution) == "" {
		resolution = "720p"
	}
	return duration, strings.TrimSpace(ratio), strings.ToLower(strings.TrimSpace(resolution)), nil
}

func consoleVideoRatioFromSize(size string) string {
	switch strings.ToLower(strings.TrimSpace(size)) {
	case "16:9", "1280x720", "1920x1080":
		return "16:9"
	case "9:16", "720x1280", "1080x1920":
		return "9:16"
	case "1:1", "1024x1024":
		return "1:1"
	default:
		return ""
	}
}

func (a *Adapter) localizeConsoleChatVideo(ctx context.Context, request provider.ResponseResourceRequest, result provider.VideoResult) (string, error) {
	if a.mediaAssets == nil {
		return "", provider.NewMediaPostProcessingError(provider.MediaPostProcessingStorage, errors.New("视频媒体存储未配置"))
	}
	var lastErr error
	for attempt := 0; attempt < consoleMediaOutputAttempts; attempt++ {
		body, contentType, _, downloadErr := a.DownloadVideo(ctx, request.Credential, result.URL)
		if downloadErr != nil {
			lastErr = provider.NewMediaPostProcessingError(provider.MediaPostProcessingDownload, downloadErr)
		} else {
			asset, saveErr := a.mediaAssets.SaveVideo(ctx, "", contentType, body)
			_ = body.Close()
			if saveErr == nil {
				return a.mediaAssets.PublicVideoURL(asset.ID), nil
			}
			lastErr = provider.NewMediaPostProcessingError(provider.MediaPostProcessingStorage, saveErr)
		}
		if ctx.Err() != nil || attempt+1 >= consoleMediaOutputAttempts {
			break
		}
		timer := time.NewTimer(time.Duration(attempt+1) * 250 * time.Millisecond)
		select {
		case <-ctx.Done():
			timer.Stop()
			return "", ctx.Err()
		case <-timer.C:
		}
	}
	return "", lastErr
}

func consoleChatCompletionResponse(model, content string, streaming bool, quotaUnits int) *provider.Response {
	responseID := newConsoleChatID()
	created := time.Now().Unix()
	if !streaming {
		payload := map[string]any{
			"id": responseID, "object": "chat.completion", "created": created, "model": model,
			"choices": []any{map[string]any{"index": 0, "message": map[string]any{"role": "assistant", "content": content}, "finish_reason": "stop"}},
			"usage":   map[string]any{"prompt_tokens": 0, "completion_tokens": 0, "total_tokens": 0},
		}
		response := jsonProviderResponse(http.StatusOK, payload)
		response.QuotaUnits = quotaUnits
		return response
	}
	chunks := []map[string]any{
		{"id": responseID, "object": "chat.completion.chunk", "created": created, "model": model, "choices": []any{map[string]any{"index": 0, "delta": map[string]any{"role": "assistant"}, "finish_reason": nil}}},
		{"id": responseID, "object": "chat.completion.chunk", "created": created, "model": model, "choices": []any{map[string]any{"index": 0, "delta": map[string]any{"content": content}, "finish_reason": nil}}},
		{"id": responseID, "object": "chat.completion.chunk", "created": created, "model": model, "choices": []any{map[string]any{"index": 0, "delta": map[string]any{}, "finish_reason": "stop"}}},
	}
	var body strings.Builder
	for _, chunk := range chunks {
		data, _ := json.Marshal(chunk)
		body.WriteString("data: ")
		body.Write(data)
		body.WriteString("\n\n")
	}
	body.WriteString("data: [DONE]\n\n")
	value := body.String()
	return &provider.Response{
		StatusCode: http.StatusOK, Status: "200 OK", QuotaUnits: quotaUnits,
		Header: http.Header{"Content-Type": []string{"text/event-stream"}, "Cache-Control": []string{"no-cache"}, "Content-Length": []string{strconv.Itoa(len(value))}},
		Body:   io.NopCloser(strings.NewReader(value)),
	}
}

func newConsoleChatID() string {
	raw := make([]byte, 18)
	if _, err := rand.Read(raw); err != nil {
		return "chatcmpl_" + strconv.FormatInt(time.Now().UnixNano(), 36)
	}
	return "chatcmpl_" + base64.RawURLEncoding.EncodeToString(raw)
}
