package rmapi

import (
	"fmt"
	"os"
	"os/exec"
	"strings"

	"github.com/rmitchellscott/aviary/internal/config"
	"github.com/rmitchellscott/aviary/internal/database"
	"github.com/rmitchellscott/aviary/internal/logging"
)

// ExecCommand is exec.Command by default, but can be overridden in tests
var ExecCommand = exec.Command

func init() {
	if config.Get("DRY_RUN", "") != "" {
		ExecCommand = func(name string, args ...string) *exec.Cmd {
			cmdStr := name
			if len(args) > 0 {
				cmdStr += " " + strings.Join(args, " ")
			}
			logging.Logf("[DRY RUN] would run: %s", cmdStr)
			return exec.Command("true")
		}
	}
}

// NewCommand creates a new rmapi command with the caller's cache and the
// caller's effective rmapi destination. The cleanup function must be called
// after execution. Destination resolution errors are returned before a
// command can run, so a missing or inactive shared owner cannot fall back to
// another account's configuration.
func NewCommand(user *database.User, args ...string) (*exec.Cmd, func(), error) {
	cleanup := func() {}
	env := os.Environ()
	var tempConfigPath string

	if database.IsMultiUserMode() && user == nil {
		return nil, cleanup, fmt.Errorf("authenticated user required for rmapi in multi-user mode")
	}

	if user != nil {
		effectiveUser, _, err := ResolveEffectiveUser(user)
		if err != nil {
			return nil, cleanup, err
		}

		// Keep cache isolation tied to the authenticated caller, even when the
		// rmapi destination is shared.
		cachePath := GetUserCachePath(user.ID)
		env = append(env, "XDG_CACHE_HOME="+cachePath)

		if effectiveUser.RmapiHost != "" {
			env = append(env, "RMAPI_HOST="+effectiveUser.RmapiHost)
		} else {
			// Remove server-level RMAPI_HOST to use official cloud.
			env = filterEnv(env, "RMAPI_HOST")
		}

		cfg, err := GetUserConfigPath(effectiveUser.ID)
		if err != nil {
			return nil, cleanup, err
		}
		env = append(env, "RMAPI_CONFIG="+cfg)
		tempConfigPath = cfg
		cleanup = func() {
			CleanupTempConfigFile(tempConfigPath)
		}
	}

	cmd := ExecCommand("rmapi", args...)
	cmd.Env = env
	return cmd, cleanup, nil
}

// filterEnv removes environment variables with the given prefix from the slice
func filterEnv(env []string, prefix string) []string {
	var filtered []string
	for _, e := range env {
		if !strings.HasPrefix(e, prefix+"=") {
			filtered = append(filtered, e)
		}
	}
	return filtered
}
