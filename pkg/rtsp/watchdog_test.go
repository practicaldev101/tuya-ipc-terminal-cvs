package rtsp

import (
	"context"
	"errors"
	"net"
	"sync"
	"testing"
	"time"

	"tuya-ipc-terminal/pkg/storage"

	"github.com/pion/rtp"
)

func newWatchdogTestServer(now time.Time) *RTSPServer {
	ctx, cancel := context.WithCancel(context.Background())
	return &RTSPServer{
		clients: make(map[string]*RTSPClient),
		streams: make(map[string]*CameraStream),
		ctx:     ctx,
		cancel:  cancel,
		now:     func() time.Time { return now },
	}
}

// addActiveStream registers an active stream with one attached RTSP client and
// returns the stream plus the peer side of the client connection.
func addActiveStream(t *testing.T, s *RTSPServer, startedAt time.Time) (*CameraStream, net.Conn) {
	t.Helper()
	camera := &storage.CameraInfo{DeviceID: "dev1", DeviceName: "CORN_CHM-302", RTSPPath: "/CORN_CHM-302"}
	stream := NewCameraStream(camera, "sd", nil, nil, s)
	stream.mutex.Lock()
	stream.active = true
	stream.startedAt = startedAt
	stream.mutex.Unlock()

	serverSide, peer := net.Pipe()
	client := &RTSPClient{conn: serverSide, session: "recorder", stream: stream}
	stream.mutex.Lock()
	stream.clients[client.session] = client
	stream.mutex.Unlock()

	s.mutex.Lock()
	s.streams[stream.streamId] = stream
	s.clients[client.session] = client
	s.mutex.Unlock()
	return stream, peer
}

func TestWatchdogKeepsStreamWithRecentMedia(t *testing.T) {
	now := time.Date(2026, 10, 16, 12, 0, 0, 0, time.UTC)
	s := newWatchdogTestServer(now)
	stream, peer := addActiveStream(t, s, now.Add(-time.Hour))
	defer peer.Close()

	stream.webrtcBridge.rtpForwarder.lastVideoAt.Store(now.Add(-5 * time.Second).UnixNano())

	if stalled := s.checkMediaWatchdog(20*time.Second, DefaultStartupTimeout); len(stalled) != 0 {
		t.Fatalf("watchdog tore down a stream that received media 5s ago")
	}
	if stream.isDead() {
		t.Fatal("stream marked dead despite recent media")
	}
}

func TestWatchdogTearsDownStalledStreamWithClientsAttached(t *testing.T) {
	now := time.Date(2026, 10, 16, 12, 0, 0, 0, time.UTC)
	s := newWatchdogTestServer(now)
	stream, peer := addActiveStream(t, s, now.Add(-time.Hour))
	defer peer.Close()

	stream.webrtcBridge.rtpForwarder.lastVideoAt.Store(now.Add(-21 * time.Second).UnixNano())

	stalled := s.checkMediaWatchdog(20*time.Second, DefaultStartupTimeout)
	if len(stalled) != 1 || stalled[0] != stream {
		t.Fatalf("expected the stalled stream to be returned, got %v", stalled)
	}
	if !stream.isDead() {
		t.Fatal("stalled stream not marked dead")
	}
	s.mutex.RLock()
	_, stillMapped := s.streams[stream.streamId]
	s.mutex.RUnlock()
	if stillMapped {
		t.Fatal("stalled stream still in server map; reconnecting clients would reuse it")
	}

	// The camera still holds the previous WebRTC slot. A reconnect in that
	// quiet period must be refused, even before teardown has finished.
	if _, err := s.getOrCreateStream(stream.camera, "sd", nil); !errors.As(err, new(*errStartBackoff)) {
		t.Fatalf("reconnect during the quiet period = %v, want backoff", err)
	}
	s.now = func() time.Time { return now.Add(MinStartBackoff) }

	// After the quiet period the client must get a brand-new stream, not the dead one.
	fresh, err := s.getOrCreateStream(stream.camera, "sd", nil)
	if err != nil {
		t.Fatal(err)
	}
	if fresh == stream {
		t.Fatal("getOrCreateStream returned the dead stream")
	}
	if err := stream.AddClient(&RTSPClient{session: "late"}); !errors.Is(err, errStreamClosed) {
		t.Fatalf("AddClient on dead stream returned %v, want errStreamClosed", err)
	}

	stream.teardown()

	// The attached recorder connection must be closed so it reconnects.
	_ = peer.SetReadDeadline(time.Now().Add(time.Second))
	if _, err := peer.Read(make([]byte, 1)); err == nil {
		t.Fatal("RTSP client connection still open after teardown")
	}
	if !stream.webrtcBridge.IsStopped() {
		t.Fatal("WebRTC bridge not stopped by teardown")
	}
	if n := stream.webrtcBridge.rtpForwarder.GetClientCount(); n != 0 {
		t.Fatalf("forwarder still has %d clients", n)
	}

	// Second check must not report the same stream again.
	if again := s.checkMediaWatchdog(20*time.Second, DefaultStartupTimeout); len(again) != 0 {
		t.Fatal("dead stream reported twice")
	}
}

// watchdogClock is a settable clock for the watchdog.
type watchdogClock struct {
	mu  sync.Mutex
	now time.Time
}

func (c *watchdogClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

func (c *watchdogClock) Advance(d time.Duration) {
	c.mu.Lock()
	c.now = c.now.Add(d)
	c.mu.Unlock()
}

// startingStream registers a stream whose WebRTC startup began at the
// clock's current time and that has not delivered any video yet.
func startingStream(t *testing.T, clock *watchdogClock) (*RTSPServer, *CameraStream, net.Conn) {
	t.Helper()
	s := newWatchdogTestServer(clock.Now())
	s.now = clock.Now
	stream, peer := addActiveStream(t, s, clock.Now())
	stream.mutex.Lock()
	stream.active = false
	stream.starting = true
	stream.connecting = true
	stream.mutex.Unlock()
	return s, stream, peer
}

// A slow startup (Tuya HTTP calls, MQTT, TURN) is not torn down by the media
// timeout; the media timeout only starts counting at the first video packet.
func TestWatchdogStartupGraceThenMediaTimeout(t *testing.T) {
	clock := &watchdogClock{now: time.Date(2026, 10, 16, 12, 0, 0, 0, time.UTC)}
	s, stream, peer := startingStream(t, clock)
	defer peer.Close()
	const mediaTimeout, startupTimeout = 20 * time.Second, 60 * time.Second

	clock.Advance(30 * time.Second) // cvs.1 tore the stream down here
	if stalled := s.checkMediaWatchdog(mediaTimeout, startupTimeout); len(stalled) != 0 {
		t.Fatal("startup torn down by the media timeout before the first video packet")
	}
	clock.Advance(29 * time.Second) // 59 s into startup
	if stalled := s.checkMediaWatchdog(mediaTimeout, startupTimeout); len(stalled) != 0 {
		t.Fatal("startup torn down before the startup timeout elapsed")
	}

	// First video packet at 59 s; the stream becomes active.
	stream.webrtcBridge.rtpForwarder.lastVideoAt.Store(clock.Now().UnixNano())
	stream.mutex.Lock()
	stream.active, stream.starting, stream.connecting = true, false, false
	stream.mutex.Unlock()

	clock.Advance(19 * time.Second) // 78 s: past the startup timeout, 19 s since media
	if stalled := s.checkMediaWatchdog(mediaTimeout, startupTimeout); len(stalled) != 0 {
		t.Fatal("stream with media 19s ago torn down")
	}
	clock.Advance(time.Second) // 20 s without media
	stalled := s.checkMediaWatchdog(mediaTimeout, startupTimeout)
	if len(stalled) != 1 || stalled[0] != stream {
		t.Fatalf("stream silent for the media timeout not torn down (got %d)", len(stalled))
	}
	stream.teardown()
}

func TestWatchdogStartupTimeoutTearsDownStartupWithoutMedia(t *testing.T) {
	clock := &watchdogClock{now: time.Date(2026, 10, 16, 12, 0, 0, 0, time.UTC)}
	s, stream, peer := startingStream(t, clock)
	defer peer.Close()

	clock.Advance(60 * time.Second)
	stalled := s.checkMediaWatchdog(20*time.Second, 60*time.Second)
	if len(stalled) != 1 || stalled[0] != stream {
		t.Fatalf("startup without media for the startup timeout not torn down (got %d)", len(stalled))
	}
	if !stream.isDead() {
		t.Fatal("stream not marked dead")
	}
	stream.teardown()
}

// Startup timeout 0 falls back to the media timeout (cvs.1 behaviour).
func TestWatchdogZeroStartupTimeoutUsesMediaTimeout(t *testing.T) {
	clock := &watchdogClock{now: time.Date(2026, 10, 16, 12, 0, 0, 0, time.UTC)}
	s, _, peer := startingStream(t, clock)
	defer peer.Close()

	clock.Advance(10 * time.Second)
	if stalled := s.checkMediaWatchdog(20*time.Second, 0); len(stalled) != 0 {
		t.Fatal("startup torn down before the timeout elapsed")
	}
	clock.Advance(20 * time.Second)
	if stalled := s.checkMediaWatchdog(20*time.Second, 0); len(stalled) != 1 {
		t.Fatalf("startup without media for 30s not torn down with startup timeout 0 (got %d)", len(stalled))
	}
}

func TestEffectiveStartupTimeout(t *testing.T) {
	cases := []struct{ media, startup, want time.Duration }{
		{20 * time.Second, 60 * time.Second, 60 * time.Second},
		{20 * time.Second, 0, 20 * time.Second},
		{0, 60 * time.Second, 0},
	}
	for _, c := range cases {
		if got := effectiveStartupTimeout(c.media, c.startup); got != c.want {
			t.Errorf("effectiveStartupTimeout(%v, %v) = %v, want %v", c.media, c.startup, got, c.want)
		}
	}
}

func TestWatchdogDisabledWithZeroTimeout(t *testing.T) {
	now := time.Date(2026, 10, 16, 12, 0, 0, 0, time.UTC)
	s := newWatchdogTestServer(now)
	_, peer := addActiveStream(t, s, now.Add(-time.Hour))
	defer peer.Close()

	if stalled := s.checkMediaWatchdog(0, DefaultStartupTimeout); len(stalled) != 0 {
		t.Fatal("watchdog acted while disabled")
	}
}

func TestForwarderRecordsLastMediaWithoutClients(t *testing.T) {
	forwarder := NewRTPForwarder()
	if !forwarder.LastVideoAt().IsZero() {
		t.Fatal("expected zero last video time before any packet")
	}
	before := time.Now()
	forwarder.ForwardVideoPacket(&rtp.Packet{Header: rtp.Header{PayloadType: 96}, Payload: []byte{0x65, 0x00}})
	forwarder.ForwardAudioPacket(&rtp.Packet{Header: rtp.Header{PayloadType: 0}, Payload: []byte{0x00}})
	if got := forwarder.LastVideoAt(); got.Before(before) {
		t.Fatalf("last video time %v not updated", got)
	}
	if got := forwarder.LastAudioAt(); got.Before(before) {
		t.Fatalf("last audio time %v not updated", got)
	}
}

func TestBridgeStopUnblocksPendingStartAndIsIdempotent(t *testing.T) {
	bridge := NewWebRTCBridge(&storage.CameraInfo{DeviceName: "cam"}, "sd", nil, nil)

	// Simulate Start() blocked waiting for the camera answer while holding
	// the bridge mutex.
	waitDone := make(chan error, 1)
	bridge.mutex.Lock()
	go func() {
		defer bridge.mutex.Unlock()
		waitDone <- bridge.waiter.Wait()
	}()

	stopped := make(chan struct{})
	go func() {
		bridge.Stop()
		bridge.Stop()
		close(stopped)
	}()

	select {
	case err := <-waitDone:
		if !errors.Is(err, errBridgeStopped) {
			t.Fatalf("pending start returned %v, want errBridgeStopped", err)
		}
	case <-time.After(time.Second):
		t.Fatal("Stop did not unblock a pending Start")
	}
	select {
	case <-stopped:
	case <-time.After(time.Second):
		t.Fatal("Stop blocked")
	}
	if err := bridge.Start(); !errors.Is(err, errBridgeStopped) {
		t.Fatalf("Start after Stop returned %v, want errBridgeStopped", err)
	}
}

func TestWatchdogInterval(t *testing.T) {
	cases := map[time.Duration]time.Duration{
		20 * time.Second: 5 * time.Second,
		4 * time.Second:  time.Second,
		time.Second:      250 * time.Millisecond,
		time.Minute:      5 * time.Second,
	}
	for timeout, want := range cases {
		if got := watchdogInterval(timeout); got != want {
			t.Errorf("watchdogInterval(%v) = %v, want %v", timeout, got, want)
		}
	}
}

func TestStreamsHealthReportsStateAndLastMedia(t *testing.T) {
	now := time.Date(2026, 10, 16, 12, 0, 0, 0, time.UTC)
	s := newWatchdogTestServer(now)
	stream, peer := addActiveStream(t, s, now.Add(-time.Minute))
	defer peer.Close()
	last := now.Add(-3 * time.Second)
	stream.webrtcBridge.rtpForwarder.lastVideoAt.Store(last.UnixNano())

	health := s.StreamsHealth()
	if len(health) != 1 {
		t.Fatalf("got %d streams", len(health))
	}
	h := health[0]
	if h.State != "active" || h.Clients != 1 || h.Resolution != "sd" || h.RTSPPath != "/CORN_CHM-302" || !h.LastMediaAt.Equal(last) {
		t.Fatalf("unexpected health %+v", h)
	}
}
