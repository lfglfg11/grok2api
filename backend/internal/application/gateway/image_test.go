package gateway

import (
	"context"
	"io"
	"net/http"
	"strings"
	"testing"

	accountdomain "github.com/chenyme/grok2api/backend/internal/domain/account"
	"github.com/chenyme/grok2api/backend/internal/infra/provider"
)

func TestEditImageForwardsURLBeforeMaterializationFallback(t *testing.T) {
	const remoteURL = "https://images.example.com/reference.png"
	store := &videoAssetStoreStub{imported: map[string][]byte{remoteURL: []byte("png-bytes")}}
	service := &Service{mediaAssets: store}
	service.ConfigureMedia(nil, 4)
	adapter := &imageEditFallbackAdapter{inputFailures: 1}
	request := provider.ImageEditRequest{ImageURLs: []string{remoteURL}}

	response, err := service.editImageWithMaterializationFallback(context.Background(), adapter, request, "request-id")
	if err != nil {
		t.Fatal(err)
	}
	if response == nil || response.StatusCode != http.StatusOK || adapter.calls != 2 {
		t.Fatalf("response=%#v calls=%d", response, adapter.calls)
	}
	if adapter.references[0][0] != remoteURL {
		t.Fatalf("first request did not forward URL: %#v", adapter.references)
	}
	if !strings.HasPrefix(adapter.references[1][0], "data:image/png;base64,") {
		t.Fatalf("fallback request was not materialized: %#v", adapter.references)
	}
	if store.importCalls != 1 || store.releaseCalls != 1 {
		t.Fatalf("imports=%d releases=%d", store.importCalls, store.releaseCalls)
	}
}

func TestEditImageDoesNotMaterializeOrdinaryUpstreamFailure(t *testing.T) {
	const remoteURL = "https://images.example.com/reference.png"
	store := &videoAssetStoreStub{imported: map[string][]byte{remoteURL: []byte("png-bytes")}}
	service := &Service{mediaAssets: store}
	adapter := &imageEditFallbackAdapter{ordinaryFailure: true}

	response, err := service.editImageWithMaterializationFallback(context.Background(), adapter, provider.ImageEditRequest{ImageURLs: []string{remoteURL}}, "request-id")
	if err != nil {
		t.Fatal(err)
	}
	if response == nil || response.StatusCode != http.StatusBadRequest || adapter.calls != 1 || store.importCalls != 0 {
		t.Fatalf("response=%#v calls=%d imports=%d", response, adapter.calls, store.importCalls)
	}
}

func TestEditImageRetriesMaterializedInputThreeTimes(t *testing.T) {
	const remoteURL = "https://images.example.com/reference.png"
	store := &videoAssetStoreStub{imported: map[string][]byte{remoteURL: []byte("png-bytes")}}
	service := &Service{mediaAssets: store}
	service.ConfigureMedia(nil, 4)
	adapter := &imageEditFallbackAdapter{inputFailures: 3}

	response, err := service.editImageWithMaterializationFallback(context.Background(), adapter, provider.ImageEditRequest{ImageURLs: []string{remoteURL}}, "request-id")
	if err != nil {
		t.Fatal(err)
	}
	if response == nil || response.StatusCode != http.StatusOK || adapter.calls != 4 {
		t.Fatalf("response=%#v calls=%d", response, adapter.calls)
	}
	if adapter.references[0][0] != remoteURL {
		t.Fatalf("first request did not forward URL: %#v", adapter.references)
	}
	for attempt := 1; attempt < len(adapter.references); attempt++ {
		if !strings.HasPrefix(adapter.references[attempt][0], "data:image/png;base64,") {
			t.Fatalf("fallback attempt %d was not materialized: %#v", attempt, adapter.references)
		}
	}
}

type imageEditFallbackAdapter struct {
	inputFailures   int
	ordinaryFailure bool
	calls           int
	references      [][]string
}

func (*imageEditFallbackAdapter) Provider() accountdomain.Provider {
	return accountdomain.ProviderConsole
}

func (a *imageEditFallbackAdapter) EditImage(_ context.Context, request provider.ImageEditRequest) (*provider.Response, error) {
	a.calls++
	a.references = append(a.references, append([]string(nil), request.ImageURLs...))
	if a.ordinaryFailure {
		return imageFailureResponse(`{"error":{"message":"invalid prompt"}}`), nil
	}
	if a.calls <= a.inputFailures {
		return imageFailureResponse(`{"error":{"code":"image_download_error","message":"Failed to download the provided image (image_download_error=image_download_interrupted)"}}`), nil
	}
	return &provider.Response{StatusCode: http.StatusOK, Body: io.NopCloser(strings.NewReader(`{"data":[]}`))}, nil
}

func imageFailureResponse(body string) *provider.Response {
	data := []byte(body)
	return &provider.Response{
		StatusCode: http.StatusBadRequest,
		Body:       io.NopCloser(strings.NewReader(body)),
		Diagnostic: &provider.DiagnosticResponse{StatusCode: http.StatusBadRequest, Body: data},
	}
}
