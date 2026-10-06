package rmapi

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/google/uuid"
	"github.com/rmitchellscott/aviary/internal/config"
	"github.com/rmitchellscott/aviary/internal/database"
)

const tempConfigPrefix = "aviary-rmapi-"

// DestinationStatus describes the rmapi destination visible to an Aviary user.
// The user's own identity and settings remain outside this type; only the
// effective rmapi host, pairing state, and whether the destination is shared
// are exposed to callers.
type DestinationStatus struct {
	Host   string
	Paired bool
	Shared bool
}

// ResolveEffectiveUser returns the user whose rmapi configuration and host
// should be used for a caller. In the default configuration it returns the
// caller unchanged. RMAPI_SHARED_USER_ID is deliberately resolved on every
// use so an owner being removed or deactivated fails closed immediately.
func ResolveEffectiveUser(user *database.User) (*database.User, bool, error) {
	ownerID, configured, err := sharedOwnerID()
	if err != nil {
		return nil, false, err
	}
	if !configured {
		return user, false, nil
	}
	if user == nil {
		return nil, false, fmt.Errorf("shared rmapi destination requires an authenticated user")
	}
	if database.DB == nil {
		return nil, false, fmt.Errorf("shared rmapi destination database is unavailable")
	}

	var owner database.User
	if err := database.DB.Where("id = ?", ownerID).First(&owner).Error; err != nil {
		return nil, false, fmt.Errorf("shared rmapi owner ID %s is unavailable: %w", ownerID, err)
	}
	if !owner.IsActive {
		return nil, false, fmt.Errorf("shared rmapi owner ID %s is inactive", ownerID)
	}

	return &owner, owner.ID != user.ID, nil
}

func sharedOwnerID() (uuid.UUID, bool, error) {
	if !database.IsMultiUserMode() {
		return uuid.Nil, false, nil
	}

	rawID := strings.TrimSpace(config.Get("RMAPI_SHARED_USER_ID", ""))
	if rawID == "" {
		return uuid.Nil, false, nil
	}
	ownerID, err := uuid.Parse(rawID)
	if err != nil {
		return uuid.Nil, true, fmt.Errorf("shared rmapi owner ID is invalid: %w", err)
	}
	return ownerID, true, nil
}

// IsSharedBorrower reports whether the caller uses another active user's
// rmapi destination. Resolution errors are returned so mutation endpoints can
// fail closed when the configured owner is unavailable.
func IsSharedBorrower(user *database.User) (bool, error) {
	_, shared, err := ResolveEffectiveUser(user)
	return shared, err
}

// DestinationStatusForUser returns the effective rmapi state for a caller.
// When a configured shared owner is unavailable, the state is intentionally
// empty and unpaired rather than falling back to the caller's destination.
func DestinationStatusForUser(user *database.User) DestinationStatus {
	if user == nil {
		return DestinationStatus{}
	}

	effective, shared, err := ResolveEffectiveUser(user)
	if err != nil {
		if database.IsMultiUserMode() && strings.TrimSpace(config.Get("RMAPI_SHARED_USER_ID", "")) != "" {
			return DestinationStatus{Shared: true}
		}
		return DestinationStatus{}
	}

	return DestinationStatus{
		Host:   effective.RmapiHost,
		Paired: IsUserPaired(effective.ID),
		Shared: shared,
	}
}

// LoadUserConfig loads rmapi configuration for a user
// In multi-user mode: loads from database
// In single-user mode: returns path to ~/.config/rmapi/rmapi.conf
func LoadUserConfig(userID uuid.UUID) (string, error) {
	if database.IsMultiUserMode() {
		user, err := loadUserByID(userID)
		if err != nil {
			return "", err
		}
		effective, _, err := ResolveEffectiveUser(user)
		if err != nil {
			return "", err
		}
		return loadMultiUserConfig(effective.ID)
	}
	return loadSingleUserConfig()
}

// SaveUserConfig saves rmapi configuration for a user
// In multi-user mode: saves to database
// In single-user mode: not supported (configs are managed by rmapi directly)
func SaveUserConfig(userID uuid.UUID, configContent string) error {
	if !database.IsMultiUserMode() {
		return fmt.Errorf("config saving only available in multi-user mode")
	}
	return database.SaveUserRmapiConfig(userID, configContent)
}

// loadMultiUserConfig loads config content from database
func loadMultiUserConfig(userID uuid.UUID) (string, error) {
	var user database.User
	if err := database.DB.Select("rmapi_config").Where("id = ?", userID).First(&user).Error; err != nil {
		return "", fmt.Errorf("failed to load user config from database: %w", err)
	}
	return user.RmapiConfig, nil
}

// loadSingleUserConfig returns path to single-user config file
func loadSingleUserConfig() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("failed to get user home directory: %w", err)
	}
	return filepath.Join(home, ".config", "rmapi", "rmapi.conf"), nil
}

// GetUserConfigPath returns the config path for a user
// In multi-user mode: creates temporary file from database content
// In single-user mode: returns ~/.config/rmapi/rmapi.conf
func GetUserConfigPath(userID uuid.UUID) (string, error) {
	if database.IsMultiUserMode() {
		user, err := loadUserByID(userID)
		if err != nil {
			return "", err
		}
		effective, _, err := ResolveEffectiveUser(user)
		if err != nil {
			return "", err
		}

		// Load config from database and create a unique temp file.
		configContent, err := loadMultiUserConfig(effective.ID)
		if err != nil {
			return "", err
		}
		if configContent == "" {
			return "", fmt.Errorf("user %s has no rmapi config", effective.Username)
		}
		return createTempConfigFile(configContent)
	}
	return loadSingleUserConfig()
}

func loadUserByID(userID uuid.UUID) (*database.User, error) {
	if database.DB == nil {
		return nil, fmt.Errorf("database is unavailable")
	}
	var user database.User
	if err := database.DB.Where("id = ?", userID).First(&user).Error; err != nil {
		return nil, fmt.Errorf("failed to load user from database: %w", err)
	}
	return &user, nil
}

// createTempConfigFile creates a temporary config file from database content
func createTempConfigFile(configContent string) (string, error) {
	tempFile, err := os.CreateTemp(os.TempDir(), tempConfigPrefix+"*.conf")
	if err != nil {
		return "", fmt.Errorf("failed to create temp config file: %w", err)
	}
	tempFilePath := tempFile.Name()
	defer func() {
		_ = tempFile.Close()
	}()

	if err := tempFile.Chmod(0600); err != nil {
		_ = os.Remove(tempFilePath)
		return "", fmt.Errorf("failed to secure temp config file: %w", err)
	}
	if _, err := tempFile.WriteString(configContent); err != nil {
		_ = os.Remove(tempFilePath)
		return "", fmt.Errorf("failed to write temp config file: %w", err)
	}
	if err := tempFile.Close(); err != nil {
		_ = os.Remove(tempFilePath)
		return "", fmt.Errorf("failed to close temp config file: %w", err)
	}
	return tempFilePath, nil
}

// CleanupTempConfigFile removes a temporary config file if it exists
func CleanupTempConfigFile(configPath string) {
	if configPath == "" {
		return
	}

	// Only cleanup files in temp directory created by createTempConfigFile.
	if filepath.Clean(filepath.Dir(configPath)) == filepath.Clean(os.TempDir()) &&
		strings.HasPrefix(filepath.Base(configPath), tempConfigPrefix) &&
		strings.HasSuffix(filepath.Base(configPath), ".conf") {
		_ = os.Remove(configPath)
	}
}

// GetUserCachePath returns the cache directory path for a user
// This is used to isolate rmapi cache between users in multi-user mode
func GetUserCachePath(userID uuid.UUID) string {
	if database.IsMultiUserMode() && userID != uuid.Nil {
		return filepath.Join(os.TempDir(), fmt.Sprintf("aviary-cache-%s", userID.String()))
	}
	// Single-user mode: use default cache
	home, err := os.UserHomeDir()
	if err != nil {
		return filepath.Join(os.TempDir(), "aviary-cache")
	}
	return filepath.Join(home, ".cache")
}

// CleanupUserCache removes a user's cache directory if it exists
func CleanupUserCache(cachePath string) {
	if cachePath == "" {
		return
	}

	// Only cleanup cache directories in temp with our naming pattern
	if filepath.Clean(filepath.Dir(cachePath)) == filepath.Clean(os.TempDir()) &&
		filepath.Base(cachePath) != ".cache" {
		os.RemoveAll(cachePath)
	}
}
