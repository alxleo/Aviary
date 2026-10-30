// internal/webhook/handler.go
package webhook

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/coder/websocket"
	"github.com/coder/websocket/wsjson"
	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"github.com/rmitchellscott/aviary/internal/auth"
	"github.com/rmitchellscott/aviary/internal/compressor"
	"github.com/rmitchellscott/aviary/internal/converter"
	"github.com/rmitchellscott/aviary/internal/database"
	"github.com/rmitchellscott/aviary/internal/downloader"
	"github.com/rmitchellscott/aviary/internal/downloads"
	"github.com/rmitchellscott/aviary/internal/jobs"
	"github.com/rmitchellscott/aviary/internal/manager"
	"github.com/rmitchellscott/aviary/internal/pdfprocessor"
	"github.com/rmitchellscott/aviary/internal/security"
	"github.com/rmitchellscott/aviary/internal/storage"
	"golang.org/x/text/cases"
	"golang.org/x/text/language"
)

// isConflictError checks if an error is due to rmapi conflict (entry already exists)
func isConflictError(err error) bool {
	if err == nil {
		return false
	}
	errStr := strings.ToLower(err.Error())
	return strings.Contains(errStr, "entry already exists") ||
		strings.Contains(errStr, "error: entry already exists")
}

// secureCleanupPaths safely removes all files in the provided path list
func secureCleanupPaths(paths []string) {
	for _, pathStr := range paths {
		if securePath, err := security.NewSecurePathFromExisting(pathStr); err == nil {
			security.SafeRemove(securePath)
		}
	}
}

// keyToMessage converts i18n keys to human-readable messages for logging
func keyToMessage(key string) string {
	switch key {
	case "backend.status.upload_success":
		return "Upload successful"
	case "backend.status.upload_success_multiple":
		return "Multiple uploads successful"
	case "backend.status.internal_error":
		return "Internal error"
	case "backend.status.using_uploaded_file":
		return "Using uploaded file"
	case "backend.status.no_url":
		return "No URL found"
	case "backend.status.downloading":
		return "Downloading"
	case "backend.status.download_error":
		return "Download error"
	case "backend.status.converting_pdf":
		return "Converting to PDF"
	case "backend.status.conversion_error":
		return "Conversion error"
	case "backend.status.compressing_pdf":
		return "Compressing PDF"
	case "backend.status.compress_error":
		return "Compression error"
	case "backend.status.removing_background":
		return "Removing background images"
	case "backend.status.rename_error":
		return "Rename error"
	case "backend.status.uploading":
		return "Uploading"
	case "backend.status.invalid_prefix":
		return "Invalid prefix"
	case "backend.status.job_not_found":
		return "Job not found"
	case "backend.status.conflict_entry_exists":
		return "Entry already exists"
	default:
		return key // fallback to key if not found
	}
}

var (
	// urlRegex is used to find an http(s) URL within the Body string.
	urlRegex = regexp.MustCompile(`https?://[^\s]+`)
	// markdownClient fetches raw Markdown, validating every address it reaches.
	markdownClient = security.NewHTTPClient()
	// jobStore holds in-memory jobs for status polling.
	jobStore = jobs.NewStore()
)

// enqueueJob creates a new job ID, logs form fields, starts processPDF(form) in a goroutine,
// and returns the newly generated jobId.
func enqueueJob(form map[string]string) string {
	return enqueueJobForUser(form, uuid.Nil)
}

// enqueueJobForUser creates a new job ID for a specific user, logs form fields, starts processPDF(form) in a goroutine,
// and returns the newly generated jobId.
func enqueueJobForUser(form map[string]string, userID uuid.UUID) string {
	// Get user for logging context
	var user *database.User
	if database.IsMultiUserMode() && userID != uuid.Nil {
		userService := database.NewUserService(database.DB)
		if u, err := userService.GetUserByID(userID); err == nil {
			user = u
		}
	}

	// Log each form field in "Human Key: Value" format with username.
	titleCaser := cases.Title(language.English)
	for key, val := range form {
		humanKey := titleCaser.String(strings.ReplaceAll(key, "_", " "))
		manager.LogfWithUser(user, "%s: %s", humanKey, val)
	}

	// Create a new job in the in-memory store.
	id := uuid.NewString()
	jobStore.Create(id)

	// Launch background worker
	go func() {
		jobStore.Update(id, "Running", "", nil)
		jobStore.UpdateProgress(id, 0)

		// Catch panics
		defer func() {
			if r := recover(); r != nil {
				manager.LogfWithUser(user, "Panic in processPDF: %v", r)
				jobStore.Update(id, "Error", "backend.status.internal_error", nil)
			}
		}()

		// Do the actual work
		msgKey, data, err := processPDFForUser(id, form, userID)
		if err != nil {
			manager.LogfWithUser(user, "processPDF error: %v, message: %s", err, keyToMessage(msgKey))
			jobStore.Update(id, "error", msgKey, data)
		} else {
			logMsg := keyToMessage(msgKey)
			if data != nil && data["path"] != "" {
				logMsg += " -> " + data["path"]
			}
			manager.LogfWithUser(user, "processPDF success: %s", logMsg)
			jobStore.Update(id, "success", msgKey, data)
		}
	}()

	return id
}

// EnqueueHandler accepts either form-values (URL-based flow) or JSON (document content flow), enqueues a job, and returns JSON{"jobId": "..."}.
func EnqueueHandler(c *gin.Context) {
	// Get user context for multi-user mode
	var userID uuid.UUID
	if database.IsMultiUserMode() {
		user, ok := auth.RequireUser(c)
		if !ok {
			return // auth.RequireUser already set the response
		}
		userID = user.ID
	}

	// Check if this is a JSON request with document content
	contentType := c.GetHeader("Content-Type")
	if strings.HasPrefix(contentType, "application/json") {
		var req DocumentRequest
		if err := c.ShouldBindJSON(&req); err != nil {
			c.JSON(http.StatusBadRequest, gin.H{"error": "Invalid JSON format"})
			return
		}

		// Handle document content or URL processing via JSON
		if req.IsContent {
			id := enqueueDocumentJobForUser(req, userID)
			c.JSON(http.StatusAccepted, gin.H{"jobId": id})
		} else {
			// JSON URL processing - convert to form map
			form := req.form(req.Body)
			id := enqueueJobForUser(form, userID)
			c.JSON(http.StatusAccepted, gin.H{"jobId": id})
		}
	} else {
		// Legacy form-encoded processing (for frontend compatibility)
		form := map[string]string{
			"Body":                c.PostForm("Body"),
			"prefix":              c.PostForm("prefix"),
			"compress":            c.DefaultPostForm("compress", "false"),
			"compression_preset":  c.PostForm("compression_preset"),
			"manage":              c.DefaultPostForm("manage", "false"),
			"archive":             c.DefaultPostForm("archive", "false"),
			"rm_dir":              c.PostForm("rm_dir"),
			"retention_days":      c.DefaultPostForm("retention_days", "7"),
			"conflict_resolution": c.PostForm("conflict_resolution"),
			"coverpage":           c.PostForm("coverpage"),
			"contrast":            c.PostForm("contrast"),
			"currentpage":         c.PostForm("currentpage"),
			"remove_background":   c.PostForm("remove_background"),
			"source":              "ui",
		}
		id := enqueueJobForUser(form, userID)
		c.JSON(http.StatusAccepted, gin.H{"jobId": id})
	}
}

// StatusHandler returns current status & message for a given jobId.
func StatusHandler(c *gin.Context) {
	id := c.Param("id")
	if job, ok := jobStore.Get(id); ok {
		c.JSON(http.StatusOK, job)
	} else {
		c.JSON(http.StatusNotFound, gin.H{"error": "backend.status.job_not_found"})
	}
}

// StatusWSHandler streams job updates over a WebSocket connection.
func StatusWSHandler(c *gin.Context) {
	id := c.Param("id")
	if _, ok := jobStore.Get(id); !ok {
		c.JSON(http.StatusNotFound, gin.H{"error": "backend.status.job_not_found"})
		return
	}

	conn, err := websocket.Accept(c.Writer, c.Request, nil)
	if err != nil {
		return
	}
	defer conn.Close(websocket.StatusInternalError, "internal error")

	ch, unsubscribe := jobStore.Subscribe(id)
	defer unsubscribe()

	ctx := c.Request.Context()
	for job := range ch {
		if err := wsjson.Write(ctx, conn, job); err != nil {
			return
		}
		if job.Status == "success" || job.Status == "error" {
			conn.Close(websocket.StatusNormalClosure, "done")
			return
		}
	}
}

// processPDF is the core pipeline: Given a form-map, it either treats form["Body"] as a local file path
// (if it exists on disk) or else extracts a URL from form["Body"], downloads it, and then proceeds to
// (optionally) compress, then upload/manage on the reMarkable. Returns a human-readable status message
// and/or an error.
func processPDF(jobID string, form map[string]string) (string, map[string]string, error) {
	return processPDFForUser(jobID, form, uuid.Nil)
}

// processPDFForUser is the core pipeline for a specific user: Given a form-map and userID, it either treats form["Body"] as a local file path
// (if it exists on disk) or else extracts a URL from form["Body"], downloads it, and then proceeds to
// (optionally) compress, then upload/manage on the reMarkable. Returns a human-readable status message
// and/or an error.
// fetchErrorKey maps a fetch failure to the message the user sees. An address the
// server refused gets its own key so it does not read as a network fault.
func fetchErrorKey(err error, fallbackKey string) string {
	if security.IsBlockedAddress(err) {
		return "backend.status.blocked_address"
	}
	return fallbackKey
}

func processPDFForUser(jobID string, form map[string]string, userID uuid.UUID) (string, map[string]string, error) {

	body := form["Body"]
	prefix := form["prefix"]
	if p, perr := manager.SanitizePrefix(prefix); perr != nil {
		return "backend.status.invalid_prefix", nil, perr
	} else {
		prefix = p
	}
	compress := isTrue(form["compress"])
	manage := isTrue(form["manage"])
	archive := isTrue(form["archive"])
	retentionStr := form["retention_days"]
	uploadOpts := manager.UploadOptions{
		ConflictResolution: form["conflict_resolution"],
		Coverpage:          form["coverpage"],
		Contrast:           form["contrast"],
		CurrentPage:        form["currentpage"],
	}

	retentionDays := 7
	if rd, err := strconv.Atoi(retentionStr); err == nil && rd > 0 {
		retentionDays = rd
	}

	var (
		localPath   string
		sourceIsPDF bool
		remoteName  string
		err         error
		cleanupErr  error
		dbUser      *database.User
	)

	if database.IsMultiUserMode() && userID != uuid.Nil {
		dbUser, _ = database.NewUserService(database.DB).GetUserByID(userID)
	}

	// Determine target reMarkable directory
	rmDir := form["rm_dir"]
	if rmDir == "" {
		// In multi-user mode, use user's personal default folder
		if database.IsMultiUserMode() && dbUser != nil && dbUser.DefaultRmdir != "" {
			rmDir = dbUser.DefaultRmdir
		} else {
			// Fallback to global default
			rmDir = manager.DefaultRmDir()
		}
	}

	// 1) Handle multiple files if Body starts with "files:"
	if strings.HasPrefix(body, "files:") {
		return processMultipleFilesForUser(jobID, form, userID)
	}

	// 2) If "Body" is already a valid local file path, skip download.
	// First validate the path to prevent path injection attacks
	if secureBodyPath, err := security.NewSecurePathFromExisting(body); err == nil {
		if fi, statErr := security.SafeStat(secureBodyPath); statErr == nil && !fi.IsDir() {
			localPath = body
			sourceIsPDF = strings.EqualFold(filepath.Ext(body), ".pdf")
			manager.Logf("processPDF: using local file path %q, skipping download", localPath)
			jobStore.UpdateWithOperation(jobID, "Running", "backend.status.using_uploaded_file", nil, "processing")
			// Ensure we delete this file (even on error) once we're done
			defer func() {
				// Only attempt removal if the file still exists
				if security.SafeStatExists(secureBodyPath) {
					if cleanupErr = security.SafeRemove(secureBodyPath); cleanupErr != nil {
						manager.Logf("cleanup warning (on exit): could not remove %q: %v", localPath, cleanupErr)
					}
				}
			}()
		}
	} else {
		// 2) Otherwise, extract a URL
		match := urlRegex.FindString(body)
		if match == "" {
			return "backend.status.no_url", nil, fmt.Errorf("no URL")
		}

		// Detect content type from URL path extension or HTTP sniffing
		contentType := detectURLContentType(match)

		if contentType == "application/pdf" || contentType == "application/epub+zip" {
			sourceIsPDF = contentType == "application/pdf"
			// Direct download of PDF/EPUB
			manager.Logf("DownloadPDF: tmp=true, prefix=%q", prefix)
			jobStore.UpdateWithOperation(jobID, "Running", "backend.status.downloading", nil, "downloading")
			localPath, err = downloader.DownloadPDFForUser(match, true, prefix, userID, nil)
			if err != nil {
				return fetchErrorKey(err, "backend.status.download_error"), nil, err
			}
		} else if contentType == "text/markdown" || contentType == "text/plain" {
			// Markdown URL - fetch raw content and convert
			manager.Logf("Fetching markdown from URL: %s", match)
			jobStore.UpdateWithOperation(jobID, "Running", "backend.status.fetching_url", nil, "fetching")

			if err := security.ValidateURL(match); err != nil {
				return fetchErrorKey(err, "backend.status.invalid_url"), nil, fmt.Errorf("URL validation failed: %w", err)
			}

			// codeql[go/request-forgery]: URL is validated by security.ValidateURL above
			resp, err := markdownClient.Get(match)
			if err != nil {
				return fetchErrorKey(err, "backend.status.download_error"), nil, fmt.Errorf("failed to fetch markdown: %w", err)
			}
			defer resp.Body.Close()

			mdBytes, err := io.ReadAll(resp.Body)
			if err != nil {
				return "backend.status.download_error", nil, fmt.Errorf("failed to read markdown: %w", err)
			}

			mdContent, err := converter.ConvertMarkdownStringToHTML(string(mdBytes))
			if err != nil {
				return "backend.status.conversion_error", nil, fmt.Errorf("failed to convert markdown: %w", err)
			}

			title := mdContent.Metadata.Title
			if title == "" {
				title = filepath.Base(match)
				title = strings.TrimSuffix(title, filepath.Ext(title))
			}

			outputFormat := getOutputFormat(form, dbUser)
			manager.Logf("Output format: %s", outputFormat)

			tempDir, tempErr := manager.CreateUserTempDir(userID)
			if tempErr != nil {
				return "backend.status.internal_error", nil, fmt.Errorf("failed to create temp dir: %w", tempErr)
			}

			filename := sanitizeFilename(title)
			var convertedPath string
			var convErr error

			if outputFormat == "epub" {
				jobStore.UpdateWithOperation(jobID, "Running", "backend.status.generating_epub", nil, "generating")
				convertedPath = filepath.Join(tempDir, filename+".epub")

				epubOptions := converter.EPUBOptions{
					Title:    title,
					Author:   mdContent.Metadata.Author,
					Language: "en",
				}
				convErr = converter.ConvertHTMLToEPUB(mdContent.HTML, convertedPath, epubOptions)
			} else {
				jobStore.UpdateWithOperation(jobID, "Running", "backend.status.rendering_pdf", nil, "rendering")
				convertedPath = filepath.Join(tempDir, filename+".pdf")

				var pdfOptions converter.PDFOptions
				if database.IsMultiUserMode() && dbUser != nil {
					pdfOptions = converter.GetPDFOptionsForUser(dbUser.PageResolution, dbUser.PageDPI)
				} else {
					pdfOptions = converter.GetPDFOptionsFromConfig()
				}
				pdfOptions.Title = title
				convErr = converter.ConvertHTMLToPDF(mdContent.HTML, convertedPath, pdfOptions)
			}

			if convErr != nil {
				return "backend.status.conversion_error", nil, fmt.Errorf("failed to convert markdown: %w", convErr)
			}

			localPath = convertedPath
			manager.Logf("Markdown converted: %s", localPath)
		} else {
			// Web article - extract readable content and convert to EPUB/PDF
			manager.Logf("Extracting article from URL: %s", match)
			jobStore.UpdateWithOperation(jobID, "Running", "backend.status.fetching_url", nil, "fetching")

			// Extract readable content from URL
			articleContent, extractErr := converter.ExtractFromURL(match)
			if extractErr != nil {
				return fetchErrorKey(extractErr, "backend.status.download_error"), nil, fmt.Errorf("failed to extract article: %w", extractErr)
			}

			// Determine output format
			outputFormat := getOutputFormat(form, dbUser)
			manager.Logf("Output format: %s", outputFormat)

			// Create temp file for the converted content
			tempDir, tempErr := manager.CreateUserTempDir(userID)
			if tempErr != nil {
				return "backend.status.internal_error", nil, fmt.Errorf("failed to create temp dir: %w", tempErr)
			}

			var convertedPath string
			var convErr error

			filename := sanitizeFilename(articleContent.Title)

			if outputFormat == "epub" {
				jobStore.UpdateWithOperation(jobID, "Running", "backend.status.generating_epub", nil, "generating")
				convertedPath = filepath.Join(tempDir, filename+".epub")

				epubOptions := converter.EPUBOptions{
					Title:     articleContent.Title,
					Author:    articleContent.Byline,
					Language:  "en",
					SourceURL: match,
				}

				convErr = converter.ConvertHTMLToEPUB(articleContent.HTML, convertedPath, epubOptions)
			} else {
				jobStore.UpdateWithOperation(jobID, "Running", "backend.status.rendering_pdf", nil, "rendering")
				convertedPath = filepath.Join(tempDir, filename+".pdf")

				var pdfOptions converter.PDFOptions
				if database.IsMultiUserMode() && dbUser != nil {
					pdfOptions = converter.GetPDFOptionsForUser(dbUser.PageResolution, dbUser.PageDPI)
				} else {
					pdfOptions = converter.GetPDFOptionsFromConfig()
				}
				pdfOptions.Title = articleContent.Title
				pdfOptions.SourceURL = match

				convErr = converter.ConvertHTMLToPDF(articleContent.HTML, convertedPath, pdfOptions)
			}

			if convErr != nil {
				return "backend.status.conversion_error", nil, fmt.Errorf("failed to convert article: %w", convErr)
			}

			localPath = convertedPath
			manager.Logf("Article converted: %s", localPath)
		}

		// Ensure we delete this file (even on error) once we're done
		defer func() {
			// Only attempt removal if the file still exists
			if secureLocalPath, pathErr := security.NewSecurePathFromExisting(localPath); pathErr == nil {
				if security.SafeStatExists(secureLocalPath) {
					if cleanupErr = security.SafeRemove(secureLocalPath); cleanupErr != nil {
						manager.Logf("cleanup warning (on exit): could not remove %q: %v", localPath, cleanupErr)
					}
				}
			}
		}()
	}

	preset := compressor.DefaultPreset
	if compress {
		preset, err = compressor.SelectPreset(form["compression_preset"], sourceIsPDF)
		if err != nil {
			return "backend.status.invalid_compression_preset", nil, err
		}
	}

	if converter.IsOfficeDocument(localPath) {
		jobStore.UpdateWithOperation(jobID, "Running", "backend.status.converting_pdf", nil, "converting")
		pdfPath, convErr := converter.ConvertOfficeToPDF(context.Background(), localPath)
		if convErr != nil {
			return "backend.status.conversion_error", nil, convErr
		}
		defer secureCleanupPaths([]string{pdfPath})
		localPath = pdfPath
	}

	// 3) If the file is an image, convert it to PDF now.
	ext := strings.ToLower(filepath.Ext(localPath))
	if ext == ".jpg" || ext == ".jpeg" || ext == ".png" {
		manager.Logf("Detected image %q – converting to PDF", localPath)
		jobStore.UpdateWithOperation(jobID, "Running", "backend.status.converting_pdf", nil, "converting")
		origPath := localPath

		// Convert the image → PDF (using per-user PAGE_RESOLUTION & PAGE_DPI if available)
		var pdfPath string
		var convErr error
		if database.IsMultiUserMode() && dbUser != nil {
			// Use user-specific settings if available, falling back to environment defaults
			pdfPath, convErr = converter.ConvertImageToPDFWithSettings(origPath, dbUser.PageResolution, dbUser.PageDPI)
		} else {
			// Use environment defaults (existing behavior)
			pdfPath, convErr = converter.ConvertImageToPDF(origPath)
		}
		if convErr != nil {
			return "backend.status.conversion_error", nil, convErr
		}

		// Schedule cleanup of the generated PDF at the end of processPDF
		defer func() {
			if securePdfPath, err := security.NewSecurePathFromExisting(pdfPath); err == nil {
				if security.SafeStatExists(securePdfPath) {
					if cleanupErr = security.SafeRemove(securePdfPath); cleanupErr != nil {
						manager.Logf("cleanup warning (on exit): could not remove generated PDF %q: %v", pdfPath, cleanupErr)
					}
				}
			}
		}()

		// Replace localPath so the rest of the pipeline uses the PDF
		localPath = pdfPath
	}

	// 3.5) Handle HTML/Markdown conversion to EPUB or PDF
	if ext == ".html" || ext == ".htm" || ext == ".md" || ext == ".markdown" {
		manager.Logf("Detected %s file – converting to EPUB/PDF", strings.ToUpper(strings.TrimPrefix(ext, ".")))

		// Determine output format
		outputFormat := getOutputFormat(form, dbUser)
		manager.Logf("Output format: %s", outputFormat)

		var htmlContent *converter.ArticleContent
		var title string
		var convErr error

		if ext == ".md" || ext == ".markdown" {
			// Markdown → HTML (skip readability)
			jobStore.UpdateWithOperation(jobID, "Running", "backend.status.converting_markdown", nil, "converting")
			mdResult, mdErr := converter.ConvertMarkdownToHTML(localPath)
			if mdErr != nil {
				return "backend.status.conversion_error", nil, fmt.Errorf("markdown conversion failed: %w", mdErr)
			}
			// Use metadata from frontmatter
			title = mdResult.Metadata.Title
			if title == "" {
				title = strings.TrimSuffix(filepath.Base(localPath), ext)
			}
			htmlContent = &converter.ArticleContent{
				HTML:   mdResult.HTML,
				Title:  title,
				Byline: mdResult.Metadata.Author,
			}
		} else {
			// HTML → extract readable content via go-readability
			jobStore.UpdateWithOperation(jobID, "Running", "backend.status.extracting_article", nil, "extracting")
			htmlContent, convErr = converter.ExtractFromHTML(localPath)
			if convErr != nil {
				return "backend.status.conversion_error", nil, fmt.Errorf("HTML extraction failed: %w", convErr)
			}
			title = htmlContent.Title
			if title == "" {
				title = strings.TrimSuffix(filepath.Base(localPath), ext)
			}
		}

		// Convert to EPUB or PDF based on output format
		var convertedPath string
		if outputFormat == "epub" {
			jobStore.UpdateWithOperation(jobID, "Running", "backend.status.generating_epub", nil, "generating")
			epubPath := strings.TrimSuffix(localPath, ext) + ".epub"

			epubOptions := converter.EPUBOptions{
				Title:    title,
				Author:   htmlContent.Byline,
				Language: "en",
			}

			convErr = converter.ConvertHTMLToEPUB(htmlContent.HTML, epubPath, epubOptions)
			if convErr != nil {
				return "backend.status.conversion_error", nil, fmt.Errorf("EPUB generation failed: %w", convErr)
			}
			convertedPath = epubPath
		} else {
			// PDF
			jobStore.UpdateWithOperation(jobID, "Running", "backend.status.rendering_pdf", nil, "rendering")
			pdfPath := strings.TrimSuffix(localPath, ext) + ".pdf"

			var pdfOptions converter.PDFOptions
			if database.IsMultiUserMode() && dbUser != nil {
				pdfOptions = converter.GetPDFOptionsForUser(dbUser.PageResolution, dbUser.PageDPI)
			} else {
				pdfOptions = converter.GetPDFOptionsFromConfig()
			}
			pdfOptions.Title = title

			convErr = converter.ConvertHTMLToPDF(htmlContent.HTML, pdfPath, pdfOptions)
			if convErr != nil {
				return "backend.status.conversion_error", nil, fmt.Errorf("PDF generation failed: %w", convErr)
			}
			convertedPath = pdfPath
		}

		// Schedule cleanup of the generated file at the end
		defer func() {
			if secureConvertedPath, err := security.NewSecurePathFromExisting(convertedPath); err == nil {
				if security.SafeStatExists(secureConvertedPath) {
					if cleanupErr = security.SafeRemove(secureConvertedPath); cleanupErr != nil {
						manager.Logf("cleanup warning (on exit): could not remove generated file %q: %v", convertedPath, cleanupErr)
					}
				}
			}
		}()

		// Replace localPath so the rest of the pipeline uses the converted file
		localPath = convertedPath
		manager.Logf("Conversion complete: %s", localPath)
	}

	// 4) Optionally remove background images from PDF
	if shouldRemoveBackground(form, dbUser) && strings.ToLower(filepath.Ext(localPath)) == ".pdf" {
		manager.Logf("Removing background images from PDF")
		jobStore.UpdateWithOperation(jobID, "Running", "backend.status.removing_background", nil, "removing_background")

		processedPath := strings.TrimSuffix(localPath, ".pdf") + "_nobg.pdf"
		removedCount, bgErr := pdfprocessor.RemoveBackgroundImages(localPath, processedPath)
		if bgErr != nil {
			manager.Logf("Background removal warning: %v (continuing with original file)", bgErr)
		} else if removedCount > 0 {
			if secureLocalPath, err := security.NewSecurePathFromExisting(localPath); err == nil {
				security.SafeRemove(secureLocalPath)
			}
			secureProcessedPath, err := security.NewSecurePathFromExisting(processedPath)
			if err == nil {
				secureLocalPath, err := security.NewSecurePathFromExisting(localPath)
				if err == nil {
					if err := security.SafeRename(secureProcessedPath, secureLocalPath); err != nil {
						manager.Logf("Failed to rename processed PDF: %v", err)
						localPath = processedPath
					}
				}
			}
			manager.Logf("Removed %d background image(s) from PDF", removedCount)
		} else {
			if secureProcessedPath, err := security.NewSecurePathFromExisting(processedPath); err == nil {
				security.SafeRemove(secureProcessedPath)
			}
			manager.Logf("No background images to remove")
		}
	}

	// 5) Optionally compress the PDF
	if compress {
		manager.Logf("Compressing PDF")
		jobStore.UpdateWithOperation(jobID, "Running", "backend.status.compressing_pdf", nil, "compressing")
		jobStore.UpdateProgress(jobID, 0)
		compressedPath, compErr := compressor.CompressPDFWithProgress(localPath, preset, func(page, total int) {
			pct := int(float64(page) / float64(total) * 100)
			jobStore.UpdateProgress(jobID, pct)
		})
		if compErr != nil {
			return "backend.status.compress_error", nil, compErr
		}

		if secureLocalPath, err := security.NewSecurePathFromExisting(localPath); err == nil {
			if err := security.SafeRemove(secureLocalPath); err != nil {
				manager.Logf("failed to remove uncompressed PDF %q: %v", localPath, err)
			}
		}

		localPath = compressedPath
		jobStore.UpdateProgress(jobID, 100)

		origPath := strings.TrimSuffix(localPath, "_compressed.pdf") + ".pdf"
		secureLocalPath, err := security.NewSecurePathFromExisting(localPath)
		if err != nil {
			return "backend.status.rename_error", nil, err
		}
		secureOrigPath, err := security.NewSecurePathFromExisting(origPath)
		if err != nil {
			return "backend.status.rename_error", nil, err
		}
		if err := security.SafeRename(secureLocalPath, secureOrigPath); err != nil {
			return "backend.status.rename_error", nil, err
		}
		localPath = origPath
	}

	// 6) Rename file for managed workflows
	var finalLocalPath string
	if manage {
		// Create new filename with month and day but no year
		today := time.Now()
		month, day := today.Format("January"), today.Day()

		ext := filepath.Ext(localPath)
		var newFilename string
		if prefix != "" {
			manager.Logf("Renaming file for managed workflow with prefix: %s", prefix)
			newFilename = fmt.Sprintf("%s %s %d%s", prefix, month, day, ext)
		} else {
			manager.Logf("Renaming file for managed workflow (no prefix)")
			newFilename = fmt.Sprintf("%s %d%s", month, day, ext)
		}

		// Create renamed file in same directory as original
		dir := filepath.Dir(localPath)
		finalLocalPath = filepath.Join(dir, newFilename)

		// Copy to new location
		secureLocalPath, err := security.NewSecurePathFromExisting(localPath)
		if err != nil {
			return "backend.status.rename_error", nil, err
		}
		secureFinalPath, err := security.NewSecurePathFromExisting(finalLocalPath)
		if err != nil {
			return "backend.status.rename_error", nil, err
		}
		if err := security.SafeRename(secureLocalPath, secureFinalPath); err != nil {
			return "backend.status.rename_error", nil, err
		}

		// Clean up the renamed file when done
		defer func() {
			if secureFinalPath, err := security.NewSecurePathFromExisting(finalLocalPath); err == nil {
				if security.SafeStatExists(secureFinalPath) {
					if cleanupErr := security.SafeRemove(secureFinalPath); cleanupErr != nil {
						manager.Logf("cleanup warning: could not remove renamed file %q: %v", finalLocalPath, cleanupErr)
					}
				}
			}
		}()
	} else {
		finalLocalPath = localPath
	}

	// 5) Upload to rmapi
	jobStore.UpdateWithOperation(jobID, "Running", "backend.status.uploading", nil, "uploading")
	manager.Logf("Uploading to reMarkable")
	remoteName, err = manager.SimpleUpload(finalLocalPath, rmDir, dbUser, uploadOpts)
	if err != nil {
		if isConflictError(err) {
			return "backend.status.conflict_entry_exists", map[string]string{
				"conflict_resolution": "settings.labels.conflict_resolution",
				"settings":            "app.settings",
			}, err
		}
		return "backend.status.internal_error", nil, err
	}

	// 6) Archive to storage backend if requested
	if archive {
		manager.Logf("Archiving to storage backend")
		filename := filepath.Base(finalLocalPath)
		multiUserMode := database.IsMultiUserMode()

		var storageKey string
		if manage {
			// For managed files, use no-year format first, then add year for archival
			noYearKey := storage.GenerateUserDocumentKey(userID, prefix, filename, multiUserMode)

			// Copy to storage with no-year format
			ctx := context.Background()
			if err := storage.CopyFileToStorage(ctx, finalLocalPath, noYearKey); err != nil {
				manager.Logf("archival warning: failed to copy to storage: %v", err)
			} else {
				// Add year for archival copy
				if yearKey, err2 := manager.AppendYearStorage(ctx, noYearKey, userID); err2 != nil {
					manager.Logf("archival warning: failed to create year copy: %v", err2)
				} else {
					storageKey = yearKey
				}
			}
		} else {
			// For non-managed files, archive as-is
			storageKey = storage.GenerateUserDocumentKey(userID, "", filename, multiUserMode)
			ctx := context.Background()
			if err := storage.CopyFileToStorage(ctx, finalLocalPath, storageKey); err != nil {
				manager.Logf("archival warning: failed to copy to storage: %v", err)
			}
		}

	}

	// 7) If manage==true, perform cleanup
	if manage {
		if err := manager.CleanupOld(prefix, rmDir, retentionDays, dbUser); err != nil {
			manager.Logf("cleanup warning: %v", err)
		}
	}

	// 8) Track document in database if in multi-user mode
	if database.IsMultiUserMode() && userID != uuid.Nil {
		if err := trackDocumentUpload(userID, finalLocalPath, remoteName, rmDir); err != nil {
			manager.Logf("failed to track document upload: %v", err)
			// Continue anyway - the upload was successful
		}
	}

	// 9) Now that the file has been uploaded to the reMarkable, build final status message
	fullPath := filepath.Join(rmDir, remoteName)
	fullPath = strings.TrimPrefix(fullPath, "/")
	jobStore.UpdateProgress(jobID, 100)

	data := map[string]string{"path": fullPath}

	if shouldOfferDownloadLink(dbUser) && form["source"] == "ui" {
		if token, dlErr := downloads.Register(finalLocalPath, filepath.Base(finalLocalPath)); dlErr != nil {
			manager.Logf("download link warning: %v", dlErr)
		} else {
			data["download_token"] = token
		}
	}

	return "backend.status.upload_success", data, nil
}
