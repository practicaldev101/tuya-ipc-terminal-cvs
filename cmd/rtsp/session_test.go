package rtsp

import (
	"path/filepath"
	"testing"

	"github.com/spf13/cobra"

	"tuya-ipc-terminal/pkg/session"
)

func newSessionTestCmd() *cobra.Command {
	cmd := &cobra.Command{Use: "test"}
	cmd.Flags().Duration("session-check-interval", session.DefaultCheckInterval, "")
	cmd.Flags().Duration("session-renew-margin", session.DefaultRenewMargin, "")
	cmd.Flags().Duration("session-lifetime", session.DefaultSessionLifetime, "")
	cmd.Flags().String("credentials-file", "", "")
	return cmd
}

func clearCredentialEnv(t *testing.T) {
	t.Helper()
	for _, key := range []string{session.EnvEmail, session.EnvPassword, session.EnvRegion, session.EnvCredentialsFile} {
		t.Setenv(key, "")
	}
}

// Incomplete credentials must not stop the bridge from starting: the keeper
// runs report-only instead.
func TestIncompleteCredentialsRunKeeperReportOnly(t *testing.T) {
	clearCredentialEnv(t)
	t.Setenv(session.EnvEmail, "owner@example.com")

	cfg, err := sessionConfigFromFlags(newSessionTestCmd())
	if err != nil {
		t.Fatalf("incomplete credentials must not be fatal: %v", err)
	}
	if cfg == nil {
		t.Fatal("session keeper must stay enabled (report-only)")
	}
	if cfg.Credentials != nil {
		t.Fatalf("incomplete credentials must not be used: %+v", cfg.Credentials)
	}
	if cfg.CheckInterval != session.DefaultCheckInterval {
		t.Fatalf("check interval = %v", cfg.CheckInterval)
	}
}

func TestUnreadableCredentialsFileRunsKeeperReportOnly(t *testing.T) {
	clearCredentialEnv(t)
	cmd := newSessionTestCmd()
	_ = cmd.Flags().Set("credentials-file", filepath.Join(t.TempDir(), "missing.env"))

	cfg, err := sessionConfigFromFlags(cmd)
	if err != nil || cfg == nil || cfg.Credentials != nil {
		t.Fatalf("unreadable credentials file must run report-only, got cfg=%+v err=%v", cfg, err)
	}
}

func TestCompleteCredentialsAreUsed(t *testing.T) {
	clearCredentialEnv(t)
	t.Setenv(session.EnvEmail, "owner@example.com")
	t.Setenv(session.EnvPassword, "pw")

	cfg, err := sessionConfigFromFlags(newSessionTestCmd())
	if err != nil || cfg == nil || cfg.Credentials == nil || cfg.Credentials.Email != "owner@example.com" {
		t.Fatalf("complete credentials not loaded: cfg=%+v err=%v", cfg, err)
	}
}

func TestZeroCheckIntervalDisablesKeeper(t *testing.T) {
	clearCredentialEnv(t)
	t.Setenv("TUYA_SESSION_CHECK_INTERVAL", "0")
	cfg, err := sessionConfigFromFlags(newSessionTestCmd())
	if err != nil || cfg != nil {
		t.Fatalf("interval 0 must disable the keeper, got cfg=%+v err=%v", cfg, err)
	}
}
