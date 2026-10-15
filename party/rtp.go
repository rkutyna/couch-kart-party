package main

import (
	"log"
	"net"
	"sync/atomic"

	"github.com/pion/webrtc/v4"
)

// rtpIngest listens on 127.0.0.1:port and forwards every RTP packet it
// receives into track, counting packets and bytes in ctr. It runs for the life
// of the process, so ffmpeg can be restarted freely.
func rtpIngest(port int, track *webrtc.TrackLocalStaticRTP, label string, stop <-chan struct{}, ctr *slotCounters) error {
	conn, err := net.ListenUDP("udp", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1), Port: port})
	if err != nil {
		return err
	}
	_ = conn.SetReadBuffer(4 << 20)
	go func() {
		<-stop
		_ = conn.Close()
	}()
	go func() {
		buf := make([]byte, 1600)
		var bad, total atomic.Uint64
		for {
			n, _, err := conn.ReadFromUDP(buf)
			if err != nil {
				return
			}
			total.Add(1)
			ctr.rtpPkts.Add(1)
			ctr.rtpBytes.Add(uint64(n))
			if _, err := track.Write(buf[:n]); err != nil {
				// Only malformed packets or a track without bindings land here;
				// both are normal (no viewer yet, or stray bytes on the port).
				ctr.trackErrs.Add(1)
				if bad.Add(1)%500 == 1 {
					log.Printf("[%s] rtp write: %v (bad so far: %d, total: %d)", label, err, bad.Load(), total.Load())
				}
			} else {
				ctr.trackWrites.Add(1)
			}
		}
	}()
	log.Printf("[%s] RTP ingest on 127.0.0.1:%d", label, port)
	return nil
}
