package rtsp

import (
	"errors"
	"io"
	"net"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/pion/rtp"

	"tuya-ipc-terminal/pkg/storage"
)

// tcpPair returns both ends of a loopback TCP connection with small socket
// buffers so a peer that does not read blocks the writer quickly.
func tcpPair(t *testing.T) (serverSide, peer net.Conn) {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()

	accepted := make(chan net.Conn, 1)
	go func() {
		conn, err := ln.Accept()
		if err != nil {
			close(accepted)
			return
		}
		accepted <- conn
	}()
	peer, err = net.Dial("tcp", ln.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	serverSide, ok := <-accepted
	if !ok {
		t.Fatal("accept failed")
	}
	_ = serverSide.(*net.TCPConn).SetWriteBuffer(4096)
	_ = peer.(*net.TCPConn).SetReadBuffer(4096)
	t.Cleanup(func() {
		_ = serverSide.Close()
		_ = peer.Close()
	})
	return serverSide, peer
}

// pump forwards video packets until stop is closed. It returns a counter of
// completed ForwardVideoPacket calls and a channel closed when it exits.
func pump(forwarder *RTPForwarder, stop <-chan struct{}) (*atomic.Int64, <-chan struct{}) {
	var count atomic.Int64
	done := make(chan struct{})
	payload := make([]byte, 1400)
	payload[0] = 0x41 // non-IDR slice
	go func() {
		defer close(done)
		for seq := uint16(0); ; seq++ {
			select {
			case <-stop:
				return
			default:
			}
			forwarder.ForwardVideoPacket(&rtp.Packet{Header: rtp.Header{Version: 2, PayloadType: 96, SequenceNumber: seq}, Payload: payload})
			count.Add(1)
		}
	}()
	return &count, done
}

// waitBlocked waits until the pump stops making progress (a write is stuck
// on the non-reading peer).
func waitBlocked(t *testing.T, count *atomic.Int64) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	last := count.Load()
	for time.Now().Before(deadline) {
		time.Sleep(300 * time.Millisecond)
		current := count.Load()
		if current == last {
			return
		}
		last = current
	}
	t.Fatal("forwarder never blocked on the non-reading client")
}

// A recorder that stops reading must not stall teardown (previously up to
// the 60 s RTSP read deadline): teardown closes the client connections
// before taking the forwarder lock.
func TestTeardownWithNonReadingTCPClientIsFast(t *testing.T) {
	s := newWatchdogTestServer(time.Now())
	camera := &storage.CameraInfo{DeviceID: "dev1", DeviceName: "CORN_CHM-302", RTSPPath: "/CORN_CHM-302"}
	stream := NewCameraStream(camera, "sd", nil, nil, s)
	forwarder := stream.webrtcBridge.rtpForwarder
	// Default 5 s write timeout: a teardown that waited for it would fail
	// the 2 s bound below.
	if forwarder.tcpWriteTimeout != tcpWriteTimeout {
		t.Fatalf("forwarder write timeout %v, want %v", forwarder.tcpWriteTimeout, tcpWriteTimeout)
	}

	serverSide, peer := tcpPair(t)
	client := &RTSPClient{conn: serverSide, session: "stuck", stream: stream, transportMode: TransportTCP}
	stream.mutex.Lock()
	stream.active = true
	stream.clients[client.session] = client
	stream.mutex.Unlock()
	s.mutex.Lock()
	s.streams[stream.streamId] = stream
	s.clients[client.session] = client
	s.mutex.Unlock()
	if err := forwarder.AddTCPClient(client.session, serverSide, 0, 2, 4); err != nil {
		t.Fatal(err)
	}

	stop := make(chan struct{})
	count, pumpDone := pump(forwarder, stop)
	defer func() {
		close(stop)
		<-pumpDone
	}()
	waitBlocked(t, count)

	start := time.Now()
	finished := make(chan struct{})
	go func() {
		stream.teardown()
		close(finished)
	}()
	select {
	case <-finished:
	case <-time.After(2 * time.Second):
		t.Fatal("teardown blocked by a non-reading TCP client for more than 2s")
	}
	t.Logf("teardown took %v", time.Since(start))

	if n := forwarder.GetClientCount(); n != 0 {
		t.Fatalf("forwarder still has %d clients", n)
	}
	stream.mutex.RLock()
	remaining := len(stream.clients)
	stream.mutex.RUnlock()
	if remaining != 0 {
		t.Fatalf("stream still has %d clients", remaining)
	}
	// The forwarder keeps running (no client left to block on).
	before := count.Load()
	time.Sleep(100 * time.Millisecond)
	if count.Load() == before {
		t.Fatal("forwarder still blocked after teardown")
	}
	_ = peer
}

// A TCP client that stops reading is dropped after the write timeout
// without blocking other forwarding.
func TestTCPWriteTimeoutDropsNonReadingClient(t *testing.T) {
	forwarder := NewRTPForwarder()
	forwarder.tcpWriteTimeout = 200 * time.Millisecond

	serverSide, peer := tcpPair(t)
	if err := forwarder.AddTCPClient("stuck", serverSide, 0, 2, 4); err != nil {
		t.Fatal(err)
	}
	forwarder.mutex.RLock()
	client := forwarder.clients["stuck"]
	forwarder.mutex.RUnlock()

	stop := make(chan struct{})
	count, pumpDone := pump(forwarder, stop)
	defer func() {
		close(stop)
		<-pumpDone
	}()

	deadline := time.Now().Add(5 * time.Second)
	for !client.dropped.Load() {
		if time.Now().After(deadline) {
			t.Fatal("non-reading TCP client not dropped after the write timeout")
		}
		time.Sleep(10 * time.Millisecond)
	}
	// Dropping closes the connection (so the RTSP handler removes the client).
	if _, err := serverSide.Write([]byte{0}); !errors.Is(err, net.ErrClosed) {
		t.Fatalf("connection of dropped client not closed: %v", err)
	}
	// Forwarding continues without blocking on the dropped client.
	before := count.Load()
	time.Sleep(100 * time.Millisecond)
	if count.Load() == before {
		t.Fatal("forwarder blocked after dropping the client")
	}
	_ = peer
}

// A successful forwarder write must not leave a write deadline behind that
// would make a later RTSP response on the same connection fail.
func TestTCPWriteDeadlineIsClearedAfterWrite(t *testing.T) {
	forwarder := NewRTPForwarder()
	forwarder.tcpWriteTimeout = 50 * time.Millisecond

	serverSide, peer := tcpPair(t)
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		_, _ = io.Copy(io.Discard, peer)
	}()
	if err := forwarder.AddTCPClient("reader", serverSide, 0, 2, 4); err != nil {
		t.Fatal(err)
	}
	forwarder.ForwardVideoPacket(&rtp.Packet{Header: rtp.Header{Version: 2, PayloadType: 96}, Payload: []byte{0x41, 0x00}})

	time.Sleep(150 * time.Millisecond)
	if _, err := serverSide.Write([]byte("RTSP/1.0 200 OK\r\n\r\n")); err != nil {
		t.Fatalf("RTSP response after forwarder write failed: %v", err)
	}
	forwarder.mutex.RLock()
	dropped := forwarder.clients["reader"].dropped.Load()
	forwarder.mutex.RUnlock()
	if dropped {
		t.Fatal("reading client was dropped")
	}
	_ = serverSide.Close()
	wg.Wait()
}
