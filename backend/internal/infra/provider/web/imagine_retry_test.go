package web

import (
	"context"
	"io"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"

	fhttp "github.com/bogdanfinn/fhttp"
	fhttptest "github.com/bogdanfinn/fhttp/httptest"
	"github.com/bogdanfinn/websocket"

	"github.com/chenyme/grok2api/backend/internal/infra/provider"
)

// readTestImagineRequest consumes the reset and create frames the adapter writes
// before it starts listening for generated images.
func readTestImagineRequest(connection *websocket.Conn) bool {
	for range 2 {
		var message map[string]any
		if err := connection.ReadJSON(&message); err != nil {
			return false
		}
	}
	return true
}

func writeTestImagineImage(t *testing.T, connection *websocket.Conn) {
	t.Helper()
	if err := connection.WriteJSON(map[string]any{
		"type": "image", "id": "image-1", "blob": "aW1hZ2U=", "percentage_complete": 100, "grid_index": 0,
	}); err != nil {
		t.Errorf("write Imagine image: %v", err)
		return
	}
	if err := connection.WriteJSON(map[string]any{
		"type": "json", "id": "image-1", "current_status": "completed", "moderated": false, "order": 0,
	}); err != nil {
		t.Errorf("write Imagine completion: %v", err)
	}
}

func writeTestModeratedCompletion(t *testing.T, connection *websocket.Conn) {
	t.Helper()
	if err := connection.WriteJSON(map[string]any{
		"type": "json", "id": "image-1", "current_status": "completed", "moderated": true, "order": 0,
	}); err != nil {
		t.Errorf("write moderated completion: %v", err)
	}
}

// imagineRetryServer upgrades /ws/imagine/listen and lets each attempt decide
// what upstream does. The returned counter reports how many attempts the
// adapter made.
func imagineRetryServer(t *testing.T, attempt func(t *testing.T, number int32, connection *websocket.Conn)) (string, *atomic.Int32) {
	t.Helper()
	var solverCalls atomic.Int32
	var handshakes atomic.Int32
	server := fhttptest.NewServer(fhttp.HandlerFunc(func(writer fhttp.ResponseWriter, request *fhttp.Request) {
		if request.URL.Path == "/v1" {
			writeTestClearanceSolution(t, writer, solverCalls.Add(1))
			return
		}
		if request.URL.Path != "/ws/imagine/listen" {
			fhttp.NotFound(writer, request)
			return
		}
		connection, err := (&websocket.Upgrader{CheckOrigin: func(*fhttp.Request) bool { return true }}).Upgrade(writer, request, nil)
		if err != nil {
			t.Errorf("upgrade Imagine WebSocket: %v", err)
			return
		}
		number := handshakes.Add(1)
		if !readTestImagineRequest(connection) {
			_ = connection.Close()
			return
		}
		attempt(t, number, connection)
		_ = connection.Close()
	}))
	t.Cleanup(server.Close)
	return server.URL, &handshakes
}

func generateTestImage(t *testing.T, baseURL string) (*provider.Response, error) {
	t.Helper()
	adapter, credential := testMediaAdapter(t, baseURL)
	enableTestClearance(adapter, baseURL)
	return adapter.GenerateImage(context.Background(), provider.ImageGenerationRequest{
		Credential: credential, Model: "grok-imagine-image-quality", Prompt: "draw a teapot", Count: 1,
		Resolution: "1k", Quality: "medium", ResponseFormat: "b64_json",
	})
}

// A dropped WebSocket (upstream closes without a frame, which surfaces as close
// 1006) must be retried instead of failing the request outright.
func TestGenerateWSImageRetriesAfterDroppedSocket(t *testing.T) {
	baseURL, handshakes := imagineRetryServer(t, func(t *testing.T, number int32, connection *websocket.Conn) {
		if number == 1 {
			return // close without any frame
		}
		writeTestImagineImage(t, connection)
	})
	response, err := generateTestImage(t, baseURL)
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	body, readErr := io.ReadAll(response.Body)
	if readErr != nil || response.StatusCode != http.StatusOK || !strings.Contains(string(body), `"b64_json"`) {
		t.Fatalf("status=%d body=%s err=%v", response.StatusCode, body, readErr)
	}
	if got := handshakes.Load(); got != 2 {
		t.Fatalf("handshakes=%d, want 2", got)
	}
}

// An upstream job that settles with every image moderated must be retried: the
// condition is per-attempt and says nothing about the selected account.
func TestGenerateWSImageRetriesAfterModeratedCompletion(t *testing.T) {
	baseURL, handshakes := imagineRetryServer(t, func(t *testing.T, number int32, connection *websocket.Conn) {
		if number == 1 {
			writeTestModeratedCompletion(t, connection)
			return
		}
		writeTestImagineImage(t, connection)
	})
	response, err := generateTestImage(t, baseURL)
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	body, readErr := io.ReadAll(response.Body)
	if readErr != nil || response.StatusCode != http.StatusOK || !strings.Contains(string(body), `"b64_json"`) {
		t.Fatalf("status=%d body=%s err=%v", response.StatusCode, body, readErr)
	}
	if got := handshakes.Load(); got != 2 {
		t.Fatalf("handshakes=%d, want 2", got)
	}
}

// Retries are bounded: an always-moderated prompt must give up after
// imagineGenerationAttempts and report the moderated condition so the caller can
// expose a real reason instead of a generic upstream failure.
func TestGenerateWSImageReportsModeratedAfterRetriesExhausted(t *testing.T) {
	baseURL, handshakes := imagineRetryServer(t, func(t *testing.T, _ int32, connection *websocket.Conn) {
		writeTestModeratedCompletion(t, connection)
	})
	response, err := generateTestImage(t, baseURL)
	if response != nil {
		_ = response.Body.Close()
		t.Fatalf("response=%#v, want nil", response)
	}
	if !provider.IsImagineTransientError(err) || !provider.IsImagineModeratedError(err) {
		t.Fatalf("err=%v, want a moderated transient Imagine error", err)
	}
	if got := int(handshakes.Load()); got != imagineGenerationAttempts {
		t.Fatalf("handshakes=%d, want %d", got, imagineGenerationAttempts)
	}
}

func TestGenerateWSImageReportsDroppedSocketAfterRetriesExhausted(t *testing.T) {
	baseURL, handshakes := imagineRetryServer(t, func(t *testing.T, _ int32, connection *websocket.Conn) {
		// Return immediately so the deferred close drops the socket.
	})
	response, err := generateTestImage(t, baseURL)
	if response != nil {
		_ = response.Body.Close()
		t.Fatalf("response=%#v, want nil", response)
	}
	if !provider.IsImagineTransientError(err) || provider.IsImagineModeratedError(err) {
		t.Fatalf("err=%v, want a dropped-socket transient Imagine error", err)
	}
	if got := int(handshakes.Load()); got != imagineGenerationAttempts {
		t.Fatalf("handshakes=%d, want %d", got, imagineGenerationAttempts)
	}
}

// Non-Imagine errors must not be classified as retryable Imagine failures.
func TestImagineTransientClassifiersIgnoreOtherErrors(t *testing.T) {
	for _, err := range []error{nil, io.EOF, provider.NewMediaPostProcessingError(provider.MediaPostProcessingTransform, io.EOF)} {
		if provider.IsImagineTransientError(err) || provider.IsImagineModeratedError(err) {
			t.Fatalf("err=%v classified as an Imagine transient failure", err)
		}
	}
	dropped := provider.NewImagineTransientError(provider.ImagineTransientDropped, io.EOF)
	moderated := provider.NewImagineTransientError(provider.ImagineTransientModerated, io.EOF)
	if !provider.IsImagineTransientError(dropped) || provider.IsImagineModeratedError(dropped) {
		t.Fatalf("dropped=%v misclassified", dropped)
	}
	if !provider.IsImagineTransientError(moderated) || !provider.IsImagineModeratedError(moderated) {
		t.Fatalf("moderated=%v misclassified", moderated)
	}
	if provider.NewImagineTransientError(provider.ImagineTransientDropped, nil) != nil {
		t.Fatal("a nil cause must not produce an error")
	}
}
