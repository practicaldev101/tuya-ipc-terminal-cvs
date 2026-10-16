package rtsp

import (
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/spf13/cobra"
)

// Flags added by the CVS fork can also be set through environment variables
// (convenient for systemd EnvironmentFile= and container deployments). An
// explicitly passed command-line flag always wins over the environment.

func stringFlagOrEnv(cmd *cobra.Command, name, env string) string {
	value, _ := cmd.Flags().GetString(name)
	if cmd.Flags().Changed(name) {
		return value
	}
	if fromEnv, ok := os.LookupEnv(env); ok {
		return strings.TrimSpace(fromEnv)
	}
	return value
}

func durationFlagOrEnv(cmd *cobra.Command, name, env string) (time.Duration, error) {
	value, _ := cmd.Flags().GetDuration(name)
	if cmd.Flags().Changed(name) {
		return value, nil
	}
	if fromEnv, ok := os.LookupEnv(env); ok && strings.TrimSpace(fromEnv) != "" {
		parsed, err := parseDuration(strings.TrimSpace(fromEnv))
		if err != nil {
			return 0, fmt.Errorf("invalid %s=%q: %v", env, fromEnv, err)
		}
		return parsed, nil
	}
	return value, nil
}

// parseDuration accepts Go durations ("20s", "6h") and bare integers, which
// are interpreted as seconds.
func parseDuration(value string) (time.Duration, error) {
	if d, err := time.ParseDuration(value); err == nil {
		return d, nil
	}
	var seconds int64
	if _, err := fmt.Sscanf(value, "%d", &seconds); err == nil && fmt.Sprint(seconds) == value {
		return time.Duration(seconds) * time.Second, nil
	}
	return 0, fmt.Errorf("not a duration (e.g. 20s, 6h) or integer seconds")
}
