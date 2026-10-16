package rtsp

import (
	"fmt"
	"regexp"
	"time"

	"tuya-ipc-terminal/pkg/core"
	"tuya-ipc-terminal/pkg/storage"
	"tuya-ipc-terminal/pkg/tuya"
)

// Stream start backoff (CVS patch).
//
// A recorder (FFmpeg, go2rtc) reconnects as soon as a stream start fails;
// without a limit every reconnect made several Tuya API calls (observed:
// USER_SESSION_INVALID about every 130 ms). After a failed start of a
// camera/resolution, further starts of the same stream within the backoff
// window are refused with RTSP 503 (Retry-After) without calling Tuya. The
// window doubles per consecutive failure from MinStartBackoff up to
// MaxStartBackoff and is cleared by a successful start. A start that Tuya
// rejected (HTTP 401/403 or success=false) also asks the session keeper for
// an immediate check of the camera's user.

const (
	MinStartBackoff = time.Second
	MaxStartBackoff = 30 * time.Second
	// maxStartErrorLen bounds last_start_error (HTTP error bodies can be
	// whole HTML pages).
	maxStartErrorLen = 256
)

// startBackoffDelay returns the window after the given number of
// consecutive failures (>= 1): 1, 2, 4, 8, 16, 30, 30, ... seconds.
func startBackoffDelay(failures int) time.Duration {
	if failures < 1 {
		return 0
	}
	delay := MinStartBackoff
	for i := 1; i < failures && delay < MaxStartBackoff; i++ {
		delay *= 2
	}
	if delay > MaxStartBackoff {
		delay = MaxStartBackoff
	}
	return delay
}

// startBackoff is the failure state of one stream id (device-resolution).
// It outlives the (dead) CameraStream so it is guarded by RTSPServer.mutex.
type startBackoff struct {
	device, deviceID, rtspPath, resolution string

	failures      int
	lastError     string
	failedAt      time.Time
	nextAttemptAt time.Time
}

// errStartBackoff is returned by getOrCreateStream while a stream's start is
// backing off.
type errStartBackoff struct {
	retryAt time.Time
}

func (e *errStartBackoff) Error() string {
	return fmt.Sprintf("stream start backing off until %s", e.retryAt.UTC().Format(time.RFC3339))
}

// retryAfterSeconds is the Retry-After value (whole seconds, at least 1).
func (e *errStartBackoff) retryAfterSeconds(now time.Time) int {
	wait := e.retryAt.Sub(now)
	seconds := int((wait + time.Second - 1) / time.Second)
	if seconds < 1 {
		seconds = 1
	}
	return seconds
}

// SessionRejectedFunc is called when Tuya rejected a stream start for the
// camera's user (see tuya.IsAPIRejection). It must not block.
type SessionRejectedFunc func(userKey string, err error)

// SetSessionRejectedHandler installs the callback for stream starts
// rejected by Tuya (typically session.Keeper.RequestCheck). Must be called
// before Start.
func (s *RTSPServer) SetSessionRejectedHandler(handler SessionRejectedFunc) {
	s.mutex.Lock()
	defer s.mutex.Unlock()
	s.onSessionRejected = handler
}

// checkStartBackoffLocked returns an *errStartBackoff while the start of
// streamId is backing off. Requires s.mutex.
func (s *RTSPServer) checkStartBackoffLocked(streamId string) error {
	entry := s.startBackoffs[streamId]
	if entry == nil {
		return nil
	}
	if s.clock().Before(entry.nextAttemptAt) {
		return &errStartBackoff{retryAt: entry.nextAttemptAt}
	}
	return nil
}

// recordStartFailure extends the backoff of the stream and returns the
// consecutive failure count and the new window.
func (s *RTSPServer) recordStartFailure(cs *CameraStream, err error) (int, time.Duration) {
	now := s.clock()
	s.mutex.Lock()
	defer s.mutex.Unlock()
	if s.startBackoffs == nil {
		s.startBackoffs = make(map[string]*startBackoff)
	}
	entry := s.startBackoffs[cs.streamId]
	if entry == nil {
		entry = &startBackoff{resolution: cs.resolution}
		if cs.camera != nil {
			entry.device = cs.camera.DeviceName
			entry.deviceID = cs.camera.DeviceID
			entry.rtspPath = cs.camera.RTSPPath
		}
		s.startBackoffs[cs.streamId] = entry
	}
	entry.failures++
	entry.lastError = sanitizeStartError(err)
	entry.failedAt = now
	delay := startBackoffDelay(entry.failures)
	entry.nextAttemptAt = now.Add(delay)
	return entry.failures, delay
}

// recordStartSuccess clears the backoff of the stream and returns how many
// consecutive failures preceded the success.
func (s *RTSPServer) recordStartSuccess(streamId string) int {
	s.mutex.Lock()
	defer s.mutex.Unlock()
	entry := s.startBackoffs[streamId]
	if entry == nil {
		return 0
	}
	delete(s.startBackoffs, streamId)
	return entry.failures
}

// reportSessionRejected forwards a Tuya rejection of a stream start to the
// session rejected handler.
func (s *RTSPServer) reportSessionRejected(user *storage.UserSession, err error) {
	if user == nil || !tuya.IsAPIRejection(err) {
		return
	}
	s.mutex.RLock()
	handler := s.onSessionRejected
	s.mutex.RUnlock()
	if handler != nil {
		handler(user.UserKey, err)
	}
}

var urlUserinfo = regexp.MustCompile(`([A-Za-z][A-Za-z0-9+.-]*://)[^/@\s]+@`)

// sanitizeStartError makes an error safe for the health document: URL
// userinfo is redacted and the text is bounded. (Start errors carry Tuya
// API messages and URLs; cookies and the account password never appear in
// them.)
func sanitizeStartError(err error) string {
	if err == nil {
		return ""
	}
	text := urlUserinfo.ReplaceAllString(err.Error(), "${1}<redacted>@")
	if len(text) > maxStartErrorLen {
		cut := maxStartErrorLen
		for cut > 0 && text[cut]&0xC0 == 0x80 { // keep UTF-8 valid
			cut--
		}
		text = text[:cut] + "..."
	}
	return text
}

// logStartFailure logs one ERROR per failed start attempt; attempts refused
// by the backoff are not starts and log at debug level only.
func logStartFailure(cs *CameraStream, err error, failures int, retryIn time.Duration) {
	core.Logger.Error().Err(err).
		Str("device", cs.cameraName()).
		Str("resolution", cs.resolution).
		Int("consecutive_failures", failures).
		Dur("retry_in", retryIn).
		Msgf("Failed to start WebRTC bridge for camera: %s (next start attempt in %v)", cs.cameraName(), retryIn)
}

func (cs *CameraStream) cameraName() string {
	if cs.camera == nil {
		return ""
	}
	return cs.camera.DeviceName
}
