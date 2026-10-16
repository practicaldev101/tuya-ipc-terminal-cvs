package rtsp

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/spf13/cobra"

	"tuya-ipc-terminal/cmd/auth"
	"tuya-ipc-terminal/pkg/core"
	"tuya-ipc-terminal/pkg/health"
	"tuya-ipc-terminal/pkg/rtsp"
	"tuya-ipc-terminal/pkg/session"
	"tuya-ipc-terminal/pkg/storage"
	"tuya-ipc-terminal/pkg/tuya"
)

var storageManager *storage.StorageManager
var rtspServer *rtsp.RTSPServer

func SetStorageManager(sm *storage.StorageManager) {
	storageManager = sm
}

func NewRTSPCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "rtsp",
		Short: "Manage RTSP server",
		Long:  "Commands to start, stop, and manage the RTSP server for camera streaming.",
	}

	cmd.AddCommand(newStartCmd())
	cmd.AddCommand(newStopCmd())
	cmd.AddCommand(newStatusCmd())
	cmd.AddCommand(newListEndpointsCmd())

	return cmd
}

func newStartCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "start",
		Short: "Start RTSP server",
		Long:  "Start the RTSP server to provide camera streams.",
		RunE:  runStartServer,
	}

	cmd.Flags().String("listen", rtsp.DefaultListenAddress, "RTSP server listening address (host or IP). Defaults to loopback only; use \"0.0.0.0\" or \"::\" to listen on all interfaces [env TUYA_RTSP_LISTEN]")
	cmd.Flags().StringP("address", "a", rtsp.DefaultListenAddress, "Deprecated alias for --listen")
	_ = cmd.Flags().MarkDeprecated("address", "use --listen instead")
	cmd.Flags().IntP("port", "p", 8554, "RTSP server port")
	cmd.Flags().Duration("media-timeout", rtsp.DefaultMediaTimeout, "Tear a stream down (even with RTSP clients attached) when no video RTP arrived for this long; 0 disables [env TUYA_MEDIA_TIMEOUT]")
	cmd.Flags().BoolP("daemon", "d", false, "Run as daemon (background)")

	// Session keeper (automatic re-login)
	cmd.Flags().Duration("session-check-interval", session.DefaultCheckInterval, "How often stored Tuya sessions are validated and renewed; 0 disables the session keeper [env TUYA_SESSION_CHECK_INTERVAL]")
	cmd.Flags().Duration("session-renew-margin", session.DefaultRenewMargin, "Re-login this long before the session's expected expiry [env TUYA_SESSION_RENEW_MARGIN]")
	cmd.Flags().Duration("session-lifetime", session.DefaultSessionLifetime, "Assumed session lifetime after login when cookies carry no expiry [env TUYA_SESSION_LIFETIME]")
	cmd.Flags().String("health-listen", "", "Serve JSON health at http://<addr>/healthz; \"8556\" or \":8556\" binds 127.0.0.1:8556. Empty disables [env TUYA_HEALTH_LISTEN]")
	cmd.Flags().String("credentials-file", "", "File with TUYA_EMAIL=, TUYA_PASSWORD= and optional TUYA_REGION= lines for non-interactive re-login (overrides those env vars) [env TUYA_CREDENTIALS_FILE]")

	return cmd
}

func newStopCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "stop",
		Short: "Stop RTSP server",
		Long:  "Stop the running RTSP server.",
		RunE:  runStopServer,
	}
}

func newStatusCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "status",
		Short: "Show RTSP server status",
		Long:  "Display current status and statistics of the RTSP server.",
		RunE:  runServerStatus,
	}
}

func newListEndpointsCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "list-endpoints",
		Short: "List available RTSP endpoints",
		Long:  "Display all available RTSP camera endpoints.",
		RunE:  runListEndpoints,
	}

	cmd.Flags().BoolP("online-only", "o", false, "Show only online cameras")

	return cmd
}

func runStartServer(cmd *cobra.Command, args []string) error {
	address := stringFlagOrEnv(cmd, "listen", "TUYA_RTSP_LISTEN")
	if !cmd.Flags().Changed("listen") && cmd.Flags().Changed("address") {
		address, _ = cmd.Flags().GetString("address")
	}
	mediaTimeout, err := durationFlagOrEnv(cmd, "media-timeout", "TUYA_MEDIA_TIMEOUT")
	if err != nil {
		return err
	}
	sessionCfg, err := sessionConfigFromFlags(cmd)
	if err != nil {
		return err
	}
	healthListen := stringFlagOrEnv(cmd, "health-listen", "TUYA_HEALTH_LISTEN")
	port, _ := cmd.Flags().GetInt("port")
	daemon, _ := cmd.Flags().GetBool("daemon")

	// Check if we have any authenticated users
	users, err := storageManager.ListUsers()
	if err != nil {
		return fmt.Errorf("failed to check users: %v", err)
	}

	if len(users) == 0 {
		core.Logger.Warn().Msg("No authenticated users found.")
		core.Logger.Warn().Msg("Please add users first:")
		core.Logger.Warn().Msg("  tuya-ipc-terminal auth add [region] [email]")
		return errors.New("no authenticated users")
	}

	// Check if we have any cameras
	cameras, err := storageManager.GetAllCameras()
	if err != nil {
		return fmt.Errorf("failed to check cameras: %v", err)
	}

	if len(cameras) == 0 {
		core.Logger.Warn().Msg("No cameras found.")
		core.Logger.Warn().Msg("Please refresh camera discovery:")
		core.Logger.Warn().Msg("  tuya-ipc-terminal cameras refresh")
		return errors.New("no cameras found")
	}

	// Create and start RTSP server
	rtspServer = rtsp.NewRTSPServer(address, port, storageManager)
	rtspServer.SetMediaTimeout(mediaTimeout)

	core.Logger.Info().Msgf("Starting RTSP server on %s...", rtsp.ListenAddress(address, port))

	if err := rtspServer.Start(); err != nil {
		return fmt.Errorf("failed to start RTSP server: %v", err)
	}

	backgroundCtx, stopBackground := context.WithCancel(context.Background())
	defer stopBackground()

	var sessions health.SessionSource
	if sessionCfg != nil {
		keeper := session.NewKeeper(*sessionCfg, storageManager, session.TuyaValidate, session.TuyaPasswordLogin(lookupRegion))
		core.Logger.Info().Msgf("Session keeper enabled: check every %v, renew %v before expiry, credentials configured: %v",
			sessionCfg.CheckInterval, sessionCfg.RenewMargin, sessionCfg.Credentials != nil)
		go keeper.Run(backgroundCtx)
		sessions = keeper
	} else {
		core.Logger.Warn().Msg("Session keeper disabled: stored sessions will not be validated or renewed")
	}

	if healthListen != "" {
		addr, err := health.Serve(backgroundCtx, healthListen, health.Handler(cmd.Root().Version, rtspServer, sessions))
		if err != nil {
			_ = rtspServer.Stop()
			return fmt.Errorf("failed to start health endpoint: %v", err)
		}
		core.Logger.Info().Msgf("Health endpoint: http://%s/healthz", addr)
	}

	if daemon {
		core.Logger.Info().Msgf("RTSP server started in daemon mode")
		return nil
	}

	// Wait for interrupt signal
	core.Logger.Info().Msgf("RTSP server is running. Press Ctrl+C to stop.")

	signalChan := make(chan os.Signal, 1)
	signal.Notify(signalChan, syscall.SIGINT, syscall.SIGTERM)

	<-signalChan

	core.Logger.Info().Msgf("Shutting down RTSP server...")
	if err := rtspServer.Stop(); err != nil {
		return fmt.Errorf("error stopping server: %v", err)
	}

	core.Logger.Info().Msgf("RTSP server stopped.")
	return nil
}

func runStopServer(cmd *cobra.Command, args []string) error {
	if rtspServer == nil {
		return errors.New("no RTSP server instance found")
	}

	if !rtspServer.IsRunning() {
		core.Logger.Info().Msg("RTSP server is not running")
		return nil
	}

	core.Logger.Info().Msg("Stopping RTSP server...")
	if err := rtspServer.Stop(); err != nil {
		return fmt.Errorf("failed to stop server: %v", err)
	}

	core.Logger.Info().Msg("RTSP server stopped.")
	return nil
}

func runServerStatus(cmd *cobra.Command, args []string) error {
	if rtspServer == nil {
		fmt.Println("RTSP Server Status: Not initialized")
		return nil
	}

	stats := rtspServer.GetStats()

	fmt.Println("RTSP Server Status:")
	fmt.Println("==================")
	fmt.Printf("Port: %d\n", stats.Port)
	fmt.Printf("Running: %v\n", stats.Running)
	fmt.Printf("Connected clients: %d\n", stats.ClientCount)
	fmt.Printf("Active streams: %d\n", stats.StreamCount)
	fmt.Printf("Total streams: %d\n", stats.TotalStreams)

	if stats.Running {
		fmt.Printf("\nServer has been running since startup\n")
		fmt.Printf("Access cameras via: rtsp://localhost:%d/[camera-path]\n", stats.Port)
	}

	return nil
}

func runListEndpoints(cmd *cobra.Command, args []string) error {
	cameras, err := storageManager.GetAllCameras()
	if err != nil {
		return fmt.Errorf("failed to get cameras: %v", err)
	}

	if len(cameras) == 0 {
		fmt.Println("No cameras found.")
		fmt.Println("Run 'tuya-ipc-terminal cameras refresh' to discover cameras.")
		return nil
	}

	if len(cameras) == 0 {
		fmt.Println("No online cameras found.")
		return nil
	}

	port := 8554
	if rtspServer != nil {
		port = rtspServer.GetPort()
	}

	fmt.Println("Available RTSP Endpoints:")
	fmt.Println("========================")

	for i, camera := range cameras {
		fmt.Printf("%d. %s\n", i+1, camera.DeviceName)
		fmt.Printf("   URL: rtsp://localhost:%d%s\n", port, camera.RTSPPath)
		fmt.Printf("   Device ID: %s\n", camera.DeviceID)
		fmt.Printf("   User: %s\n", camera.UserKey)
		fmt.Println()
	}

	registry, err := storageManager.GetCameraRegistry()
	if err == nil && !registry.LastUpdated.IsZero() {
		fmt.Printf("Camera list last updated: %s\n", registry.LastUpdated.Format("2006-01-02 15:04:05"))

		if time.Since(registry.LastUpdated) > 24*time.Hour {
			fmt.Println("⚠️  Camera list is old. Consider running 'cameras refresh'")
		}
	}

	fmt.Printf("Example usage:\n")
	fmt.Printf("  ffplay rtsp://localhost:%d%s\n", port, cameras[0].RTSPPath)
	fmt.Printf("  vlc rtsp://localhost:%d%s\n", port, cameras[0].RTSPPath)

	return nil
}

// sessionConfigFromFlags returns nil when the session keeper is disabled.
func sessionConfigFromFlags(cmd *cobra.Command) (*session.Config, error) {
	interval, err := durationFlagOrEnv(cmd, "session-check-interval", "TUYA_SESSION_CHECK_INTERVAL")
	if err != nil {
		return nil, err
	}
	if interval <= 0 {
		return nil, nil
	}
	margin, err := durationFlagOrEnv(cmd, "session-renew-margin", "TUYA_SESSION_RENEW_MARGIN")
	if err != nil {
		return nil, err
	}
	lifetime, err := durationFlagOrEnv(cmd, "session-lifetime", "TUYA_SESSION_LIFETIME")
	if err != nil {
		return nil, err
	}

	credentialsFile := stringFlagOrEnv(cmd, "credentials-file", session.EnvCredentialsFile)
	credentials, err := session.LoadCredentials(credentialsFile, os.Getenv)
	if err != nil {
		return nil, fmt.Errorf("session keeper: %v", err)
	}

	return &session.Config{
		CheckInterval:   interval,
		RenewMargin:     margin,
		SessionLifetime: lifetime,
		Credentials:     credentials,
	}, nil
}

func lookupRegion(name string) (tuya.Region, bool) {
	for _, region := range auth.AvailableRegions {
		if region.Name == name {
			return region, true
		}
	}
	return tuya.Region{}, false
}
