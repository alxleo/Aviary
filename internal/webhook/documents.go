package webhook

import (
	"encoding/base64"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strings"

	"github.com/google/uuid"
	"github.com/rmitchellscott/aviary/internal/config"
	"github.com/rmitchellscott/aviary/internal/database"
	"github.com/rmitchellscott/aviary/internal/downloader"
	"github.com/rmitchellscott/aviary/internal/manager"
	"github.com/rmitchellscott/aviary/internal/security"
)

var (
	// supportedContentTypes lists MIME types we can process
	supportedContentTypes = []string{
		"application/pdf",
		"application/msword",
		"application/vnd.openxmlformats-officedocument.wordprocessingml.document",
		"image/jpeg",
		"image/png",
		"application/epub+zip",
		"text/markdown",
		"text/html",
		"text/plain", // for .md files that might be detected as plain text
	}
)

// DocumentRequest represents a webhook request that can contain either a URL or document content
type DocumentRequest struct {
	Body               string `form:"Body" json:"body"`               // URL or base64 content
	ContentType        string `form:"ContentType" json:"contentType"` // MIME type for content
	Filename           string `form:"Filename" json:"filename"`       // Original filename
	IsContent          bool   `form:"IsContent" json:"isContent"`     // Flag: true=content, false=URL
	Prefix             string `form:"prefix" json:"prefix"`
	Compress           string `form:"compress" json:"compress"`
	CompressionPreset  string `form:"compression_preset" json:"compression_preset"`
	Manage             string `form:"manage" json:"manage"`
	Archive            string `form:"archive" json:"archive"`
	RmDir              string `form:"rm_dir" json:"rm_dir"`
	RetentionDays      string `form:"retention_days" json:"retention_days"`
	ConflictResolution string `form:"conflict_resolution" json:"conflict_resolution"`
	Coverpage          string `form:"coverpage" json:"coverpage"`
	Contrast           string `form:"contrast" json:"contrast"`
	CurrentPage        string `form:"currentpage" json:"currentpage"`
	RemoveBackground   string `form:"remove_background" json:"removeBackground"`
}

func (req DocumentRequest) form(body string) map[string]string {
	form := map[string]string{
		"Body":                body,
		"prefix":              req.Prefix,
		"compress":            req.Compress,
		"compression_preset":  req.CompressionPreset,
		"manage":              req.Manage,
		"archive":             req.Archive,
		"rm_dir":              req.RmDir,
		"retention_days":      req.RetentionDays,
		"conflict_resolution": req.ConflictResolution,
		"coverpage":           req.Coverpage,
		"contrast":            req.Contrast,
		"currentpage":         req.CurrentPage,
		"remove_background":   req.RemoveBackground,
	}

	// Set defaults for empty values
	if form["compress"] == "" {
		form["compress"] = "false"
	}
	if form["manage"] == "" {
		form["manage"] = "false"
	}
	if form["archive"] == "" {
		form["archive"] = "false"
	}
	if form["retention_days"] == "" {
		form["retention_days"] = "7"
	}

	return form
}

// enqueueDocumentJob processes document content instead of URLs
func enqueueDocumentJob(req DocumentRequest) string {
	return enqueueDocumentJobForUser(req, uuid.Nil)
}

// enqueueDocumentJobForUser processes document content instead of URLs for a specific user
func enqueueDocumentJobForUser(req DocumentRequest, userID uuid.UUID) string {
	// Log document processing request
	manager.Logf("Processing document content: %s (%s)", req.Filename, req.ContentType)

	// Create a new job in the in-memory store
	id := uuid.NewString()
	jobStore.Create(id)

	// Launch background worker
	go func() {
		jobStore.Update(id, "Running", "", nil)
		jobStore.UpdateProgress(id, 0)

		// Catch panics
		defer func() {
			if r := recover(); r != nil {
				manager.Logf("Panic in processDocument: %v", r)
				jobStore.Update(id, "Error", "backend.status.internal_error", nil)
			}
		}()

		// Process the document content
		msgKey, data, err := processDocumentForUser(id, req, userID)
		if err != nil {
			manager.Logf("processDocument error: %v, message: %q", err, msgKey)
			jobStore.Update(id, "error", msgKey, data)
		} else {
			manager.Logf("processDocument success: %s", msgKey)
			jobStore.Update(id, "success", msgKey, data)
		}
	}()

	return id
}

// processDocument handles document content processing
func processDocument(jobID string, req DocumentRequest) (string, map[string]string, error) {
	return processDocumentForUser(jobID, req, uuid.Nil)
}

// processDocumentForUser handles document content processing for a specific user
func processDocumentForUser(jobID string, req DocumentRequest, userID uuid.UUID) (string, map[string]string, error) {
	// Create job-specific temp directory
	jobTempDir, err := manager.CreateUserTempDir(userID)
	if err != nil {
		return "backend.status.internal_error", nil, fmt.Errorf("failed to create job temp directory: %w", err)
	}

	// Ensure cleanup of job temp directory
	defer func() {
		if err := os.RemoveAll(jobTempDir); err != nil {
			manager.Logf("cleanup warning: could not remove job temp directory %q: %v", jobTempDir, err)
		}
	}()

	// Validate content type if provided
	if req.ContentType != "" {
		if !isValidContentType(req.ContentType) {
			return "backend.status.unsupported_file_type", nil, fmt.Errorf("unsupported content type: %s", req.ContentType)
		}
	}

	// Decode base64 content with validation/cleaning
	jobStore.UpdateWithOperation(jobID, "Running", "backend.status.decoding_content", nil, "decoding")
	cleanedBase64 := cleanBase64(req.Body)
	content, err := base64.StdEncoding.DecodeString(cleanedBase64)
	if err != nil {
		return "backend.status.decode_error", nil, fmt.Errorf("failed to decode document content: %w", err)
	}

	// Verify content type matches actual content
	detectedType := http.DetectContentType(content[:min(512, len(content))])
	if req.ContentType != "" && !isContentTypeMatch(req.ContentType, detectedType) {
		manager.Logf("Content type mismatch: claimed %s, detected %s", req.ContentType, detectedType)
		// Continue but log the mismatch - use detected type for processing
	}

	// Validate and clean filename
	filename, err := security.ValidateAndCleanFilename(req.Filename)
	if err != nil || filename == "" {
		filename = "document"
		// Add extension based on detected or claimed content type
		contentTypeToUse := req.ContentType
		if contentTypeToUse == "" {
			contentTypeToUse = detectedType
		}
		switch {
		case strings.HasPrefix(contentTypeToUse, "application/pdf"):
			filename += ".pdf"
		case strings.HasPrefix(contentTypeToUse, "image/jpeg"):
			filename += ".jpg"
		case strings.HasPrefix(contentTypeToUse, "image/png"):
			filename += ".png"
		case strings.HasPrefix(contentTypeToUse, "application/epub"):
			filename += ".epub"
		}
	}

	// The declared Word type must enter conversion even with a missing or
	// contradictory filename suffix; raw Office bytes must never reach rmapi.
	switch {
	case strings.HasPrefix(req.ContentType, "application/msword"):
		filename = strings.TrimSuffix(filename, filepath.Ext(filename)) + ".doc"
	case strings.HasPrefix(req.ContentType, "application/vnd.openxmlformats-officedocument.wordprocessingml.document"):
		filename = strings.TrimSuffix(filename, filepath.Ext(filename)) + ".docx"
	}

	// Create temp file with proper extension
	dstPath := filepath.Join(jobTempDir, filename)
	tempFile, err := os.Create(dstPath)
	if err != nil {
		return "backend.status.save_error", nil, fmt.Errorf("failed to create temp file: %w", err)
	}
	tempFilePath := dstPath

	// Write content to temp file
	if _, err := tempFile.Write(content); err != nil {
		tempFile.Close()
		os.Remove(dstPath)
		return "backend.status.save_error", nil, fmt.Errorf("failed to write document: %w", err)
	}
	tempFile.Close()

	form := req.form(tempFilePath)

	// Process through existing pipeline
	return processPDFForUser(jobID, form, userID)
}

// isValidContentType checks if the content type is supported
func isValidContentType(contentType string) bool {
	for _, supported := range supportedContentTypes {
		if strings.HasPrefix(contentType, supported) {
			return true
		}
	}
	return false
}

// cleanBase64 removes whitespace and validates base64 content
func cleanBase64(data string) string {
	// Remove all whitespace (spaces, newlines, tabs)
	cleaned := strings.ReplaceAll(data, " ", "")
	cleaned = strings.ReplaceAll(cleaned, "\n", "")
	cleaned = strings.ReplaceAll(cleaned, "\r", "")
	cleaned = strings.ReplaceAll(cleaned, "\t", "")
	return cleaned
}

// isContentTypeMatch checks if claimed content type matches detected type
func isContentTypeMatch(claimed, detected string) bool {
	// Handle common variations
	switch {
	case strings.HasPrefix(claimed, "application/pdf") && strings.HasPrefix(detected, "application/pdf"):
		return true
	case strings.HasPrefix(claimed, "image/jpeg") && strings.HasPrefix(detected, "image/jpeg"):
		return true
	case strings.HasPrefix(claimed, "image/png") && strings.HasPrefix(detected, "image/png"):
		return true
	case strings.HasPrefix(claimed, "application/epub") && strings.HasPrefix(detected, "application/zip"):
		// EPUB files are detected as application/zip
		return true
	default:
		return strings.HasPrefix(detected, claimed) || strings.HasPrefix(claimed, detected)
	}
}

// min returns the minimum of two integers
func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}

// isTrue interprets "true"/"1"/"yes" (case-insensitive) as true.
func isTrue(s string) bool {
	s = strings.ToLower(s)
	return s == "true" || s == "1" || s == "yes"
}

var invalidFilenameChars = regexp.MustCompile(`[<>:"/\\|?*\x00-\x1f]`)

func sanitizeFilename(title string) string {
	if title == "" {
		return "article"
	}

	name := strings.TrimSpace(title)
	name = invalidFilenameChars.ReplaceAllString(name, "_")
	name = strings.ReplaceAll(name, "  ", " ")

	if len(name) > 200 {
		name = name[:200]
	}

	name = strings.TrimSpace(name)
	name = strings.Trim(name, "._")

	if name == "" {
		return "article"
	}

	return name
}

// getOutputFormat determines the output format (pdf or epub) based on:
// 1. Request parameter override (outputFormat in form)
// 2. User's per-user setting (if multi-user mode)
// 3. Global environment variable
// Returns "pdf" or "epub"
func getOutputFormat(form map[string]string, dbUser *database.User) string {
	// Check for request override
	if format := strings.ToLower(form["outputFormat"]); format == "pdf" || format == "epub" {
		return format
	}

	// Check user preference in multi-user mode
	if database.IsMultiUserMode() && dbUser != nil && dbUser.ConversionOutputFormat != "" {
		format := strings.ToLower(dbUser.ConversionOutputFormat)
		if format == "pdf" || format == "epub" {
			return format
		}
	}

	// Fall back to environment config
	return config.GetConversionOutputFormat()
}

func shouldOfferDownloadLink(dbUser *database.User) bool {
	if dbUser != nil && dbUser.ExperimentalDownloadLink != nil {
		return *dbUser.ExperimentalDownloadLink
	}
	return config.GetBool("EXPERIMENTAL_DOWNLOAD_LINK", false)
}

func shouldRemoveBackground(form map[string]string, _ *database.User) bool {
	if val := form["remove_background"]; val != "" {
		return isTrue(val)
	}
	return false
}

// isURL checks if the string is an HTTP(S) URL
func isURL(s string) bool {
	return strings.HasPrefix(s, "http://") || strings.HasPrefix(s, "https://")
}

// detectURLContentType determines content type for a URL.
// First checks URL path extension (fast), then falls back to HTTP sniffing.
func detectURLContentType(urlStr string) string {
	if parsedURL, err := url.Parse(urlStr); err == nil {
		ext := strings.ToLower(filepath.Ext(parsedURL.Path))
		switch ext {
		case ".pdf":
			return "application/pdf"
		case ".epub":
			return "application/epub+zip"
		case ".md", ".markdown":
			return "text/markdown"
		}
	}

	if mime, err := downloader.SniffMime(urlStr); err == nil {
		return mime
	}

	return "text/html"
}

// trackDocumentUpload records a document upload in the database
func trackDocumentUpload(userID uuid.UUID, localPath, remoteName, rmDir string) error {
	if database.DB == nil {
		return nil // Database not initialized
	}

	// Get file info
	var fileSize int64
	if secureLocalPath, err := security.NewSecurePathFromExisting(localPath); err == nil {
		if info, err := security.SafeStat(secureLocalPath); err == nil {
			fileSize = info.Size()
		}
	}

	// Determine document type from extension
	ext := strings.ToLower(filepath.Ext(localPath))
	var docType string
	switch ext {
	case ".pdf":
		docType = "PDF"
	case ".jpg", ".jpeg":
		docType = "JPEG"
	case ".png":
		docType = "PNG"
	case ".epub":
		docType = "EPUB"
	default:
		docType = "Unknown"
	}

	// Create document record
	doc := database.Document{
		ID:           uuid.New(),
		UserID:       userID,
		DocumentName: remoteName,
		LocalPath:    localPath,
		RemotePath:   filepath.Join(rmDir, remoteName),
		DocumentType: docType,
		FileSize:     fileSize,
		Status:       "uploaded",
	}

	return database.DB.Create(&doc).Error
}
