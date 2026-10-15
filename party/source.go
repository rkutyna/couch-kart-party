package main

import (
	"fmt"
	"log"
	"net"
	"sync"
)

// srcKey names a video source: one pad (player) of one Dolphin instance.
type srcKey struct{ inst, pad int }

// sourceRouter forwards the packets of every instance source to the slots that
// are currently assigned to it (slot.cfg.Instance / slot.cfg.Pad). The table is
// rebuilt by Server.rebuildRoutesLocked whenever an assignment changes.
type sourceRouter struct {
	mu   sync.RWMutex
	subs map[srcKey][]*slot
}

func (r *sourceRouter) set(subs map[srcKey][]*slot) {
	r.mu.Lock()
	r.subs = subs
	r.mu.Unlock()
}

func (r *sourceRouter) slotsFor(k srcKey) []*slot {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return r.subs[k]
}

// rebuildRoutesLocked recomputes which slot gets which source: a slot with an
// instance and a pad receives that source. Caller holds s.mu.
func (s *Server) rebuildRoutesLocked() {
	subs := map[srcKey][]*slot{}
	for _, sl := range s.slots {
		if sl.cfg.Instance > 0 && sl.cfg.Pad > 0 {
			k := srcKey{sl.cfg.Instance, sl.cfg.Pad}
			subs[k] = append(subs[k], sl)
		}
	}
	s.router.set(subs)
}

// sourceIngest listens on 127.0.0.1:port for one instance source and writes
// every packet to the tracks of the slots routed to it. It runs for the life
// of the process, so the capture pipeline can restart freely.
func (s *Server) sourceIngest(src SourceConfig, stop <-chan struct{}) error {
	conn, err := net.ListenUDP("udp", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1), Port: src.Port})
	if err != nil {
		return err
	}
	_ = conn.SetReadBuffer(4 << 20)
	go func() {
		<-stop
		_ = conn.Close()
	}()
	key := srcKey{src.Instance, src.Pad}
	label := fmt.Sprintf("i%d/p%d", src.Instance, src.Pad)
	go func() {
		buf := make([]byte, 1600)
		var bad uint64
		for {
			n, _, err := conn.ReadFromUDP(buf)
			if err != nil {
				return
			}
			for _, sl := range s.router.slotsFor(key) {
				sl.ctr.rtpPkts.Add(1)
				sl.ctr.rtpBytes.Add(uint64(n))
				if _, err := sl.track.Write(buf[:n]); err != nil {
					sl.ctr.trackErrs.Add(1)
					if bad++; bad%500 == 1 {
						log.Printf("[%s -> slot %d] rtp write: %v (bad so far: %d)", label, sl.cfg.Slot, err, bad)
					}
				} else {
					sl.ctr.trackWrites.Add(1)
				}
			}
		}
	}()
	return nil
}
