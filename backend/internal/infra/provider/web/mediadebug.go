package web

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
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
