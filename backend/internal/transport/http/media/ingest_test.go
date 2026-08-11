package media

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	mediaapp "github.com/chenyme/grok2api/backend/internal/application/media"
	localmedia "github.com/chenyme/grok2api/backend/internal/infra/media"
	"github.com/chenyme/grok2api/backend/internal/infra/persistence/relational"
	"github.com/gin-gonic/gin"
)

func TestAdminUploadCreatesHiddenTransientInput(t *testing.T) {
	gin.SetMode(gin.TestMode)
	ctx := context.Background()
	database, err := relational.OpenSQLite(ctx, filepath.Join(t.TempDir(), "media-ingest.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	if err := database.InitializeSchema(ctx); err != nil {
		t.Fatal(err)
	}
	objects, err := localmedia.NewLocalStore(filepath.Join(t.TempDir(), "objects"))
	if err != nil {
		t.Fatal(err)
	}
	service := mediaapp.NewService(relational.NewMediaAssetRepository(database), relational.NewMediaJobRepository(database), objects, nil, mediaapp.Config{
		PublicBaseURL: "https://api.example", MaxImageBytes: 32 << 20, MaxTotalBytes: 1 << 30,
		CleanupThresholdPercent: 80, CleanupInterval: time.Minute,
	})
	raw, _ := base64.StdEncoding.DecodeString("iVBORw0KGgoAAAANSUhEUgAAAAEAAAABCAQAAAC1HAwCAAAAC0lEQVR42mNk+A8AAQUBAScY42YAAAAASUVORK5CYII=")
	var requestBody bytes.Buffer
	writer := multipart.NewWriter(&requestBody)
	part, err := writer.CreateFormFile("file", "input.png")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := part.Write(raw); err != nil {
		t.Fatal(err)
	}
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}

	handler := NewHandler(service)
	router := gin.New()
	handler.RegisterPublic(router)
	handler.RegisterAdmin(router.Group("/api/admin/v1"))
	request := httptest.NewRequest(http.MethodPost, "/api/admin/v1/media/inputs/upload", &requestBody)
	request.Header.Set("Content-Type", writer.FormDataContentType())
	recorder := httptest.NewRecorder()
	router.ServeHTTP(recorder, request)
	if recorder.Code != http.StatusCreated {
		t.Fatalf("upload status=%d body=%s", recorder.Code, recorder.Body.String())
	}
	var envelope struct {
		Data struct {
			FileID    string `json:"fileId"`
			ExpiresAt string `json:"expiresAt"`
		} `json:"data"`
	}
	if err := json.Unmarshal(recorder.Body.Bytes(), &envelope); err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(envelope.Data.FileID, "input_") || envelope.Data.ExpiresAt == "" {
		t.Fatalf("response=%s", recorder.Body.String())
	}
	public := httptest.NewRecorder()
	router.ServeHTTP(public, httptest.NewRequest(http.MethodGet, "/v1/media/images/"+envelope.Data.FileID, nil))
	if public.Code != http.StatusNotFound {
		t.Fatalf("transient input became public: status=%d", public.Code)
	}
	if values, total, err := service.AdminListImages(ctx, 1, 20, ""); err != nil || total != 0 || len(values) != 0 {
		t.Fatalf("gallery values=%#v total=%d err=%v", values, total, err)
	}
}
