package rtsp

import (
	"encoding/binary"
	"io"
	"net"
	"testing"
	"time"

	"github.com/pion/rtp"
)

func videoPacket(seq uint16) *rtp.Packet {
	return &rtp.Packet{
		Header: rtp.Header{
			Version:        2,
			PayloadType:    95,
			SequenceNumber: seq,
			Timestamp:      uint32(seq) * 3000,
			SSRC:           7,
			Marker:         true,
		},
		Payload: []byte{0x09, byte(seq), byte(seq >> 8)},
	}
}

func TestVideoReorderHoldsSwapThenReleasesInOrder(t *testing.T) {
	var r videoReorder
	var got []uint16
	for _, seq := range []uint16{1, 2, 3, 5, 4, 6} {
		for _, p := range r.push(videoPacket(seq)) {
			got = append(got, p.SequenceNumber)
		}
	}
	want := []uint16{1, 2, 3, 4, 5, 6}
	if len(got) != len(want) {
		t.Fatalf("got %v", got)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("got %v, want %v", got, want)
		}
	}
}

func TestVideoReorderSkipsARealHole(t *testing.T) {
	var r videoReorder
	var got []uint16
	// 1 establishes next. 2 never arrives. The following depth packets force the skip.
	seqs := []uint16{1}
	for s := uint16(3); s < 3+videoReorderDepth; s++ {
		seqs = append(seqs, s)
	}
	for _, seq := range seqs {
		for _, p := range r.push(videoPacket(seq)) {
			got = append(got, p.SequenceNumber)
		}
	}
	if len(got) != 1+videoReorderDepth {
		t.Fatalf("got %d packets, want %d", len(got), 1+videoReorderDepth)
	}
	if got[0] != 1 || got[1] != 3 {
		t.Fatalf("hole was not skipped: %v", got[:4])
	}
}

func TestVideoReorderKeepsAFortyPacketDelay(t *testing.T) {
	var r videoReorder
	var got []uint16
	push := func(seq uint16) {
		for _, p := range r.push(videoPacket(seq)) {
			got = append(got, p.SequenceNumber)
		}
	}
	push(1)
	for s := uint16(3); s <= 42; s++ {
		push(s)
	}
	if len(got) != 1 {
		t.Fatalf("released early: %v", got)
	}
	push(2)
	if len(got) != 42 || got[1] != 2 || got[41] != 42 {
		t.Fatalf("late packet was not restored: len=%d head=%v", len(got), got[:min(4, len(got))])
	}
}

func TestVideoReorderWrapsSequence(t *testing.T) {
	var r videoReorder
	var got []uint16
	for _, seq := range []uint16{65534, 0, 65535, 1} {
		for _, p := range r.push(videoPacket(seq)) {
			got = append(got, p.SequenceNumber)
		}
	}
	want := []uint16{65534, 65535, 0, 1}
	if len(got) != len(want) {
		t.Fatalf("got %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("got %v, want %v", got, want)
		}
	}
}

func TestForwardVideoPacketWritesSwappedRTPInOrder(t *testing.T) {
	rf := NewRTPForwarder()
	rf.tcpWriteTimeout = time.Second
	server, client := net.Pipe()
	t.Cleanup(func() {
		server.Close()
		client.Close()
	})
	if err := rf.AddTCPClient("s", server, 0, 2, 4); err != nil {
		t.Fatal(err)
	}

	got := make(chan uint16, 8)
	errc := make(chan error, 1)
	go func() {
		for {
			seq, err := readInterleavedSeq(client)
			if err != nil {
				errc <- err
				return
			}
			got <- seq
		}
	}()

	for _, seq := range []uint16{10, 11, 13, 12, 14} {
		rf.ForwardVideoPacket(videoPacket(seq))
	}

	var seqs []uint16
	deadline := time.After(2 * time.Second)
	for len(seqs) < 5 {
		select {
		case seq := <-got:
			seqs = append(seqs, seq)
		case err := <-errc:
			t.Fatal(err)
		case <-deadline:
			t.Fatalf("timed out, got %v", seqs)
		}
	}
	want := []uint16{10, 11, 12, 13, 14}
	for i := range want {
		if seqs[i] != want[i] {
			t.Fatalf("got %v, want %v", seqs, want)
		}
	}
}

func readInterleavedSeq(r io.Reader) (uint16, error) {
	var hdr [4]byte
	if _, err := io.ReadFull(r, hdr[:]); err != nil {
		return 0, err
	}
	if hdr[0] != '$' {
		return 0, io.ErrUnexpectedEOF
	}
	n := int(binary.BigEndian.Uint16(hdr[2:4]))
	payload := make([]byte, n)
	if _, err := io.ReadFull(r, payload); err != nil {
		return 0, err
	}
	var pkt rtp.Packet
	if err := pkt.Unmarshal(payload); err != nil {
		return 0, err
	}
	return pkt.SequenceNumber, nil
}
