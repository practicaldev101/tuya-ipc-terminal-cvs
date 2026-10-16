package rtsp

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"net"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"tuya-ipc-terminal/pkg/session"
	"tuya-ipc-terminal/pkg/storage"
	"tuya-ipc-terminal/pkg/tuya"
)

type testClock struct {
	mu  sync.Mutex
	now time.Time
}

func newTestClock() *testClock {
	return &testClock{now: time.Date(2026, 10, 16, 12, 0, 0, 0, time.UTC)}
}

func (c *testClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

func (c *testClock) Advance(d time.Duration) {
	c.mu.Lock()
	c.now = c.now.Add(d)
	c.mu.Unlock()
}

var errSessionInvalid = fmt.Errorf("failed to get app info: %w", &tuya.APIError{StatusCode: 200, Msg: "USER_SESSION_INVALID"})

// fakeStarter replaces WebRTCBridge.Start and counts calls.
type fakeStarter struct {
	calls  atomic.Int32
	called chan struct{}

	mu     sync.Mutex
	result error
}

func newFakeStarter(result error) *fakeStarter {
	return &fakeStarter{called: make(chan struct{}, 64), result: result}
}

func (f *fakeStarter) set(result error) {
	f.mu.Lock()
	f.result = result
	f.mu.Unlock()
}

func (f *fakeStarter) start(*CameraStream) error {
	f.calls.Add(1)
	f.called <- struct{}{}
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.result
}

func TestStartBackoffDelaySequence(t *testing.T) {
	want := []time.Duration{0, 1, 2, 4, 8, 16, 30, 30, 30}
	for failures, seconds := range want {
		if got := startBackoffDelay(failures); got != seconds*time.Second {
			t.Errorf("startBackoffDelay(%d) = %v, want %ds", failures, got, seconds)
		}
	}
	if got := startBackoffDelay(1000); got != MaxStartBackoff {
		t.Errorf("startBackoffDelay(1000) = %v, want cap", got)
	}
}

func newBackoffTestServer(clock *testClock, starter *fakeStarter) *RTSPServer {
	return &RTSPServer{
		clients:       make(map[string]*RTSPClient),
		streams:       make(map[string]*CameraStream),
		startBackoffs: make(map[string]*startBackoff),
		now:           clock.Now,
		startBridge:   starter.start,
	}
}

var (
	backoffCamera = &storage.CameraInfo{DeviceName: "CORN CHM-302", DeviceID: "dev1", RTSPPath: "/CORN_CHM-302", UserKey: "eu-central_owner"}
	backoffUser   = &storage.UserSession{UserKey: "eu-central_owner", Region: "eu-central"}
)

// clientAttempt does what handleConnection does for a new RTSP client:
// get or create the stream and attach. It returns the backoff error (no
// start) or waits until the start it triggered has finished.
func clientAttempt(t *testing.T, s *RTSPServer, n int) (*CameraStream, error) {
	t.Helper()
	stream, err := s.getOrCreateStream(backoffCamera, "sd", backoffUser)
	if err != nil {
		return nil, err
	}
	if err := stream.AddClient(&RTSPClient{session: fmt.Sprintf("client-%d", n)}); err != nil {
		t.Fatalf("attach: %v", err)
	}
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		stream.mutex.RLock()
		finished := stream.dead || stream.active
		stream.mutex.RUnlock()
		if finished {
			return stream, nil
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatal("stream start did not finish")
	return nil, nil
}

func TestStartBackoffRefusesAttemptsWithoutStarting(t *testing.T) {
	clock := newTestClock()
	starter := newFakeStarter(errSessionInvalid)
	s := newBackoffTestServer(clock, starter)
	attempt := 0
	try := func() error {
		attempt++
		_, err := clientAttempt(t, s, attempt)
		return err
	}

	// Failure k opens a window of 1, 2, 4, 8, 16, 30, 30 s.
	for k, window := range []time.Duration{1, 2, 4, 8, 16, 30, 30} {
		window *= time.Second
		if err := try(); err != nil {
			t.Fatalf("failure %d: attempt after the window was refused: %v", k+1, err)
		}
		if got := starter.calls.Load(); got != int32(k+1) {
			t.Fatalf("failure %d: start calls = %d", k+1, got)
		}
		// A burst of reconnects inside the window never reaches Tuya.
		for i := 0; i < 20; i++ {
			var backoff *errStartBackoff
			if err := try(); !errors.As(err, &backoff) {
				t.Fatalf("failure %d: attempt inside the window not refused: %v", k+1, err)
			}
		}
		clock.Advance(window - time.Millisecond)
		var backoff *errStartBackoff
		if err := try(); !errors.As(err, &backoff) {
			t.Fatalf("failure %d: window shorter than %v", k+1, window)
		} else if backoff.retryAfterSeconds(clock.Now()) != 1 {
			t.Fatalf("retry-after = %d, want 1", backoff.retryAfterSeconds(clock.Now()))
		}
		if got := starter.calls.Load(); got != int32(k+1) {
			t.Fatalf("failure %d: refused attempts called start (%d calls)", k+1, got)
		}
		clock.Advance(time.Millisecond)
	}

	// Success clears the backoff.
	starter.set(nil)
	attempt++
	stream, err := clientAttempt(t, s, attempt)
	if err != nil || stream == nil || !stream.activeForTest() {
		t.Fatalf("start after the window failed: %v", err)
	}
	if len(s.startBackoffs) != 0 {
		t.Fatalf("backoff not reset after success: %+v", s.startBackoffs)
	}
	stream.teardown()

	// The next failure starts again at 1 s.
	starter.set(errors.New("failed to establish connection: timeout"))
	if err := try(); err != nil {
		t.Fatal(err)
	}
	clock.Advance(time.Second - time.Millisecond)
	if err := try(); err == nil {
		t.Fatal("first window after reset must be 1 s")
	}
	clock.Advance(time.Millisecond)
	before := starter.calls.Load()
	if err := try(); err != nil || starter.calls.Load() != before+1 {
		t.Fatalf("attempt after the 1 s window must start: %v", err)
	}
}

func (cs *CameraStream) activeForTest() bool {
	cs.mutex.RLock()
	defer cs.mutex.RUnlock()
	return cs.active && !cs.dead
}

func TestStartFailureHealthAndSessionHook(t *testing.T) {
	clock := newTestClock()
	starter := newFakeStarter(fmt.Errorf("failed to connect to MQTT: dial wss://web_x:secret@m1.example.invalid/mqtt: %w",
		&tuya.APIError{StatusCode: 403, Body: strings.Repeat("x", 1000)}))
	s := newBackoffTestServer(clock, starter)
	var hookCalls []string
	s.SetSessionRejectedHandler(func(userKey string, err error) { hookCalls = append(hookCalls, userKey) })

	if _, err := clientAttempt(t, s, 1); err != nil {
		t.Fatal(err)
	}
	if len(hookCalls) != 1 || hookCalls[0] != backoffUser.UserKey {
		t.Fatalf("session hook calls = %v, want one for %s", hookCalls, backoffUser.UserKey)
	}

	health := s.StreamsHealth()
	if len(health) != 1 {
		t.Fatalf("failed stream must stay visible, got %+v", health)
	}
	h := health[0]
	if h.State != "stopped" || h.Device != "CORN CHM-302" || h.Resolution != "sd" {
		t.Fatalf("unexpected health %+v", h)
	}
	if !h.NextStartAttemptAt.Equal(clock.Now().Add(time.Second)) {
		t.Fatalf("next_start_attempt_at = %v", h.NextStartAttemptAt)
	}
	if strings.Contains(h.LastStartError, "secret") || !strings.Contains(h.LastStartError, "wss://<redacted>@m1") {
		t.Fatalf("credentials not redacted: %q", h.LastStartError)
	}
	if len(h.LastStartError) > maxStartErrorLen+3 {
		t.Fatalf("last_start_error not bounded: %d bytes", len(h.LastStartError))
	}

	clock.Advance(time.Second)
	if h := s.StreamsHealth()[0]; !h.NextStartAttemptAt.IsZero() || h.LastStartError == "" {
		t.Fatalf("after the window: next attempt must be null, error kept: %+v", h)
	}

	// Non-rejections (transport errors) do not wake the keeper.
	starter.set(errors.New("failed to get app info: dial tcp: i/o timeout"))
	if _, err := clientAttempt(t, s, 2); err != nil {
		t.Fatal(err)
	}
	if len(hookCalls) != 1 {
		t.Fatalf("transport error called the session hook: %v", hookCalls)
	}
}

func TestStoppedDuringStartupIsNoStartFailure(t *testing.T) {
	clock := newTestClock()
	starter := newFakeStarter(errBridgeStopped)
	s := newBackoffTestServer(clock, starter)
	if _, err := clientAttempt(t, s, 1); err != nil {
		t.Fatal(err)
	}
	if len(s.startBackoffs) != 0 {
		t.Fatal("a deliberate stop must not open a backoff window")
	}
}

// End to end over TCP: a Tuya session rejection at stream start triggers
// exactly one immediate keeper check, the reconnect burst gets RTSP 503
// without a start, and a second rejection within the keeper's min interval
// triggers no further check.
func TestSessionRejectionOverRTSP(t *testing.T) {
	clock := newTestClock()
	starter := newFakeStarter(errSessionInvalid)

	sm, err := storage.NewStorageManagerAt(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if err := sm.SaveUser("eu-central", "owner@example.com", &tuya.SessionData{ServerHost: "example.invalid"}); err != nil {
		t.Fatal(err)
	}
	users, _ := sm.ListUsers()
	camera := *backoffCamera
	camera.UserKey = users[0].UserKey
	if err := sm.SaveCameraRegistry(&storage.CameraRegistry{Cameras: []storage.CameraInfo{camera}}); err != nil {
		t.Fatal(err)
	}

	var checks atomic.Int32
	checked := make(chan struct{}, 16)
	validate := func(context.Context, *tuya.SessionData) error {
		checks.Add(1)
		checked <- struct{}{}
		return errors.New("USER_SESSION_INVALID")
	}
	keeper := session.NewKeeper(session.Config{Now: clock.Now}, sm, validate, nil)

	s := NewRTSPServer("127.0.0.1", 0, sm)
	s.now = clock.Now
	s.startBridge = starter.start
	s.SetMediaTimeout(0)
	s.SetSessionRejectedHandler(func(userKey string, err error) { keeper.RequestCheck(userKey, err.Error()) })
	if err := s.Start(); err != nil {
		t.Fatal(err)
	}
	defer s.Stop()
	addr := s.listener.Addr().String()

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { keeper.Run(ctx); close(done) }()
	defer func() { cancel(); <-done }()
	waitFor := func(ch <-chan struct{}, what string) {
		t.Helper()
		select {
		case <-ch:
		case <-time.After(2 * time.Second):
			t.Fatalf("timed out waiting for %s", what)
		}
	}
	waitFor(checked, "startup check")

	// options sends OPTIONS and returns the status line ("" when the
	// connection was closed before the response: the fake start fails
	// instantly, so its teardown can beat the OPTIONS answer) and headers.
	// The server must close the connection in any case.
	options := func() (string, map[string]string) {
		t.Helper()
		conn, err := net.DialTimeout("tcp", addr, time.Second)
		if err != nil {
			t.Fatal(err)
		}
		defer conn.Close()
		_ = conn.SetDeadline(time.Now().Add(3 * time.Second))
		fmt.Fprintf(conn, "OPTIONS rtsp://%s/CORN_CHM-302/sd RTSP/1.0\r\nCSeq: 7\r\n\r\n", addr)
		reader := bufio.NewReader(conn)
		status, err := reader.ReadString('\n')
		if err != nil {
			return "", nil
		}
		headers := map[string]string{}
		for {
			line, err := reader.ReadString('\n')
			if err != nil || strings.TrimSpace(line) == "" {
				break
			}
			if k, v, ok := strings.Cut(line, ":"); ok {
				headers[strings.TrimSpace(k)] = strings.TrimSpace(v)
			}
		}
		// The server must close the connection (teardown or refusal).
		if _, err := reader.ReadByte(); err == nil {
			t.Fatal("expected the server to close the connection")
		}
		return strings.TrimSpace(status), headers
	}

	// First client: the start is attempted and rejected by Tuya.
	if status, _ := options(); status != "" && !strings.HasPrefix(status, "RTSP/1.0 200") {
		t.Fatalf("first OPTIONS: %s", status)
	}
	waitFor(starter.called, "first start")
	waitFor(checked, "immediate on-demand check")

	// Reconnect burst within the window: 503 + Retry-After, no start.
	for i := 0; i < 10; i++ {
		status, headers := options()
		if !strings.HasPrefix(status, "RTSP/1.0 503") || headers["Retry-After"] != "1" || headers["CSeq"] != "7" {
			t.Fatalf("refused attempt: %s %v", status, headers)
		}
	}
	if got := starter.calls.Load(); got != 1 {
		t.Fatalf("start calls during the window = %d, want 1", got)
	}

	// After the window the next start fails again, within the keeper's
	// 30 s min interval: no further check.
	clock.Advance(time.Second)
	options()
	waitFor(starter.called, "second start")
	select {
	case <-checked:
		t.Fatal("second rejection within the min interval triggered another check")
	case <-time.After(300 * time.Millisecond):
	}
	if got := checks.Load(); got != 2 {
		t.Fatalf("keeper checks = %d, want 2 (startup + one on-demand)", got)
	}

	// Once the min interval passed, a rejection triggers a check again.
	clock.Advance(session.DefaultRecheckMinInterval)
	options()
	waitFor(starter.called, "third start")
	waitFor(checked, "second on-demand check")
}
