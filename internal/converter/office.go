package converter

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/rmitchellscott/aviary/internal/config"
)

const maxOfficePDFSize = 128 << 20

func IsOfficeDocument(path string) bool {
	ext := strings.ToLower(filepath.Ext(path))
	return ext == ".doc" || ext == ".docx"
}

// ConvertOfficeToPDF sends one document to the operator-configured converter.
// This client deliberately permits private addresses, unlike user-supplied URLs.
func ConvertOfficeToPDF(ctx context.Context, path string) (string, error) {
	if !IsOfficeDocument(path) {
		return "", fmt.Errorf("unsupported Word document extension")
	}
	endpoint, err := url.Parse(config.Get("GOTENBERG_URL", ""))
	if err != nil || endpoint.Host == "" || (endpoint.Scheme != "http" && endpoint.Scheme != "https") || endpoint.User != nil || endpoint.RawQuery != "" || endpoint.Fragment != "" {
		return "", fmt.Errorf("Word conversion requires a valid GOTENBERG_URL")
	}
	endpoint.Path = strings.TrimRight(endpoint.Path, "/") + "/forms/libreoffice/convert"
	input, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer input.Close()

	reader, writer := io.Pipe()
	defer reader.Close()
	form := multipart.NewWriter(writer)
	go func() {
		part, err := form.CreateFormFile("files", filepath.Base(path))
		if err == nil {
			_, err = io.Copy(part, input)
		}
		if err == nil {
			err = form.Close()
		}
		writer.CloseWithError(err)
	}()
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint.String(), reader)
	if err != nil {
		return "", err
	}
	request.Header.Set("Content-Type", form.FormDataContentType())
	client := &http.Client{
		Timeout:       config.GetDuration("GOTENBERG_TIMEOUT", 2*time.Minute),
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}
	response, err := client.Do(request)
	if err != nil {
		return "", fmt.Errorf("Word conversion request failed: %w", err)
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		return "", fmt.Errorf("Word conversion failed (HTTP %d)", response.StatusCode)
	}
	body := bufio.NewReader(response.Body)
	header, err := body.Peek(5)
	if err != nil || string(header) != "%PDF-" {
		return "", fmt.Errorf("Word converter returned an invalid PDF")
	}
	outputPath := strings.TrimSuffix(path, filepath.Ext(path)) + ".pdf"
	output, err := os.OpenFile(outputPath, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
	if err != nil {
		return "", err
	}
	success := false
	defer func() {
		output.Close()
		if !success {
			os.Remove(outputPath)
		}
	}()
	written, err := io.Copy(output, io.LimitReader(body, maxOfficePDFSize+1))
	if err != nil {
		return "", fmt.Errorf("read converted PDF: %w", err)
	}
	if written > maxOfficePDFSize {
		return "", fmt.Errorf("converted PDF exceeds 128 MiB")
	}
	if err := output.Close(); err != nil {
		return "", err
	}
	success = true
	return outputPath, nil
}
