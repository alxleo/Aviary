package webhook

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"io"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
)

func TestWordUploadsReportConversionFailure(t *testing.T) {
	t.Setenv("MULTI_USER", "false")
	t.Setenv("GOTENBERG_URL", "")
	t.Setenv("GOTENBERG_URL_FILE", "")
	gin.SetMode(gin.TestMode)
	router := gin.New()
	router.POST("/api/upload", UploadHandler)
	router.POST("/api/webhook", EnqueueHandler)
	router.GET("/api/status/:id", StatusHandler)

	for _, test := range []struct {
		name      string
		filenames []string
	}{
		{"single DOC", []string{"notes.doc"}},
		{"single DOCX", []string{"notes.docx"}},
		{"mixed batch", []string{"notes.doc", "other.docx"}},
	} {
		t.Run(test.name, func(t *testing.T) {
			var body bytes.Buffer
			form := multipart.NewWriter(&body)
			for _, filename := range test.filenames {
				part, err := form.CreateFormFile("files", filename)
				if err != nil {
					t.Fatal(err)
				}
				io.WriteString(part, "document")
			}
			form.Close()
			request := httptest.NewRequest(http.MethodPost, "/api/upload", &body)
			request.Header.Set("Content-Type", form.FormDataContentType())
			assertWordConversionFailure(t, router, request)
		})
	}

	for _, contentType := range []string{"application/msword", "application/vnd.openxmlformats-officedocument.wordprocessingml.document"} {
		for _, filename := range []string{"", "notes", "notes.pdf"} {
			t.Run(contentType+"/"+filename, func(t *testing.T) {
				body, err := json.Marshal(DocumentRequest{Body: base64.StdEncoding.EncodeToString([]byte("document")), ContentType: contentType, Filename: filename, IsContent: true})
				if err != nil {
					t.Fatal(err)
				}
				request := httptest.NewRequest(http.MethodPost, "/api/webhook", bytes.NewReader(body))
				request.Header.Set("Content-Type", "application/json")
				assertWordConversionFailure(t, router, request)
			})
		}
	}
}

func assertWordConversionFailure(t *testing.T, router http.Handler, request *http.Request) {
	t.Helper()
	response := httptest.NewRecorder()
	router.ServeHTTP(response, request)
	if response.Code != http.StatusAccepted {
		t.Fatalf("upload: %d %s", response.Code, response.Body)
	}
	var result struct {
		JobID string `json:"jobId"`
	}
	if err := json.Unmarshal(response.Body.Bytes(), &result); err != nil || result.JobID == "" {
		t.Fatalf("missing job: %s", response.Body)
	}
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		response = httptest.NewRecorder()
		router.ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/api/status/"+result.JobID, nil))
		var status struct {
			Status  string
			Message string
		}
		if err := json.Unmarshal(response.Body.Bytes(), &status); err != nil {
			t.Fatal(err)
		}
		if status.Status == "error" {
			if status.Message != "backend.status.conversion_error" {
				t.Fatalf("wrong failure: %s", response.Body)
			}
			return
		}
		if status.Status == "success" {
			t.Fatalf("raw Word document uploaded: %s", response.Body)
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("conversion job did not finish")
}
