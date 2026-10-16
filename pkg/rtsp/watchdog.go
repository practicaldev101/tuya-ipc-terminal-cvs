package rtsp

import (
	"sort"
	"time"

	"tuya-ipc-terminal/pkg/core"
)

// Media watchdog (CVS patch).
//
// A Tuya WebRTC session can silently stop delivering media while the RTSP
// side stays up: the RTSP TCP connection is healthy, so recorders such as
// FFmpeg keep waiting forever instead of reconnecting. The watchdog tears a
// stream down when no video RTP packet has arrived from the camera for
// mediaTimeout, regardless of attached RTSP clients. Tearing down closes the
// peer connection, the MQTT client and every attached RTSP client
// connection; the stream is removed from the server map first and marked
// dead, so a reconnecting client always negotiates a fresh WebRTC session.

func watchdogInterval(timeout time.Duration) time.Duration {
	interval := timeout / 4
	if interval < 250*time.Millisecond {
		interval = 250 * time.Millisecond
	}
	if interval > 5*time.Second {
		interval = 5 * time.Second
	}
	return interval
}

func (s *RTSPServer) watchdogRoutine(timeout time.Duration) {
	ticker := time.NewTicker(watchdogInterval(timeout))
	defer ticker.Stop()

	for {
		select {
		case <-s.ctx.Done():
			return
		case <-ticker.C:
			for _, stream := range s.checkMediaWatchdog(timeout) {
				// Teardown can block until a pending WebRTC startup step
				// returns; never let one camera delay the others.
				go stream.teardown()
			}
		}
	}
}

// checkMediaWatchdog marks every stalled stream dead and removes it from the
// server map (so no new client can attach to it) and returns those streams.
// The caller must call teardown() on each returned stream.
func (s *RTSPServer) checkMediaWatchdog(timeout time.Duration) []*CameraStream {
	if timeout <= 0 {
		return nil
	}

	now := s.clock()

	s.mutex.RLock()
	streams := make([]*CameraStream, 0, len(s.streams))
	for _, stream := range s.streams {
		streams = append(streams, stream)
	}
	s.mutex.RUnlock()

	var stalled []*CameraStream
	for _, stream := range streams {
		idle, clients, ok := stream.markDeadIfStalled(now, timeout)
		if !ok {
			continue
		}

		core.Logger.Warn().
			Str("component", "watchdog").
			Str("device", stream.camera.DeviceName).
			Str("device_id", stream.camera.DeviceID).
			Str("resolution", stream.resolution).
			Int("clients", clients).
			Dur("idle", idle).
			Msgf("watchdog: stream %s/%s no media for %ds, tearing down",
				stream.camera.DeviceName, stream.resolution, int(idle.Seconds()))

		s.removeStream(stream.streamId, stream)
		stalled = append(stalled, stream)
	}

	return stalled
}

func (s *RTSPServer) clock() time.Time {
	if s.now != nil {
		return s.now()
	}
	return time.Now()
}

// lastMediaLocked returns the last video packet time, or the zero time.
// Requires cs.mutex.
func (cs *CameraStream) lastMediaLocked() time.Time {
	if cs.webrtcBridge != nil && cs.webrtcBridge.rtpForwarder != nil {
		return cs.webrtcBridge.rtpForwarder.LastVideoAt()
	}
	return time.Time{}
}

// markDeadIfStalled marks a live stream dead when it has not received video
// for at least timeout (measured from the later of the current startup and
// the last video packet). It returns the idle time and client count.
func (cs *CameraStream) markDeadIfStalled(now time.Time, timeout time.Duration) (time.Duration, int, bool) {
	cs.mutex.Lock()
	defer cs.mutex.Unlock()

	if cs.dead || !(cs.active || cs.starting || cs.connecting) {
		return 0, 0, false
	}

	last := cs.startedAt
	if media := cs.lastMediaLocked(); media.After(last) {
		last = media
	}
	if last.IsZero() {
		return 0, 0, false
	}

	idle := now.Sub(last)
	if idle < timeout {
		return 0, 0, false
	}

	cs.dead = true
	return idle, len(cs.clients), true
}

// teardown stops the stream for good: it detaches and disconnects every RTSP
// client (so recorders see EOF and reconnect), removes the stream from the
// server map and stops the WebRTC bridge (peer connection, MQTT, forwarder).
// Safe to call more than once and from any goroutine not holding cs.mutex.
func (cs *CameraStream) teardown() {
	cs.mutex.Lock()
	cs.dead = true
	cs.active = false
	cs.starting = false
	cs.connecting = false
	if cs.shutdownTimer != nil {
		cs.shutdownTimer.Stop()
		cs.shutdownTimer = nil
	}
	clients := make([]*RTSPClient, 0, len(cs.clients))
	for sessionID, client := range cs.clients {
		clients = append(clients, client)
		cs.removeClientLocked(sessionID, false)
	}
	cs.mutex.Unlock()

	if cs.server != nil {
		cs.server.removeStream(cs.streamId, cs)
	}

	for _, client := range clients {
		if client.conn != nil {
			_ = client.conn.Close()
		}
	}

	if cs.webrtcBridge != nil {
		cs.webrtcBridge.Stop()
	}
}

// StreamHealth is a point-in-time view of one stream for the health endpoint.
type StreamHealth struct {
	Device      string
	DeviceID    string
	RTSPPath    string
	Resolution  string
	State       string // starting | active | stopped
	Clients     int
	StartedAt   time.Time
	LastMediaAt time.Time // last video RTP packet (zero: none yet)
	LastAudioAt time.Time
}

// StreamsHealth returns the state of every stream currently known to the
// server (streams exist while RTSP clients are attached or shortly after).
func (s *RTSPServer) StreamsHealth() []StreamHealth {
	s.mutex.RLock()
	streams := make([]*CameraStream, 0, len(s.streams))
	for _, stream := range s.streams {
		streams = append(streams, stream)
	}
	s.mutex.RUnlock()

	result := make([]StreamHealth, 0, len(streams))
	for _, stream := range streams {
		stream.mutex.RLock()
		health := StreamHealth{
			Resolution:  stream.resolution,
			Clients:     len(stream.clients),
			StartedAt:   stream.startedAt,
			LastMediaAt: stream.lastMediaLocked(),
		}
		if stream.camera != nil {
			health.Device = stream.camera.DeviceName
			health.DeviceID = stream.camera.DeviceID
			health.RTSPPath = stream.camera.RTSPPath
		}
		if stream.webrtcBridge != nil && stream.webrtcBridge.rtpForwarder != nil {
			health.LastAudioAt = stream.webrtcBridge.rtpForwarder.LastAudioAt()
		}
		switch {
		case stream.dead:
			health.State = "stopped"
		case stream.active:
			health.State = "active"
		case stream.starting || stream.connecting:
			health.State = "starting"
		default:
			health.State = "stopped"
		}
		stream.mutex.RUnlock()
		result = append(result, health)
	}

	sort.Slice(result, func(i, j int) bool {
		if result[i].RTSPPath != result[j].RTSPPath {
			return result[i].RTSPPath < result[j].RTSPPath
		}
		return result[i].Resolution < result[j].Resolution
	})
	return result
}

// MediaTimeout returns the configured watchdog timeout (0: disabled).
func (s *RTSPServer) MediaTimeout() time.Duration {
	s.mutex.RLock()
	defer s.mutex.RUnlock()
	return s.mediaTimeout
}

// ListenAddr returns the RTSP host:port the server binds to.
func (s *RTSPServer) ListenAddr() string {
	return ListenAddress(s.address, s.port)
}
