package main

import (
	"errors"
	"fmt"
	"log"
	"net"
	"time"

	"github.com/pion/ice/v4"
	"github.com/pion/interceptor"
	"github.com/pion/rtcp"
	"github.com/pion/webrtc/v4"
)

const h264Fmtp = "level-asymmetry-allowed=1;packetization-mode=1;profile-level-id=42e01f"

// ICE timing for newAPI. A phone that stops answering goes disconnected after
// iceDisconnectedTimeout (its slot shows connected=false but stays reserved)
// and failed after iceFailedTimeout (the slot is freed after -free-delay).
const (
	iceDisconnectedTimeout = 3 * time.Second
	iceFailedTimeout       = 6 * time.Second
	iceKeepaliveInterval   = 1 * time.Second
)

// newTrack creates the per-slot H.264 track. It lives as long as the process.
func newTrack(slot int) (*webrtc.TrackLocalStaticRTP, error) {
	return webrtc.NewTrackLocalStaticRTP(
		webrtc.RTPCodecCapability{
			MimeType:    webrtc.MimeTypeH264,
			ClockRate:   90000,
			SDPFmtpLine: h264Fmtp,
		},
		fmt.Sprintf("video-slot%d", slot),
		fmt.Sprintf("slot%d", slot),
	)
}

// keepCandidateIP drops addresses phones cannot reach: the 192.0.0.2 CLAT
// address and link-local addresses. Loopback is kept on purpose so a browser
// on this machine can test.
var clatAddr = net.IPv4(192, 0, 0, 2)

func keepCandidateIP(ip net.IP) bool {
	if ip.Equal(clatAddr) {
		return false
	}
	if ip.IsLinkLocalUnicast() {
		return false
	}
	return true
}

// newAPI builds the shared Pion API: default codecs and interceptors, UDP4/UDP6
// host candidates only (no STUN/TURN), and mDNS in query-only mode.
//
// mDNS: the server never generates mDNS candidates for itself. It is set to
// QueryOnly rather than Disabled because phones (Safari/Chrome) advertise their
// host candidates as *.local names, and Pion silently drops remote .local
// candidates when mDNS is Disabled.
func newAPI() (*webrtc.API, error) {
	m := &webrtc.MediaEngine{}
	if err := m.RegisterDefaultCodecs(); err != nil {
		return nil, err
	}
	ir := &interceptor.Registry{}
	if err := webrtc.RegisterDefaultInterceptors(m, ir); err != nil {
		return nil, err
	}
	se := webrtc.SettingEngine{}
	se.SetICEMulticastDNSMode(ice.MulticastDNSModeQueryOnly)
	se.SetNetworkTypes([]webrtc.NetworkType{webrtc.NetworkTypeUDP4, webrtc.NetworkTypeUDP6})
	se.SetIncludeLoopbackCandidate(true)
	se.SetIPFilter(keepCandidateIP)
	// Pion's defaults (5 s disconnected, 25 s failed, 2 s keepalive) are slow
	// to notice a phone that vanished without closing. With these, a killed
	// phone shows as disconnected in about 3 s and failed in about 6 s.
	se.SetICETimeouts(iceDisconnectedTimeout, iceFailedTimeout, iceKeepaliveInterval)
	return webrtc.NewAPI(
		webrtc.WithMediaEngine(m),
		webrtc.WithInterceptorRegistry(ir),
		webrtc.WithSettingEngine(se),
	), nil
}

// answerOffer applies the remote offer, attaches track, and returns the
// answer SDP after ICE gathering completes (non-trickle). ctr receives the
// RTCP counts for a player; viewers pass nil.
func answerOffer(api *webrtc.API, offer string, track *webrtc.TrackLocalStaticRTP, ctr *slotCounters) (*webrtc.PeerConnection, string, error) {
	pc, err := api.NewPeerConnection(webrtc.Configuration{})
	if err != nil {
		return nil, "", err
	}
	if err := pc.SetRemoteDescription(webrtc.SessionDescription{Type: webrtc.SDPTypeOffer, SDP: offer}); err != nil {
		_ = pc.Close()
		return nil, "", fmt.Errorf("bad offer: %w", err)
	}
	sender, err := pc.AddTrack(track)
	if err != nil {
		_ = pc.Close()
		return nil, "", err
	}
	go drainRTCP(sender, track.ID(), ctr)
	answer, err := pc.CreateAnswer(nil)
	if err != nil {
		_ = pc.Close()
		return nil, "", err
	}
	gathered := webrtc.GatheringCompletePromise(pc)
	if err := pc.SetLocalDescription(answer); err != nil {
		_ = pc.Close()
		return nil, "", err
	}
	select {
	case <-gathered:
	case <-time.After(5 * time.Second):
		log.Printf("ICE gathering still running after 5s; answering with what we have")
	}
	ld := pc.LocalDescription()
	if ld == nil {
		_ = pc.Close()
		return nil, "", errors.New("no local description")
	}
	return pc, ld.SDP, nil
}

// drainRTCP must run for every RTPSender so Pion's RTCP path never stalls. It
// counts picture-loss requests and NACKs from the phone (into ctr when set).
// We cannot forward those to ffmpeg, so the sender needs a short keyframe
// interval (-g 30) instead.
func drainRTCP(sender *webrtc.RTPSender, label string, ctr *slotCounters) {
	buf := make([]byte, 1500)
	var plis uint64
	for {
		n, _, err := sender.Read(buf)
		if err != nil {
			return
		}
		pkts, err := rtcp.Unmarshal(buf[:n])
		if err != nil {
			continue
		}
		for _, p := range pkts {
			switch p.(type) {
			case *rtcp.PictureLossIndication:
				plis++
				if ctr != nil {
					ctr.pli.Add(1)
				}
			case *rtcp.FullIntraRequest:
				plis++
				if ctr != nil {
					ctr.fir.Add(1)
				}
			case *rtcp.TransportLayerNack:
				if ctr != nil {
					ctr.nack.Add(1)
				}
			default:
				continue
			}
			if plis%50 == 1 {
				log.Printf("[%s] phone requested a keyframe (%d so far); ffmpeg cannot be asked, relying on -g", label, plis)
			}
		}
	}
}
