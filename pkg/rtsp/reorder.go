package rtsp

import "github.com/pion/rtp"

// videoReorderDepth is how many video RTP packets may sit ahead of a hole
// before the hole is skipped. The CORN stream inverts adjacent packets
// (N+1 arrives before N) inside FU-A fragments. TCP clients assemble those
// fragments in arrival order, so the inversion becomes a corrupt slice.
// A depth of a few packets reorders those swaps and still moves on when a
// packet is actually missing, instead of stalling the read loop.
const videoReorderDepth = 8

// videoReorder releases video RTP packets in sequence-number order.
type videoReorder struct {
	next  uint16
	ready bool
	buf   map[uint16]*rtp.Packet
}

func (r *videoReorder) reset() {
	r.next = 0
	r.ready = false
	r.buf = nil
}

// push stores p and returns the packets that can now be sent, in order.
// p is retained, so the caller must pass a packet it owns.
func (r *videoReorder) push(p *rtp.Packet) []*rtp.Packet {
	if r.buf == nil {
		r.buf = make(map[uint16]*rtp.Packet)
	}
	seq := p.SequenceNumber
	if !r.ready {
		r.ready = true
		r.next = seq
	} else if seq != r.next && seqBefore(seq, r.next) {
		return nil
	}
	if _, exists := r.buf[seq]; !exists {
		r.buf[seq] = p
	}
	return r.drain()
}

func (r *videoReorder) drain() []*rtp.Packet {
	var out []*rtp.Packet
	for {
		for {
			p, ok := r.buf[r.next]
			if !ok {
				break
			}
			delete(r.buf, r.next)
			out = append(out, p)
			r.next++
		}
		if len(r.buf) < videoReorderDepth || !r.skipHole() {
			return out
		}
	}
}

// skipHole advances next to the nearest buffered packet ahead of the hole.
func (r *videoReorder) skipHole() bool {
	var best uint16
	var bestDist uint16
	found := false
	for seq := range r.buf {
		dist := seq - r.next
		if dist == 0 || dist >= 0x8000 {
			continue
		}
		if !found || dist < bestDist {
			found = true
			best = seq
			bestDist = dist
		}
	}
	if !found {
		return false
	}
	r.next = best
	return true
}

// seqBefore reports whether seq is earlier than next, across the uint16 wrap.
func seqBefore(seq, next uint16) bool {
	if seq == next {
		return false
	}
	return uint16(next-seq) < 0x8000
}
