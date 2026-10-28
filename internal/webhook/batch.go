package webhook

import (
	"context"
	"encoding/json"
	"fmt"
	"path/filepath"
	"strings"

	"github.com/google/uuid"
	"github.com/rmitchellscott/aviary/internal/compressor"
	"github.com/rmitchellscott/aviary/internal/converter"
	"github.com/rmitchellscott/aviary/internal/database"
	"github.com/rmitchellscott/aviary/internal/downloads"
	"github.com/rmitchellscott/aviary/internal/manager"
	"github.com/rmitchellscott/aviary/internal/security"
)

// processMultipleFilesForUser handles processing multiple files uploaded together
func processMultipleFilesForUser(jobID string, form map[string]string, userID uuid.UUID) (string, map[string]string, error) {

	body := form["Body"]
	compress := isTrue(form["compress"])
	uploadOpts := manager.UploadOptions{
		ConflictResolution: form["conflict_resolution"],
		Coverpage:          form["coverpage"],
		Contrast:           form["contrast"],
		CurrentPage:        form["currentpage"],
	}

	// Extract file paths from the "files:" prefix
	pathsJSON := strings.TrimPrefix(body, "files:")
	var filePaths []string
	if err := json.Unmarshal([]byte(pathsJSON), &filePaths); err != nil {
		return "backend.status.internal_error", nil, fmt.Errorf("failed to parse file paths: %w", err)
	}

	// Validate all file paths to prevent path injection attacks
	for i, filePath := range filePaths {
		if err := security.ValidateExistingFilePath(filePath); err != nil {
			return "backend.status.internal_error", nil, fmt.Errorf("invalid file path at index %d: %w", i, err)
		}
	}

	var (
		dbUser         *database.User
		finalPaths     []string
		totalPages     int
		processedPages int
		cleanupPaths   = append([]string(nil), filePaths...)
	)

	defer func() { secureCleanupPaths(cleanupPaths) }()

	if database.IsMultiUserMode() && userID != uuid.Nil {
		dbUser, _ = database.NewUserService(database.DB).GetUserByID(userID)
	}

	// Determine target reMarkable directory
	rmDir := form["rm_dir"]
	if rmDir == "" {
		if database.IsMultiUserMode() && dbUser != nil && dbUser.DefaultRmdir != "" {
			rmDir = dbUser.DefaultRmdir
		} else {
			rmDir = manager.DefaultRmDir()
		}
	}

	jobStore.UpdateWithOperation(jobID, "Running", "backend.status.using_uploaded_file", nil, "processing")

	// Separate files by type and process compressible files first, then EPUBs
	var compressibleFiles []string
	var epubFiles []string

	for _, filePath := range filePaths {
		ext := strings.ToLower(filepath.Ext(filePath))
		if ext == ".epub" {
			epubFiles = append(epubFiles, filePath)
		} else {
			compressibleFiles = append(compressibleFiles, filePath)
		}
	}

	// Reorder file paths: compressible files first, then EPUBs
	orderedPaths := append(compressibleFiles, epubFiles...)

	// Pre-calculate total pages for accurate progress tracking (only for compressible files)
	if compress {
		for _, filePath := range compressibleFiles {
			ext := strings.ToLower(filepath.Ext(filePath))
			if ext == ".pdf" {
				if pages, err := compressor.GetPDFPageCount(filePath); err == nil {
					totalPages += pages
				} else {
					// Fallback: assume 1 page if we can't get count
					totalPages += 1
				}
			} else if ext == ".jpg" || ext == ".jpeg" || ext == ".png" {
				totalPages += 1 // Images become 1-page PDFs
			}
		}
		// Set compression status once for the entire batch
		if totalPages > 0 {
			jobStore.UpdateWithOperation(jobID, "Running", "backend.status.compressing_pdf", nil, "compressing")
			jobStore.UpdateProgress(jobID, 0)
		}
	}

	// Process each file in the reordered list
	for _, filePath := range orderedPaths {

		if converter.IsOfficeDocument(filePath) {
			jobStore.UpdateWithOperation(jobID, "Running", "backend.status.converting_pdf", nil, "converting")
			pdfPath, convErr := converter.ConvertOfficeToPDF(context.Background(), filePath)
			if convErr != nil {
				return "backend.status.conversion_error", nil, convErr
			}
			cleanupPaths = append(cleanupPaths, pdfPath)
			filePath = pdfPath
		}

		// Convert images to PDF if needed
		ext := strings.ToLower(filepath.Ext(filePath))
		if ext == ".jpg" || ext == ".jpeg" || ext == ".png" {
			manager.Logf("Converting image %q to PDF", filePath)
			var pdfPath string
			var convErr error
			if database.IsMultiUserMode() && dbUser != nil {
				pdfPath, convErr = converter.ConvertImageToPDFWithSettings(filePath, dbUser.PageResolution, dbUser.PageDPI)
			} else {
				pdfPath, convErr = converter.ConvertImageToPDF(filePath)
			}
			if convErr != nil {
				// Clean up any processed files before returning error
				return "backend.status.conversion_error", nil, convErr
			}
			cleanupPaths = append(cleanupPaths, pdfPath)
			filePath = pdfPath
		}

		// Compress PDF if requested and file is PDF
		if compress && (strings.ToLower(filepath.Ext(filePath)) == ".pdf") {
			manager.Logf("Compressing PDF %q", filePath)

			// Get the expected page count for this file from our pre-calculation
			expectedPages := 1 // fallback
			if pages, err := compressor.GetPDFPageCount(filePath); err == nil {
				expectedPages = pages
			}

			compressedPath, compErr := compressor.CompressPDFWithProgress(filePath, func(page, total int) {
				// Calculate overall progress: (all previously processed pages + current progress within this file) / total pages
				if totalPages > 0 && total > 0 {
					// Current progress within this file as a fraction of its total pages
					fileProgressPages := float64(page*expectedPages) / float64(total)
					overallProgress := int((float64(processedPages) + fileProgressPages) / float64(totalPages) * 100)
					jobStore.UpdateProgress(jobID, overallProgress)
				}
			})
			if compErr != nil {
				// Clean up any processed files before returning error
				return "backend.status.compress_error", nil, compErr
			}

			// Update processed pages count with the expected pages for this file
			processedPages += expectedPages

			// Remove uncompressed version
			if secureFilePath, err := security.NewSecurePathFromExisting(filePath); err == nil {
				security.SafeRemove(secureFilePath)
			}
			cleanupPaths = append(cleanupPaths, compressedPath)

			// Rename compressed file to remove "_compressed" suffix (to match single file flow)
			origPath := strings.TrimSuffix(compressedPath, "_compressed.pdf") + ".pdf"
			secureCompressedPath, compErr := security.NewSecurePathFromExisting(compressedPath)
			secureOrigPath, origErr := security.NewSecurePathFromExisting(origPath)
			if compErr != nil || origErr != nil || security.SafeRename(secureCompressedPath, secureOrigPath) != nil {
				// Clean up any processed files before returning error
				return "backend.status.rename_error", nil, fmt.Errorf("failed to rename compressed file")
			}

			// Update cleanup paths and file path
			cleanupPaths[len(cleanupPaths)-1] = origPath // Replace the last entry
			filePath = origPath
		} else if compress {
			// For non-PDF files (images), still increment processed pages to keep progress accurate
			processedPages += 1
		}

		finalPaths = append(finalPaths, filePath)
	}

	// Upload all processed files
	var uploadedPaths []string
	jobStore.UpdateWithOperation(jobID, "Running", "backend.status.uploading", nil, "uploading")

	for _, filePath := range finalPaths {
		// Use simple upload for each file
		remoteName, err := manager.SimpleUpload(filePath, rmDir, dbUser, uploadOpts)
		if err != nil {
			// Clean up any processed files before returning error
			if isConflictError(err) {
				return "backend.status.conflict_entry_exists", map[string]string{
					"conflict_resolution": "settings.labels.conflict_resolution",
					"settings":            "app.settings",
				}, err
			}
			return "backend.status.internal_error", nil, err
		}

		fullPath := filepath.Join(rmDir, remoteName)
		fullPath = strings.TrimPrefix(fullPath, "/")
		uploadedPaths = append(uploadedPaths, fullPath)

		// Track document in database if in multi-user mode
		if database.IsMultiUserMode() && userID != uuid.Nil {
			if err := trackDocumentUpload(userID, filePath, remoteName, rmDir); err != nil {
				manager.Logf("failed to track document upload: %v", err)
			}
		}
	}

	var downloadTokens []string
	if shouldOfferDownloadLink(dbUser) && form["source"] == "ui" {
		for _, filePath := range finalPaths {
			if token, dlErr := downloads.Register(filePath, filepath.Base(filePath)); dlErr != nil {
				manager.Logf("download link warning: %v", dlErr)
			} else {
				downloadTokens = append(downloadTokens, token)
			}
		}
	}

	// Return success with all uploaded paths as JSON array for proper HTML list rendering
	pathsJSONBytes, err := json.Marshal(uploadedPaths)
	if err != nil {
		// Fallback to simple join if JSON marshaling fails
		return "backend.status.upload_success_multiple", map[string]string{"paths": strings.Join(uploadedPaths, "\n")}, nil
	}

	data := map[string]string{"paths": string(pathsJSONBytes)}
	if len(downloadTokens) > 0 {
		if tokensJSON, jsonErr := json.Marshal(downloadTokens); jsonErr == nil {
			data["download_tokens"] = string(tokensJSON)
		}
	}

	jobStore.UpdateProgress(jobID, 100)
	return "backend.status.upload_success_multiple", data, nil
}
