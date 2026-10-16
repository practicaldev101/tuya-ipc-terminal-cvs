package rtsp

import (
	"errors"
	"fmt"
	"net"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"
	"tuya-ipc-terminal/pkg/core"
	"tuya-ipc-terminal/pkg/utils"

	"github.com/pion/rtp"
)

// RTP transport mode (UDP or TCP)
type TransportMode int

const (
	TransportUDP TransportMode = iota
	TransportTCP               // Interleaved
)

// tcpWriteTimeout bounds one interleaved RTP write to a TCP client (CVS
// patch). A client that stops reading fills its socket buffer; without a
// deadline the write blocks indefinitely while holding rf.mutex (read
// lock), which in turn blocks RemoveClient and therefore stream teardown.
// A write that times out drops that client.
const tcpWriteTimeout = 5 * time.Second

type RTPForwarder struct {
	clients map[string]*RTPClient
	mutex   sync.RWMutex

	// tcpWriteTimeout is the per-write deadline for TCP interleaved
	// clients (tcpWriteTimeout; shortened in tests, <= 0 disables).
	tcpWriteTimeout time.Duration

	// RTP session info
	videoSSRC atomic.Uint32
	audioSSRC atomic.Uint32

	// Packet count
	firstVideoPacket atomic.Bool
	firstAudioPacket atomic.Bool

	// Wall-clock time (UnixNano) of the last media packet received from the
	// camera, whether or not any RTSP client is attached. Used by the media
	// watchdog and the health endpoint.
	lastVideoAt atomic.Int64
	lastAudioAt atomic.Int64

	// Cached H.264 parameter sets from the live stream. Clients that join
	// mid-GOP need these (via sprop-parameter-sets in the SDP) to initialize
	// their decoder; otherwise they log "non-existing PPS" until the next IDR.
	spsNAL     []byte
	ppsNAL     []byte
	paramMutex sync.RWMutex

	OnBackchannelAudio func(*rtp.Packet)
}

type RTPClient struct {
	sessionID     string
	transportMode TransportMode

	// UDP transport - Outgoing connections (server -> client)
	videoConn *net.UDPConn // For sending video to client
	audioConn *net.UDPConn // For sending audio to client

	// UDP transport - Client addresses
	videoAddr *net.UDPAddr
	audioAddr *net.UDPAddr

	// UDP transport - Client ports
	videoRTPPort          int // Client's video receiving port
	audioRTPPort          int // Client's audio receiving port
	backchannelClientPort int // Client's backchannel sending port

	// UDP backchannel listeners (server side)
	backchannelListener     *net.UDPConn // Server's RTP listener for backchannel
	backchannelRTCPListener *net.UDPConn // Server's RTCP listener for backchannel
	backchannelServerPort   int          // Server's RTP listening port
	backchannelRTCPPort     int          // Server's RTCP listening port

	// TCP interleaved transport
	tcpConn             net.Conn
	videoRTPChannel     byte
	audioRTPChannel     byte
	backAudioRTPChannel byte
	// tcpWriteMutex serializes forwarder writes (video and audio arrive on
	// different goroutines) so each write's deadline is set and cleared
	// around that write only.
	tcpWriteMutex sync.Mutex
	// dropped is set once a TCP write failed or timed out: the connection
	// has been closed (its interleaved framing may be broken) and the RTSP
	// handler removes the client; until then packets are skipped.
	dropped atomic.Bool

	lastActivity atomic.Int64
}

func NewRTPForwarder() *RTPForwarder {
	forwarder := &RTPForwarder{clients: make(map[string]*RTPClient), tcpWriteTimeout: tcpWriteTimeout}
	forwarder.audioSSRC.Store(1)
	forwarder.firstVideoPacket.Store(true)
	forwarder.firstAudioPacket.Store(true)
	return forwarder
}

func (rf *RTPForwarder) AddUDPClient(sessionID, clientHost string, videoRTPPort, audioRTPPort int) error {
	rf.mutex.Lock()
	defer rf.mutex.Unlock()

	client, exists := rf.clients[sessionID]
	if !exists {
		client = &RTPClient{
			sessionID:     sessionID,
			transportMode: TransportUDP,
		}
	}

	if videoRTPPort > 0 && client.videoConn == nil {
		videoConn, videoAddr, err := dialUDPClient(clientHost, videoRTPPort)
		if err != nil {
			return fmt.Errorf("failed to create video UDP connection: %w", err)
		}
		client.videoAddr = videoAddr
		client.videoConn = videoConn
	}

	if audioRTPPort > 0 && client.audioConn == nil {
		audioConn, audioAddr, err := dialUDPClient(clientHost, audioRTPPort)
		if err != nil {
			if !exists && client.videoConn != nil {
				client.videoConn.Close()
			}
			return fmt.Errorf("failed to create audio UDP connection: %w", err)
		}

		client.audioAddr = audioAddr
		client.audioConn = audioConn
	}

	client.videoRTPPort = videoRTPPort
	client.audioRTPPort = audioRTPPort
	client.lastActivity.Store(time.Now().UnixNano())
	rf.clients[sessionID] = client

	core.Logger.Trace().Msgf("Added UDP RTP client %s at %s (video port:%d, audio port:%d)",
		sessionID, clientHost, videoRTPPort, audioRTPPort)
	return nil
}

func dialUDPClient(host string, port int) (*net.UDPConn, *net.UDPAddr, error) {
	address := net.JoinHostPort(host, strconv.Itoa(port))
	udpAddr, err := net.ResolveUDPAddr("udp", address)
	if err != nil {
		return nil, nil, fmt.Errorf("failed to resolve UDP address %s: %w", address, err)
	}
	conn, err := net.DialUDP("udp", nil, udpAddr)
	if err != nil {
		return nil, nil, fmt.Errorf("failed to dial UDP address %s: %w", address, err)
	}
	return conn, udpAddr, nil
}

func (rf *RTPForwarder) SetupUDPBackchannel(sessionID string, clientPort int) (int, error) {
	rf.mutex.Lock()
	defer rf.mutex.Unlock()

	client, exists := rf.clients[sessionID]
	if !exists {
		return 0, fmt.Errorf("client %s not found", sessionID)
	}

	if client.transportMode != TransportUDP {
		return 0, fmt.Errorf("client %s is not using UDP transport", sessionID)
	}

	// Store client's backchannel port
	client.backchannelClientPort = clientPort

	// If listeners already exist, return existing port
	if client.backchannelListener != nil {
		return client.backchannelServerPort, nil
	}

	// Allocate consecutive ports for RTP/RTCP
	portPair, err := utils.DefaultPortAllocator.GetConsecutiveUDPPorts(nil, 10)
	if err != nil {
		return 0, fmt.Errorf("failed to allocate UDP ports for backchannel: %v", err)
	}

	// Store listeners and ports
	client.backchannelListener = portPair.RTPListener
	client.backchannelRTCPListener = portPair.RTCPListener
	client.backchannelServerPort = portPair.RTPPort
	client.backchannelRTCPPort = portPair.RTCPPort

	// Start goroutines to handle incoming packets
	go rf.handleUDPBackchannelRTP(sessionID, client.backchannelListener)
	go rf.handleUDPBackchannelRTCP(client.backchannelRTCPListener)

	core.Logger.Trace().Msgf("Setup UDP backchannel for client %s (client ports:%d-%d, server ports:%d-%d)",
		sessionID, clientPort, clientPort+1, portPair.RTPPort, portPair.RTCPPort)

	return portPair.RTPPort, nil
}

func (rf *RTPForwarder) AddTCPClient(sessionID string, conn net.Conn, videoRTPChannel, audioRTPChannel, backAudioRTPChannel byte) error {
	rf.mutex.Lock()
	defer rf.mutex.Unlock()

	// Check if client already exists, update it
	if existingClient, exists := rf.clients[sessionID]; exists {
		core.Logger.Trace().Msgf("TCP client %s already exists, updating channels (video:%d->%d, audio:%d->%d, back:%d->%d)",
			sessionID, existingClient.videoRTPChannel, videoRTPChannel, existingClient.audioRTPChannel, audioRTPChannel, existingClient.backAudioRTPChannel, backAudioRTPChannel)
		existingClient.videoRTPChannel = videoRTPChannel
		existingClient.audioRTPChannel = audioRTPChannel
		existingClient.backAudioRTPChannel = backAudioRTPChannel
		existingClient.lastActivity.Store(time.Now().UnixNano())
		return nil
	}

	client := &RTPClient{
		sessionID:           sessionID,
		transportMode:       TransportTCP,
		tcpConn:             conn,
		videoRTPChannel:     videoRTPChannel,
		audioRTPChannel:     audioRTPChannel,
		backAudioRTPChannel: backAudioRTPChannel,
	}
	client.lastActivity.Store(time.Now().UnixNano())

	rf.clients[sessionID] = client

	core.Logger.Trace().Msgf("Added TCP RTP client %s (video channel:%d, audio channel:%d, back audio channel:%d)",
		sessionID, videoRTPChannel, audioRTPChannel, backAudioRTPChannel)
	return nil
}

func (rf *RTPForwarder) RemoveClient(sessionID string) {
	rf.mutex.Lock()
	defer rf.mutex.Unlock()

	if client, exists := rf.clients[sessionID]; exists {
		if client.transportMode == TransportUDP {
			if client.videoConn != nil {
				client.videoConn.Close()
			}
			if client.audioConn != nil {
				client.audioConn.Close()
			}
			if client.backchannelListener != nil {
				client.backchannelListener.Close()
			}
			if client.backchannelRTCPListener != nil {
				client.backchannelRTCPListener.Close()
			}
		}

		delete(rf.clients, sessionID)
		core.Logger.Trace().Msgf("Removed RTP client %s", sessionID)
	}
}

// cacheParameterSets extracts SPS/PPS NAL units from an H.264 RTP payload
// (single NAL or STAP-A aggregation) and caches them for SDP generation.
func (rf *RTPForwarder) cacheParameterSets(payload []byte) {
	if len(payload) < 1 {
		return
	}

	cacheNAL := func(nal []byte) {
		if len(nal) < 1 {
			return
		}
		switch nal[0] & 0x1F {
		case 7: // SPS
			rf.paramMutex.Lock()
			rf.spsNAL = append([]byte(nil), nal...)
			rf.paramMutex.Unlock()
		case 8: // PPS
			rf.paramMutex.Lock()
			rf.ppsNAL = append([]byte(nil), nal...)
			rf.paramMutex.Unlock()
		}
	}

	switch payload[0] & 0x1F {
	case 7, 8:
		cacheNAL(payload)
	case 24: // STAP-A: [header][size][NAL][size][NAL]...
		i := 1
		for i+2 <= len(payload) {
			size := int(payload[i])<<8 | int(payload[i+1])
			i += 2
			if size <= 0 || i+size > len(payload) {
				break
			}
			cacheNAL(payload[i : i+size])
			i += size
		}
	}
}

// GetParameterSets returns the cached SPS and PPS NAL units (nil if not seen yet).
func (rf *RTPForwarder) GetParameterSets() ([]byte, []byte) {
	rf.paramMutex.RLock()
	defer rf.paramMutex.RUnlock()
	return rf.spsNAL, rf.ppsNAL
}

func (rf *RTPForwarder) ForwardVideoPacket(packet *rtp.Packet) {
	// Cache SPS/PPS even when nobody is connected yet, so the SDP for the
	// next client can carry sprop-parameter-sets.
	rf.cacheParameterSets(packet.Payload)
	rf.lastVideoAt.Store(time.Now().UnixNano())

	rf.mutex.RLock()
	defer rf.mutex.RUnlock()

	if len(rf.clients) == 0 {
		return
	}

	// The RTSP SDP advertises video as payload type 96 for both H.264 and H.265.
	// Tuya's HEVC data channel uses PT 95, which RTSP clients discard as unknown.
	forwardedPacket := *packet
	forwardedPacket.PayloadType = 96

	data, err := forwardedPacket.Marshal()
	if err != nil {
		core.Logger.Error().Err(err).Msg("Error marshaling video RTP packet")
		return
	}

	// Forward to all clients
	for sessionID, client := range rf.clients {
		client.lastActivity.Store(time.Now().UnixNano())

		if client.transportMode == TransportUDP {
			if client.videoConn != nil {
				if _, err := client.videoConn.Write(data); err != nil {
					core.Logger.Error().Err(err).Msgf("Error forwarding video packet to UDP client %s", sessionID)
				} else if rf.firstVideoPacket.CompareAndSwap(true, false) {
					core.Logger.Trace().Msgf("Successfully sent first video RTP packet to UDP client %s on port %d (PT %d->96, SSRC %d, sequence %d, timestamp %d, %d bytes)",
						sessionID, client.videoRTPPort, packet.PayloadType, packet.SSRC, packet.SequenceNumber, packet.Timestamp, len(data))
				}
			}
		} else if client.transportMode == TransportTCP {
			if client.tcpConn != nil && !client.dropped.Load() {
				if err := rf.writeInterleaved(client, client.videoRTPChannel, data); err == nil && rf.firstVideoPacket.CompareAndSwap(true, false) {
					core.Logger.Trace().Msgf("Successfully sent first video RTP packet to TCP client %s on channel %d (PT %d->96, SSRC %d, sequence %d, timestamp %d, %d bytes)",
						sessionID, client.videoRTPChannel, packet.PayloadType, packet.SSRC, packet.SequenceNumber, packet.Timestamp, len(data))
				}
			}
		}
	}
}

func (rf *RTPForwarder) ForwardAudioPacket(packet *rtp.Packet) {
	rf.lastAudioAt.Store(time.Now().UnixNano())

	rf.mutex.RLock()
	defer rf.mutex.RUnlock()

	if len(rf.clients) == 0 {
		return
	}

	// Serialize packet
	data, err := packet.Marshal()
	if err != nil {
		core.Logger.Error().Err(err).Msg("Error marshaling audio RTP packet")
		return
	}

	// Forward to all clients
	for sessionID, client := range rf.clients {
		client.lastActivity.Store(time.Now().UnixNano())

		if client.transportMode == TransportUDP {
			if client.audioConn != nil {
				if _, err := client.audioConn.Write(data); err != nil {
					core.Logger.Error().Err(err).Msgf("Error forwarding audio packet to UDP client %s", sessionID)
				} else if rf.firstAudioPacket.CompareAndSwap(true, false) {
					core.Logger.Trace().Msgf("Successfully sent first audio packet to UDP client %s on port %d",
						sessionID, client.audioRTPPort)
				}
			}
		} else if client.transportMode == TransportTCP {
			if client.tcpConn != nil && !client.dropped.Load() {
				if err := rf.writeInterleaved(client, client.audioRTPChannel, data); err == nil && rf.firstAudioPacket.CompareAndSwap(true, false) {
					core.Logger.Trace().Msgf("Successfully sent first audio packet to TCP client %s on channel %d",
						sessionID, client.audioRTPChannel)
				}
			}
		}
	}
}

func (rf *RTPForwarder) Stop() {
	// Reset SSRCs
	rf.videoSSRC.Store(0)
	rf.audioSSRC.Store(1)

	// Reset first packet flags
	rf.firstVideoPacket.Store(true)
	rf.firstAudioPacket.Store(true)

	// Clear all clients (snapshot under the lock: RemoveClient locks too)
	rf.mutex.RLock()
	sessionIDs := make([]string, 0, len(rf.clients))
	for sessionID := range rf.clients {
		sessionIDs = append(sessionIDs, sessionID)
	}
	rf.mutex.RUnlock()
	for _, sessionID := range sessionIDs {
		rf.RemoveClient(sessionID)
	}

	core.Logger.Trace().Msg("RTPForwarder stopped and all clients cleared")
}

// LastVideoAt returns when the last video RTP packet arrived from the camera
// (zero if none yet).
func (rf *RTPForwarder) LastVideoAt() time.Time {
	return unixNanoTime(rf.lastVideoAt.Load())
}

// LastAudioAt returns when the last audio RTP packet arrived from the camera
// (zero if none yet).
func (rf *RTPForwarder) LastAudioAt() time.Time {
	return unixNanoTime(rf.lastAudioAt.Load())
}

func unixNanoTime(ns int64) time.Time {
	if ns == 0 {
		return time.Time{}
	}
	return time.Unix(0, ns)
}

func (rf *RTPForwarder) GetClientCount() int {
	rf.mutex.RLock()
	defer rf.mutex.RUnlock()
	return len(rf.clients)
}

func (rf *RTPForwarder) CleanupInactiveClients(timeout time.Duration) {
	rf.mutex.Lock()
	defer rf.mutex.Unlock()

	now := time.Now()
	var toRemove []string

	for sessionID, client := range rf.clients {
		if now.Sub(time.Unix(0, client.lastActivity.Load())) > timeout {
			toRemove = append(toRemove, sessionID)
		}
	}

	for _, sessionID := range toRemove {
		if client, exists := rf.clients[sessionID]; exists {
			if client.transportMode == TransportUDP {
				if client.videoConn != nil {
					client.videoConn.Close()
				}
				if client.audioConn != nil {
					client.audioConn.Close()
				}
				if client.backchannelListener != nil {
					client.backchannelListener.Close()
				}
				if client.backchannelRTCPListener != nil {
					client.backchannelRTCPListener.Close()
				}
			}
			delete(rf.clients, sessionID)
			core.Logger.Trace().Msgf("Cleaned up inactive RTP client %s", sessionID)
		}
	}
}

func (rf *RTPForwarder) handleUDPBackchannelRTP(sessionID string, listener *net.UDPConn) {
	defer listener.Close()

	buffer := make([]byte, 1500)

	for {
		n, _, err := listener.ReadFromUDP(buffer)
		if err != nil {
			if !strings.Contains(err.Error(), "closed") {
				core.Logger.Error().Err(err).Msgf("Error reading UDP RTP backchannel for client %s", sessionID)
			}
			break
		}

		// Parse RTP packet
		packet := &rtp.Packet{}
		if err := packet.Unmarshal(buffer[:n]); err != nil {
			continue
		}

		// Forward to WebRTC bridge
		if rf.OnBackchannelAudio != nil {
			rf.OnBackchannelAudio(packet)
		}
	}
}

func (rf *RTPForwarder) handleUDPBackchannelRTCP(listener *net.UDPConn) {
	defer listener.Close()

	buffer := make([]byte, 1500)

	for {
		_, _, err := listener.ReadFromUDP(buffer)
		if err != nil {
			// Ignore
			break
		}

		// Simply discard RTCP packets
	}
}

// writeInterleaved sends one interleaved RTP packet to a TCP client with a
// write deadline. On any write error (timeout included) the client is
// dropped: its connection is closed, which also ends its RTSP session (the
// handler then removes it from the stream and this forwarder). Safe to call
// while holding rf.mutex for reading.
func (rf *RTPForwarder) writeInterleaved(client *RTPClient, channel byte, rtpData []byte) error {
	client.tcpWriteMutex.Lock()
	defer client.tcpWriteMutex.Unlock()

	if client.dropped.Load() {
		return net.ErrClosed
	}
	if rf.tcpWriteTimeout > 0 {
		_ = client.tcpConn.SetWriteDeadline(time.Now().Add(rf.tcpWriteTimeout))
	}
	err := rf.sendInterleavedRTP(client.tcpConn, channel, rtpData)
	if rf.tcpWriteTimeout > 0 && err == nil {
		// Do not leave a stale deadline behind for RTSP responses written
		// later on the same connection by the protocol handler.
		_ = client.tcpConn.SetWriteDeadline(time.Time{})
	}
	if err != nil && client.dropped.CompareAndSwap(false, true) {
		event := core.Logger.Error()
		reason := "write failed"
		if netErr := net.Error(nil); errors.As(err, &netErr) && netErr.Timeout() {
			event = core.Logger.Warn()
			reason = fmt.Sprintf("not reading (write blocked for %v)", rf.tcpWriteTimeout)
		}
		event.Err(err).Str("component", "forwarder").Str("session", client.sessionID).
			Msgf("Dropping TCP RTP client %s: %s", client.sessionID, reason)
		_ = client.tcpConn.Close()
	}
	return err
}

func (rf *RTPForwarder) sendInterleavedRTP(conn net.Conn, channel byte, rtpData []byte) error {
	// Interleaved format: $ + channel + length(2 bytes) + RTP data
	header := make([]byte, 4)
	header[0] = '$'                     // Magic byte
	header[1] = channel                 // Channel number
	header[2] = byte(len(rtpData) >> 8) // Length high byte
	header[3] = byte(len(rtpData))      // Length low byte

	// Send header + data in one write to avoid fragmentation
	fullPacket := append(header, rtpData...)

	if _, err := conn.Write(fullPacket); err != nil {
		return err
	}

	return nil
}
