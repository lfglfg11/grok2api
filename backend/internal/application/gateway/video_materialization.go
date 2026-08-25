package gateway

import (
	"context"
	"errors"
	"fmt"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/chenyme/grok2api/backend/internal/infra/provider"
)

const videoMaterializedAttempts = 3

func (s *Service) generateVideoWithInputFallback(ctx context.Context, adapter provider.VideoAdapter, request provider.VideoRequest, affinity string, slotHeld bool) (provider.VideoResult, error) {
	result, err := adapter.GenerateVideo(ctx, request)
	if err == nil || !errors.Is(err, provider.ErrVideoInputDownload) {
		return result, err
	}
	references := request.ReferenceURLs
	firstFrame := false
	if len(references) == 0 && strings.TrimSpace(request.ImageURL) != "" {
		references = []string{request.ImageURL}
		firstFrame = true
	}
	if !hasRemoteVideoReference(references) {
		return result, err
	}
	var fetchRemote func(context.Context, string) ([]byte, error)
	if fetcher, ok := adapter.(provider.VideoInputImageFetcher); ok {
		fetchRemote = func(fetchCtx context.Context, rawURL string) ([]byte, error) {
			return fetcher.FetchVideoInputImage(fetchCtx, affinity, rawURL)
		}
	}
	materialized, cleanup, materializeErr := s.materializeRemoteVideoReferences(ctx, references, slotHeld, fetchRemote)
	if materializeErr != nil {
		return provider.VideoResult{}, fmt.Errorf("视频参考图物化失败: %w", materializeErr)
	}
	defer cleanup()
	if firstFrame {
		request.ImageURL = materialized[0]
	} else {
		request.ReferenceURLs = materialized
	}
	for attempt := 0; attempt < videoMaterializedAttempts; attempt++ {
		result, err = adapter.GenerateVideo(ctx, request)
		if err == nil || !errors.Is(err, provider.ErrVideoInputDownload) || attempt+1 >= videoMaterializedAttempts {
			return result, err
		}
		if waitErr := waitVideoMaterializedRetry(ctx, attempt); waitErr != nil {
			return provider.VideoResult{}, waitErr
		}
	}
	return result, err
}
func hasLocalVideoReference(references []string) bool {
	for _, reference := range references {
		if strings.HasPrefix(reference, "input://") {
			return true
		}
	}
	return false
}
func hasRemoteVideoReference(references []string) bool {
	for _, reference := range references {
		parsed, err := url.Parse(strings.TrimSpace(reference))
		if err == nil && parsed.Hostname() != "" && (parsed.Scheme == "http" || parsed.Scheme == "https") {
			return true
		}
	}
	return false
}

func (s *Service) materializeRemoteVideoReferences(ctx context.Context, references []string, slotHeld bool, fetchRemote func(context.Context, string) ([]byte, error)) ([]string, func(), error) {
	if s.mediaAssets == nil {
		return nil, func() {}, ErrVideoInputUnavailable
	}
	releaseSlot := func() {}
	if !slotHeld {
		var err error
		releaseSlot, err = s.acquireVideoMaterializationSlot(ctx)
		if err != nil {
			return nil, func() {}, err
		}
	}
	staged := make([]string, 0, len(references))
	cleanup := func() {
		releaseSlot()
		if len(staged) == 0 {
			return
		}
		cleanupCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if err := s.mediaAssets.ReleaseInputAssets(cleanupCtx, staged); err != nil && s.logger != nil {
			s.logger.Warn("video_materialized_input_release_failed", "error", err)
		}
	}
	internalReferences := make([]string, 0, len(references))
	imported := make(map[string]string, len(references))
	for _, reference := range references {
		parsed, err := url.Parse(strings.TrimSpace(reference))
		if err != nil || parsed.Hostname() == "" || (parsed.Scheme != "http" && parsed.Scheme != "https") {
			internalReferences = append(internalReferences, reference)
			continue
		}
		if internal, ok := imported[reference]; ok {
			internalReferences = append(internalReferences, internal)
			continue
		}
		asset, err := s.mediaAssets.ImportInputImageFromURL(ctx, reference)
		if err != nil && fetchRemote != nil {
			data, fetchErr := fetchRemote(ctx, reference)
			if fetchErr != nil {
				cleanup()
				return nil, func() {}, errors.Join(err, fetchErr)
			}
			asset, err = s.mediaAssets.SaveInputImage(ctx, data)
		}
		if err != nil {
			cleanup()
			return nil, func() {}, err
		}
		internal := VideoInputFileReference(asset.ID)
		imported[reference] = internal
		staged = append(staged, internal)
		internalReferences = append(internalReferences, internal)
	}
	resolved, err := s.resolveVideoInputReferences(ctx, internalReferences, "image")
	if err != nil {
		cleanup()
		return nil, func() {}, fmt.Errorf("解析物化图片: %w", err)
	}
	return resolved, cleanup, nil
}

func (s *Service) acquireVideoMaterializationSlot(ctx context.Context) (func(), error) {
	if s.mediaInputSlots == nil {
		return func() {}, nil
	}
	select {
	case s.mediaInputSlots <- struct{}{}:
		var once sync.Once
		return func() { once.Do(func() { <-s.mediaInputSlots }) }, nil
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

func waitVideoMaterializedRetry(ctx context.Context, attempt int) error {
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
