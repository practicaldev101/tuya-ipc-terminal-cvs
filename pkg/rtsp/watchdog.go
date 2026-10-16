package rtsp

import (
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
