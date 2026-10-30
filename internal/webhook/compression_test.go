package webhook

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"testing"

	"github.com/gin-gonic/gin"
)

func TestScanPresetsRejectNonPDFInputsThroughUploadRoutes(t *testing.T) {
	t.Setenv("MULTI_USER", "false")
	t.Setenv("GOTENBERG_URL", "")
	t.Setenv("GOTENBERG_URL_FILE", "")
	t.Setenv("RMAPI_CONFIG", filepath.Join(t.TempDir(), "unpaired.conf"))
	gin.SetMode(gin.TestMode)
	router := gin.New()
	router.POST("/api/upload", UploadHandler)
	router.POST("/api/webhook", EnqueueHandler)
	router.GET("/api/status/:id", StatusHandler)

	for _, filenames := range [][]string{{"notes.docx"}, {"notes.docx", "scan.pdf"}, {"scan.pdf", "notes.docx"}} {
		t.Run(fmt.Sprint(filenames), func(t *testing.T) {
			var body bytes.Buffer
			form := multipart.NewWriter(&body)
			form.WriteField("compress", "true")
			form.WriteField("compression_preset", "scan_300_color")
			for _, filename := range filenames {
				part, err := form.CreateFormFile("files", filename)
				if err != nil {
					t.Fatal(err)
				}
				io.WriteString(part, "document")
			}
			form.Close()
			request := httptest.NewRequest(http.MethodPost, "/api/upload", &body)
			request.Header.Set("Content-Type", form.FormDataContentType())
			assertJobFailure(t, router, request, "backend.status.invalid_compression_preset")
		})
	}

	for _, test := range []struct{ compress, message string }{
		{"true", "backend.status.invalid_compression_preset"},
		{"", "backend.status.conversion_error"},
	} {
		requestBody, err := json.Marshal(DocumentRequest{
			Body: base64.StdEncoding.EncodeToString([]byte("document")), ContentType: "application/msword",
			Filename: "notes.pdf", IsContent: true, Compress: test.compress, CompressionPreset: "scan_300_grayscale",
		})
		if err != nil {
			t.Fatal(err)
		}
		request := httptest.NewRequest(http.MethodPost, "/api/webhook", bytes.NewReader(requestBody))
		request.Header.Set("Content-Type", "application/json")
		assertJobFailure(t, router, request, test.message)
	}
}

func TestCompressionPresetReachesBothURLRequestFormats(t *testing.T) {
	t.Setenv("MULTI_USER", "false")
	t.Setenv("BLOCK_PRIVATE_IPS", "false")
	t.Setenv("RMAPI_CONFIG", filepath.Join(t.TempDir(), "unpaired.conf"))
	gin.SetMode(gin.TestMode)
	router := gin.New()
	router.POST("/api/webhook", EnqueueHandler)
	router.GET("/api/status/:id", StatusHandler)
	source := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/pdf")
		io.WriteString(w, "%PDF-1.7\nsynthetic input")
	}))
	defer source.Close()
	for _, jsonRequest := range []bool{false, true} {
		var body []byte
		contentType := "application/x-www-form-urlencoded"
		if jsonRequest {
			var err error
			body, err = json.Marshal(DocumentRequest{Body: source.URL + "/scan.pdf", Compress: "true", CompressionPreset: "invalid"})
			if err != nil {
				t.Fatal(err)
			}
			contentType = "application/json"
		} else {
			body = []byte(url.Values{"Body": {source.URL + "/scan.pdf"}, "compress": {"true"}, "compression_preset": {"invalid"}}.Encode())
		}
		request := httptest.NewRequest(http.MethodPost, "/api/webhook", bytes.NewReader(body))
		request.Header.Set("Content-Type", contentType)
		assertJobFailure(t, router, request, "backend.status.invalid_compression_preset")
	}
}
