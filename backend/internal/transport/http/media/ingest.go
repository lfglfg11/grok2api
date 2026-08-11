package media

import (
	"errors"
	"io"
	"net/http"
	"strings"
	"time"

	mediaapp "github.com/chenyme/grok2api/backend/internal/application/media"
	mediadomain "github.com/chenyme/grok2api/backend/internal/domain/media"
	"github.com/chenyme/grok2api/backend/internal/pkg/remotemedia"
	"github.com/chenyme/grok2api/backend/internal/shared/response"
	"github.com/gin-gonic/gin"
)

const (
	ingestMaxImageBytes = remotemedia.MaxImageBytes
	// 独立 bulkhead 防止临时导入与推理流量争抢内存和连接。
	ingestConcurrency = 4
)

type importImageRequest struct {
	URL string `json:"url" binding:"required,max=8192"`
}

// importInputImageFromURL 从管理员提供的 URL 抓取图片并登记到带 TTL 的隐藏输入区。
// 路由在 /api/admin/v1 下，已由 AdminAuth 保护；抓取带 SSRF 防护。
func (h *Handler) importInputImageFromURL(c *gin.Context) {
	if !h.acquireIngest(c) {
		return
	}
	defer h.releaseIngest()
	var request importImageRequest
	if c.ShouldBindJSON(&request) != nil {
		response.Error(c, http.StatusBadRequest, "invalidRequest", "请求参数无效")
		return
	}
	rawURL := strings.TrimSpace(request.URL)
	if len(rawURL) > 8192 {
		response.Error(c, http.StatusBadRequest, "invalidImageURL", "图片 URL 过长")
		return
	}
	asset, err := h.service.ImportInputImageFromURL(c.Request.Context(), rawURL)
	if err != nil {
		switch {
		case errors.Is(err, mediaapp.ErrInputImageTooLarge):
			response.Error(c, http.StatusRequestEntityTooLarge, "imageTooLarge", "图片超过大小上限")
		case errors.Is(err, mediaapp.ErrInputImageURLBlocked):
			response.Error(c, http.StatusBadRequest, "imageURLBlocked", "该地址不允许访问")
		case errors.Is(err, mediaapp.ErrInvalidImage):
			response.Error(c, http.StatusBadRequest, "invalidImage", "图片内容无效或格式不支持（仅 jpeg/png/webp/gif）")
		case errors.Is(err, mediaapp.ErrMediaCapacity):
			response.Error(c, http.StatusInsufficientStorage, "mediaCapacityExceeded", "媒体临时存储容量不足")
		default:
			response.Error(c, http.StatusBadGateway, "imageFetchFailed", "下载图片失败")
		}
		return
	}

	writeIngestedImage(c, asset)
}

// uploadInputImage 接收管理员上传的本地图片文件（multipart 字段名 file），登记到临时输入区。
func (h *Handler) uploadInputImage(c *gin.Context) {
	if !h.acquireIngest(c) {
		return
	}
	defer h.releaseIngest()
	fileHeader, err := c.FormFile("file")
	if err != nil {
		var maxBytesError *http.MaxBytesError
		if errors.As(err, &maxBytesError) {
			response.Error(c, http.StatusRequestEntityTooLarge, "imageTooLarge", "图片超过请求大小上限")
			return
		}
		response.Error(c, http.StatusBadRequest, "invalidRequest", "缺少上传文件")
		return
	}
	if fileHeader.Size > ingestMaxImageBytes {
		response.Error(c, http.StatusRequestEntityTooLarge, "imageTooLarge", "图片超过大小上限")
		return
	}
	src, err := fileHeader.Open()
	if err != nil {
		response.Error(c, http.StatusInternalServerError, "mediaUploadReadFailed", "读取上传文件失败")
		return
	}
	defer src.Close()
	data, err := io.ReadAll(io.LimitReader(src, ingestMaxImageBytes+1))
	if err != nil {
		response.Error(c, http.StatusInternalServerError, "mediaUploadReadFailed", "读取上传文件失败")
		return
	}
	if int64(len(data)) > ingestMaxImageBytes {
		response.Error(c, http.StatusRequestEntityTooLarge, "imageTooLarge", "图片超过大小上限")
		return
	}
	h.saveIngestedImage(c, data)
}

// saveIngestedImage 收口两种临时输入路径：校验、落盘并登记 TTL，不进入图库。
func (h *Handler) saveIngestedImage(c *gin.Context, data []byte) {
	asset, err := h.service.SaveInputImage(c.Request.Context(), data)
	if errors.Is(err, mediaapp.ErrInvalidImage) {
		response.Error(c, http.StatusBadRequest, "invalidImage", "图片内容无效或格式不支持（仅 jpeg/png/webp/gif）")
		return
	}
	if err != nil {
		if errors.Is(err, mediaapp.ErrMediaCapacity) {
			response.Error(c, http.StatusInsufficientStorage, "mediaCapacityExceeded", "媒体临时存储容量不足")
			return
		}
		response.Error(c, http.StatusInternalServerError, "mediaSaveImageFailed", "保存图片失败")
		return
	}
	writeIngestedImage(c, asset)
}

func writeIngestedImage(c *gin.Context, asset mediadomain.Asset) {
	expiresAt := ""
	if asset.ExpiresAt != nil {
		expiresAt = asset.ExpiresAt.Format(time.RFC3339)
	}
	response.Success(c, http.StatusCreated, gin.H{
		"fileId": asset.ID, "mimeType": asset.MIMEType, "sizeBytes": asset.SizeBytes, "expiresAt": expiresAt,
	})
}

func (h *Handler) acquireIngest(c *gin.Context) bool {
	select {
	case h.ingestSlots <- struct{}{}:
		return true
	default:
		response.Error(c, http.StatusServiceUnavailable, "mediaIngestBusy", "图片暂存并发已满，请稍后重试")
		return false
	}
}

func (h *Handler) releaseIngest() { <-h.ingestSlots }
