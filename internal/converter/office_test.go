package converter

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestOfficeConversion(t *testing.T) {
	for _, extension := range []string{".doc", ".DOCX"} {
		t.Run(extension, func(t *testing.T) {
			input := filepath.Join(t.TempDir(), "Reading notes"+extension)
			if err := os.WriteFile(input, []byte("Word document contents"), 0600); err != nil {
				t.Fatal(err)
			}
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Method != http.MethodPost || r.URL.Path != "/forms/libreoffice/convert" {
					t.Errorf("unexpected conversion endpoint: %s %s", r.Method, r.URL.Path)
				}
				file, header, err := r.FormFile("files")
				if err != nil {
					t.Error(err)
					w.WriteHeader(http.StatusBadRequest)
					return
				}
				defer file.Close()
				body, _ := io.ReadAll(file)
				if header.Filename != filepath.Base(input) || string(body) != "Word document contents" {
					t.Errorf("document was changed: %q %q", header.Filename, body)
				}
				w.Header().Set("Content-Type", "application/pdf")
				io.WriteString(w, "%PDF-1.7\nconverted document")
			}))
			defer server.Close()
			t.Setenv("GOTENBERG_URL", server.URL)
			output, err := ConvertOfficeToPDF(context.Background(), input)
			if err != nil {
				t.Fatal(err)
			}
			body, err := os.ReadFile(output)
			if err != nil || string(body) != "%PDF-1.7\nconverted document" || filepath.Base(output) != "Reading notes.pdf" {
				t.Fatalf("unexpected output %q: %q, %v", output, body, err)
			}
			if _, err := os.Stat(input); err != nil {
				t.Fatalf("original was removed: %v", err)
			}
		})
	}
}

func TestOfficeConversionFailures(t *testing.T) {
	for _, test := range []struct {
		name   string
		status int
		body   string
	}{
		{"rejected document", 400, "invalid document"},
		{"converter unavailable", 503, "timeout"},
		{"invalid output", 200, "not a PDF"},
		{"truncated output", 200, "%PD"},
		{"redirect", 307, ""},
	} {
		t.Run(test.name, func(t *testing.T) {
			input := filepath.Join(t.TempDir(), "document.docx")
			os.WriteFile(input, []byte("input"), 0600)
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				io.Copy(io.Discard, r.Body)
				w.Header().Set("Location", "http://127.0.0.1:1/never-follow")
				w.WriteHeader(test.status)
				io.WriteString(w, test.body)
			}))
			defer server.Close()
			t.Setenv("GOTENBERG_URL", server.URL)
			if output, err := ConvertOfficeToPDF(context.Background(), input); err == nil || output != "" {
				t.Fatalf("conversion succeeded: %q, %v", output, err)
			}
			if _, err := os.Stat(strings.TrimSuffix(input, ".docx") + ".pdf"); !os.IsNotExist(err) {
				t.Fatalf("failed conversion left a PDF behind: %v", err)
			}
		})
	}
}

func TestOfficeConversionSizeLimits(t *testing.T) {
	for _, test := range []struct {
		name   string
		size   int64
		accept bool
	}{
		{"above former limit", 65 << 20, true},
		{"at limit", 128 << 20, true},
		{"above limit", (128 << 20) + 1, false},
	} {
		t.Run(test.name, func(t *testing.T) {
			dir := t.TempDir()
			input := filepath.Join(dir, "document.docx")
			if err := os.WriteFile(input, []byte("input"), 0600); err != nil {
				t.Fatal(err)
			}
			payload := filepath.Join(dir, "converted.pdf")
			file, err := os.Create(payload)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := file.WriteString("%PDF-1.7\n"); err != nil {
				t.Fatal(err)
			}
			if err := file.Truncate(test.size); err != nil {
				t.Fatal(err)
			}
			file.Close()
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				io.Copy(io.Discard, r.Body)
				reader, err := os.Open(payload)
				if err != nil {
					t.Error(err)
					w.WriteHeader(http.StatusInternalServerError)
					return
				}
				defer reader.Close()
				io.Copy(w, reader)
			}))
			defer server.Close()
			t.Setenv("GOTENBERG_URL", server.URL)
			output, err := ConvertOfficeToPDF(context.Background(), input)
			if !test.accept {
				if err == nil || output != "" {
					t.Fatalf("oversized conversion succeeded: %q, %v", output, err)
				}
				if _, err := os.Stat(filepath.Join(dir, "document.pdf")); !os.IsNotExist(err) {
					t.Fatalf("oversized conversion left output: %v", err)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			info, err := os.Stat(output)
			if err != nil || info.Size() != test.size {
				t.Fatalf("converted PDF size does not match %d: %v", test.size, err)
			}
		})
	}
}

func TestOfficeConversionCancellation(t *testing.T) {
	input := filepath.Join(t.TempDir(), "document.doc")
	os.WriteFile(input, []byte("input"), 0600)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.Copy(io.Discard, r.Body)
		<-r.Context().Done()
	}))
	defer server.Close()
	t.Setenv("GOTENBERG_URL", server.URL)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Millisecond)
	defer cancel()
	if _, err := ConvertOfficeToPDF(ctx, input); err == nil {
		t.Fatal("cancelled conversion succeeded")
	}
}
