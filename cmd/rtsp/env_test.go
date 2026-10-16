package rtsp

import (
	"testing"
	"time"

	"github.com/spf13/cobra"
)

func newTestCmd() *cobra.Command {
	cmd := &cobra.Command{Use: "test"}
	cmd.Flags().Duration("media-timeout", 20*time.Second, "")
	cmd.Flags().String("listen", "127.0.0.1", "")
	return cmd
}

func TestDurationFlagOrEnv(t *testing.T) {
	cmd := newTestCmd()
	if got, _ := durationFlagOrEnv(cmd, "media-timeout", "CVS_TEST_TIMEOUT"); got != 20*time.Second {
		t.Fatalf("default = %v", got)
	}

	t.Setenv("CVS_TEST_TIMEOUT", "45")
	if got, _ := durationFlagOrEnv(cmd, "media-timeout", "CVS_TEST_TIMEOUT"); got != 45*time.Second {
		t.Fatalf("env integer seconds = %v", got)
	}

	t.Setenv("CVS_TEST_TIMEOUT", "1m")
	if got, _ := durationFlagOrEnv(cmd, "media-timeout", "CVS_TEST_TIMEOUT"); got != time.Minute {
		t.Fatalf("env duration = %v", got)
	}

	t.Setenv("CVS_TEST_TIMEOUT", "soon")
	if _, err := durationFlagOrEnv(cmd, "media-timeout", "CVS_TEST_TIMEOUT"); err == nil {
		t.Fatal("expected error for invalid env duration")
	}

	_ = cmd.Flags().Set("media-timeout", "5s")
	if got, _ := durationFlagOrEnv(cmd, "media-timeout", "CVS_TEST_TIMEOUT"); got != 5*time.Second {
		t.Fatalf("explicit flag must win over env, got %v", got)
	}
}

func TestStringFlagOrEnv(t *testing.T) {
	cmd := newTestCmd()
	t.Setenv("CVS_TEST_LISTEN", "0.0.0.0")
	if got := stringFlagOrEnv(cmd, "listen", "CVS_TEST_LISTEN"); got != "0.0.0.0" {
		t.Fatalf("env value = %q", got)
	}
	_ = cmd.Flags().Set("listen", "127.0.0.1")
	if got := stringFlagOrEnv(cmd, "listen", "CVS_TEST_LISTEN"); got != "127.0.0.1" {
		t.Fatalf("flag must win, got %q", got)
	}
}
