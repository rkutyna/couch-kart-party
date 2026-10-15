// Command fakephones joins N simulated phones to a running partyd, so the whole
// lobby -> navigator -> race flow can be tested without real phones.
//
// Each fake phone does what the phone page does: it opens the WebRTC video
// connection (receiving the slot's stream) and an "input" data channel, takes a
// slot, sets its name / character / vehicle / drift / course vote, and readies up.
// The first (lowest) slot is the host and presses start when everyone is ready.
// It then prints every lobby change and keeps the connections open.
//
// After hand-over (phase live and showVideo on the slot) a phone can send input
// (-after-live: "wiggle" holds A and sweeps the stick, so the kart does not sit
// idle and get disconnected; "idle" sends nothing). Input is never sent while
// the room is starting or while the navigator holds the slot (partyd drops it).
//
// usage:
//
//	go run ./cmd/fakephones -base http://127.0.0.1:8000 -config ../nav/fakephones/three.json
//	-config       JSON array, one entry per phone in join order (slot 1, 2, 3 ...):
//	              {"name","character","vehicle","drift","vote"} or, instead of "vote",
//	              "votes": [first race, second race, ...] (cycled; the next one is
//	              posted after every hand-over, i.e. it is the vote for the NEXT race)
//	-start-after  wait this long after everyone is ready before the host presses start
//	-wait-msg     press start only while the lobby message contains this text
//	              (the navigator sets "Ready to start." when every instance is prepared)
//	-rounds       how many times the host presses start (default 1; 0 = no limit)
//	-reset-after  the host resets the lobby this long after the room went live
//	              (0 = never); with -rounds this gives repeated start -> hand-over runs
//	-after-live   drive | wiggle | idle    (default drive: steers on the road using
//	              run/frames/i<slot>.bmp; wiggle: blind stick sweeps; idle: nothing)
//	-duration     how long to keep the phones open (default 15m)
//	-quiet        only print phase changes, messages and hand-overs
//
// The lobby log lines are prefixed with the time and the phone name, so they can
// be lined up against the navigator's log (nav/logs/nav-*.log).
package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"log"
	"math"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/pion/ice/v4"
	"github.com/pion/interceptor"
	"github.com/pion/webrtc/v4"
	"github.com/pion/webrtc/v4/pkg/media/h264writer"
)

var (
	base       = flag.String("base", "http://127.0.0.1:8000", "partyd base URL")
	configPath = flag.String("config", "", "JSON array of phones (required)")
	startAfter = flag.Duration("start-after", 2*time.Second, "host waits this long after all are ready before start")
	afterLive  = flag.String("after-live", "drive", "input after hand-over: drive | wiggle | idle")
	framesFlag = flag.String("frames", "", "directory of the navigator frames (default ../run/frames relative to the binary)")
	duration   = flag.Duration("duration", 15*time.Minute, "how long to keep the phones open")
	quiet      = flag.Bool("quiet", false, "print only phase changes and hand-overs")
	rounds_    = flag.Int("rounds", 1, "how many times the host presses start (0 = every time the lobby is ready)")
	resetAfter = flag.Duration("reset-after", 0, "host resets the lobby this long after the room went live (0 = never)")
	voteAt     = flag.String("vote-at", "picker", "when the next vote is posted: picker (when partyd opens the per-race vote picker) | handover (right after each hand-over; for a partyd without the picker)")
	waitMsg    = flag.String("wait-msg", "", "press start only while the lobby message contains this text (e.g. \"Ready\")")
	dumpDir    = flag.String("dump", "", "after each hand-over, write 4 s of the slot's received video to <dir>/<time>_slot<N>.h264 (check with ffmpeg)")
)

type phoneCfg struct {
	Name      string `json:"name"`
	Character string `json:"character"`
	Vehicle   string `json:"vehicle"`
	Drift     string `json:"drift"`
	Vote      string `json:"vote"`
	// Votes, when given, replaces Vote: the first entry is the vote for the first
	// race, the next one is posted after each hand-over (for the next race), cycling.
	Votes []string `json:"votes,omitempty"`
	Ready *bool    `json:"ready,omitempty"`
	// Per-race vote picker (partyd voteOpen): the phone posts its next vote when the
	// picker opens and locks in LockAfter seconds later (default 3; negative = never
	// locks, so the navigator's deadline applies). DropAtVote n > 0: the phone closes its
	// connection when the n-th picker opens (the navigator must use the last vote).
	LockAfter  *float64 `json:"lockAfter,omitempty"`
	DropAtVote int      `json:"dropAtVote,omitempty"`
	// AfterLive overrides -after-live for this phone (drive | wiggle | idle).
	AfterLive string `json:"afterLive,omitempty"`
}

// lobbyPlayer and lobbyState mirror GET /api/lobby (phone view).
type lobbyPlayer struct {
	Slot       int    `json:"slot"`
	Occupied   bool   `json:"occupied"`
	Connected  bool   `json:"connected"`
	Name       string `json:"name"`
	Character  string `json:"character"`
	Vehicle    string `json:"vehicle"`
	Drift      string `json:"drift"`
	Vote       string `json:"vote"`
	Ready      bool   `json:"ready"`
	ShowVideo  bool   `json:"showVideo"`
	NavControl bool   `json:"navControl"`
	Status     string `json:"status"`
	VoteOpen   bool   `json:"voteOpen"`
	VoteDL     int64  `json:"voteDeadlineMs"`
	VoteLocked bool   `json:"voteLocked"`
}

type lobbyState struct {
	Version  uint64        `json:"version"`
	Phase    string        `json:"phase"`
	Message  string        `json:"message"`
	HostSlot int           `json:"hostSlot"`
	Players  []lobbyPlayer `json:"players"`
	Error    string        `json:"error"`
}

// phone is one fake phone: a clientId, an optional slot, and its connection.
type phone struct {
	cfg      phoneCfg
	id       string
	api      *webrtc.API
	pc       *webrtc.PeerConnection
	dc       *webrtc.DataChannel
	slot     int
	connCh   chan struct{}
	connOnce sync.Once
	rtp      int64
	mu       sync.Mutex
	seq      int64

	sent      int64 // input messages sent (wiggle)
	handovers int   // hand-overs seen since the last lobby reset
	voteOpen  bool  // the picker is open (as last seen)
	votesSeen int   // pickers opened so far
	lockAt    time.Time
	dropped   bool
	voteRound int // the hand-over count the current vote was posted for

	dumpUntil time.Time // while set and in the future, received RTP goes to dump
	dump      *h264writer.H264Writer
}

func httpDo(method, url string, body any, out any) (int, error) {
	var rdr io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return 0, err
		}
		rdr = bytes.NewReader(b)
	}
	req, err := http.NewRequest(method, url, rdr)
	if err != nil {
		return 0, err
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := (&http.Client{Timeout: 60 * time.Second}).Do(req)
	if err != nil {
		return 0, err
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	if out != nil {
		_ = json.Unmarshal(raw, out)
	}
	if resp.StatusCode >= 400 {
		var e struct {
			Error string `json:"error"`
		}
		_ = json.Unmarshal(raw, &e)
		return resp.StatusCode, errors.New(e.Error)
	}
	return resp.StatusCode, nil
}

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
	se.SetICEMulticastDNSMode(ice.MulticastDNSModeDisabled)
	se.SetIncludeLoopbackCandidate(true)
	return webrtc.NewAPI(webrtc.WithMediaEngine(m), webrtc.WithInterceptorRegistry(ir), webrtc.WithSettingEngine(se)), nil
}

// connect performs the phone handshake: a recvonly video offer plus the input
// and ping data channels, the same channels the phone page opens.
func (p *phone) connect(wantSlot int) error {
	pc, err := p.api.NewPeerConnection(webrtc.Configuration{})
	if err != nil {
		return err
	}
	p.pc = pc
	p.connCh = make(chan struct{})
	p.connOnce = sync.Once{}
	if _, err := pc.AddTransceiverFromKind(webrtc.RTPCodecTypeVideo, webrtc.RTPTransceiverInit{Direction: webrtc.RTPTransceiverDirectionRecvonly}); err != nil {
		return err
	}
	pc.OnTrack(func(t *webrtc.TrackRemote, _ *webrtc.RTPReceiver) {
		for {
			pkt, _, err := t.ReadRTP()
			if err != nil {
				return
			}
			p.mu.Lock()
			p.rtp++
			if p.dump != nil {
				if time.Now().Before(p.dumpUntil) {
					_ = p.dump.WriteRTP(pkt)
				} else {
					_ = p.dump.Close()
					p.dump = nil
				}
			}
			p.mu.Unlock()
		}
	})
	pc.OnConnectionStateChange(func(s webrtc.PeerConnectionState) {
		if s == webrtc.PeerConnectionStateConnected {
			p.connOnce.Do(func() { close(p.connCh) })
		}
	})
	f := false
	var zero uint16
	dc, err := pc.CreateDataChannel("input", &webrtc.DataChannelInit{Ordered: &f, MaxRetransmits: &zero})
	if err != nil {
		return err
	}
	p.dc = dc
	pdc, err := pc.CreateDataChannel("ping", nil)
	if err != nil {
		return err
	}
	pdc.OnMessage(func(m webrtc.DataChannelMessage) {}) // echo not needed here
	off, err := pc.CreateOffer(nil)
	if err != nil {
		return err
	}
	gathered := webrtc.GatheringCompletePromise(pc)
	if err := pc.SetLocalDescription(off); err != nil {
		return err
	}
	select {
	case <-gathered:
	case <-time.After(5 * time.Second):
	}
	var res struct {
		Slot  int    `json:"slot"`
		SDP   string `json:"sdp"`
		Error string `json:"error"`
	}
	code, err := httpDo("POST", *base+"/api/join", map[string]any{"clientId": p.id, "wantSlot": wantSlot, "sdp": pc.LocalDescription().SDP}, &res)
	if err != nil || code != 200 {
		return fmt.Errorf("join: HTTP %d %v %s", code, err, res.Error)
	}
	p.slot = res.Slot
	if err := pc.SetRemoteDescription(webrtc.SessionDescription{Type: webrtc.SDPTypeAnswer, SDP: res.SDP}); err != nil {
		return err
	}
	return nil
}

func (p *phone) waitConnected(d time.Duration) bool {
	select {
	case <-p.connCh:
		return true
	case <-time.After(d):
		return false
	}
}

// sendInput writes one input message (the same JSON the phone page sends).
func (p *phone) sendInput(x, y float64, b int) {
	p.mu.Lock()
	p.seq++
	s := p.seq
	p.mu.Unlock()
	msg := fmt.Sprintf(`{"s":%d,"x":%.3f,"y":%.3f,"b":%d}`, s, x, y, b)
	if p.dc != nil && p.dc.ReadyState() == webrtc.DataChannelStateOpen {
		_ = p.dc.SendText(msg)
	}
}

func ts() string { return time.Now().Format("15:04:05.000") }

func post(path string, body any, out any) (int, error) {
	return httpDo("POST", *base+path, body, out)
}

func main() {
	flag.Parse()
	log.SetFlags(0)
	if *configPath == "" {
		log.Fatal("-config is required")
	}
	raw, err := os.ReadFile(*configPath)
	if err != nil {
		log.Fatal(err)
	}
	var cfgs []phoneCfg
	if err := json.Unmarshal(raw, &cfgs); err != nil {
		log.Fatalf("config: %v", err)
	}
	api, err := newAPI()
	if err != nil {
		log.Fatal(err)
	}
	var phones []*phone
	for i, c := range cfgs {
		p := &phone{cfg: c, id: fmt.Sprintf("fake-phone-%d-%s", i+1, strings.ToLower(strings.ReplaceAll(c.Name, " ", "-"))), api: api}
		phones = append(phones, p)
	}
	// Join in order so the phones land on slots 1, 2, 3 ...
	for _, p := range phones {
		if err := p.connect(0); err != nil {
			log.Fatalf("%s: connect: %v", p.cfg.Name, err)
		}
		if !p.waitConnected(15 * time.Second) {
			log.Printf("%s: slot %d did not connect within 15 s", p.cfg.Name, p.slot)
		} else {
			log.Printf("%s %s: connected on slot %d", ts(), p.cfg.Name, p.slot)
		}
	}
	// Choices (kept by partyd across lobby resets).
	for _, p := range phones {
		body := map[string]any{"clientId": p.id, "name": p.cfg.Name, "character": p.cfg.Character,
			"vehicle": p.cfg.Vehicle, "drift": p.cfg.Drift, "vote": p.voteFor(0)}
		if code, err := post("/api/lobby/player", body, nil); err != nil {
			log.Fatalf("%s: choices HTTP %d: %v", p.cfg.Name, code, err)
		}
		log.Printf("%s %s (slot %d): %s / %s / %s / vote %s", ts(), p.cfg.Name, p.slot,
			p.cfg.Character, p.cfg.Vehicle, p.cfg.Drift, p.voteFor(0))
	}
	host := phones[0]
	for _, p := range phones {
		if p.slot < host.slot {
			host = p
		}
	}
	if *framesFlag != "" {
		framesDir = *framesFlag
	} else if exe, err := os.Executable(); err == nil {
		framesDir = filepath.Join(filepath.Dir(exe), "..", "frames")
	}
	go wiggleLoop(phones) // per phone: drive | wiggle | idle (cfg.AfterLive, else -after-live)
	go pollLobby()
	go pollWiring()
	stop := time.After(*duration)
	var since uint64
	startAt := time.Time{}
	liveAt := time.Time{}
	lastPhase := ""
	lastMsg := ""
	readied := false
	rounds := 0
	t0 := time.Time{}
	for {
		select {
		case <-stop:
			log.Printf("%s duration reached; closing phones", ts())
			for _, p := range phones {
				if p.pc != nil {
					_ = p.pc.Close()
				}
			}
			return
		default:
		}
		st, ok := latestLobby()
		if !ok {
			time.Sleep(50 * time.Millisecond)
			continue
		}
		changed := st.Version != since
		since = st.Version
		if st.Phase != lastPhase {
			log.Printf("%s PHASE %s -> %s (message %q)", ts(), lastPhase, st.Phase, st.Message)
			lastPhase = st.Phase
			switch st.Phase {
			case "starting":
				t0 = time.Now()
			case "live":
				liveAt = time.Now()
				if !t0.IsZero() {
					log.Printf("%s LIVE %.1f s after START (round %d)", ts(), time.Since(t0).Seconds(), rounds)
				}
			case "lobby":
				readied = false
				startAt = time.Time{}
				for _, p := range phones {
					p.setHanded(false)
					setLive(p, false)
				}
			}
		}
		if st.Message != lastMsg {
			log.Printf("%s MESSAGE %q", ts(), st.Message)
			lastMsg = st.Message
		}
		if changed && !*quiet {
			for _, lp := range st.Players {
				if lp.Occupied {
					log.Printf("%s   slot %d %-8s ready=%-5v show=%-5v nav=%-5v vote=%-18s status=%q",
						ts(), lp.Slot, lp.Name, lp.Ready, lp.ShowVideo, lp.NavControl, lp.Vote, lp.Status)
				}
			}
		}
		if st.Phase == "lobby" {
			if !readied && (*rounds_ == 0 || rounds < *rounds_) {
				readied = true
				for _, p := range phones {
					if p.cfg.Ready != nil && !*p.cfg.Ready {
						continue
					}
					body := map[string]any{"clientId": p.id, "vote": p.voteFor(0), "ready": true}
					if code, err := post("/api/lobby/player", body, nil); err != nil {
						log.Printf("%s %s: ready HTTP %d: %v", ts(), p.cfg.Name, code, err)
						readied = false
					}
					p.voteRound = 0
				}
			}
			allReady := true
			for _, p := range phones {
				if p.cfg.Ready != nil && !*p.cfg.Ready {
					continue
				}
				if !p.isReady(st) {
					allReady = false
				}
			}
			msgOK := *waitMsg == "" || strings.Contains(st.Message, *waitMsg)
			if allReady && msgOK && (*rounds_ == 0 || rounds < *rounds_) {
				if startAt.IsZero() {
					startAt = time.Now()
				}
				if time.Since(startAt) >= *startAfter {
					code, err := post("/api/lobby/start", map[string]any{"clientId": host.id}, nil)
					if err != nil {
						log.Printf("%s start: HTTP %d %v", ts(), code, err)
						startAt = time.Time{}
					} else {
						rounds++
						log.Printf("%s host (%s) pressed START (round %d)", ts(), host.cfg.Name, rounds)
					}
				}
			} else {
				startAt = time.Time{}
			}
		}
		// Hand-over tracking. A hand-over is showVideo on and navControl off; the
		// navigator takes navControl again for the next race's vote and hands back.
		for _, p := range phones {
			lp := st.player(p.slot)
			mine := lp.ShowVideo && !lp.NavControl
			setLive(p, st.Phase == "live" && mine && !p.dropped)
			if mine && !p.handed() {
				p.setHanded(true)
				p.handovers++
				since0 := ""
				if !t0.IsZero() {
					since0 = fmt.Sprintf(", %.1f s after START", time.Since(t0).Seconds())
				}
				log.Printf("%s %s: HAND-OVER #%d (video on, control to the player) in phase %s%s",
					ts(), p.cfg.Name, p.handovers, st.Phase, since0)
				if *dumpDir != "" {
					path := filepath.Join(*dumpDir, fmt.Sprintf("%s_slot%d.h264", time.Now().Format("150405"), p.slot))
					if w, err := h264writer.New(path); err == nil {
						p.mu.Lock()
						p.dump, p.dumpUntil = w, time.Now().Add(4*time.Second)
						p.mu.Unlock()
						log.Printf("%s %s: recording 4 s of slot %d video to %s", ts(), p.cfg.Name, p.slot, path)
					}
				}
			}
			if !mine && p.handed() {
				p.setHanded(false)
				if lp.NavControl {
					log.Printf("%s %s: navigator took control again (next race vote)", ts(), p.cfg.Name)
				}
			}
			// Per-race picker: choose the next vote when it opens, lock in later.
			if lp.VoteOpen && !p.voteOpen && !p.dropped {
				p.votesSeen++
				v := p.voteFor(p.votesSeen)
				left := ""
				if lp.VoteDL > 0 {
					left = fmt.Sprintf(", deadline in %.1f s", float64(lp.VoteDL-time.Now().UnixMilli())/1000)
				}
				if p.cfg.DropAtVote > 0 && p.votesSeen == p.cfg.DropAtVote {
					log.Printf("%s %s: VOTE PICKER #%d opened%s; phone DISCONNECTS now (last vote %s)", ts(), p.cfg.Name, p.votesSeen, left, lp.Vote)
					p.dropped = true
					setLive(p, false)
					if p.pc != nil {
						_ = p.pc.Close()
					}
				} else {
					code, err := post("/api/lobby/player", map[string]any{"clientId": p.id, "vote": v}, nil)
					log.Printf("%s %s: VOTE PICKER #%d opened%s; picked %s (HTTP %d %v)", ts(), p.cfg.Name, p.votesSeen, left, v, code, err)
					la := 3.0
					if p.cfg.LockAfter != nil {
						la = *p.cfg.LockAfter
					}
					if la >= 0 {
						p.lockAt = time.Now().Add(time.Duration(la * float64(time.Second)))
					} else {
						p.lockAt = time.Time{}
						log.Printf("%s %s: will not lock in (deadline path)", ts(), p.cfg.Name)
					}
				}
			}
			if !lp.VoteOpen && p.voteOpen {
				log.Printf("%s %s: vote picker closed (vote %s, locked %v)", ts(), p.cfg.Name, lp.Vote, lp.VoteLocked)
				p.lockAt = time.Time{}
			}
			p.voteOpen = lp.VoteOpen
			if !p.lockAt.IsZero() && time.Now().After(p.lockAt) && lp.VoteOpen {
				p.lockAt = time.Time{}
				code, err := post("/api/lobby/player", map[string]any{"clientId": p.id, "voteLocked": true}, nil)
				log.Printf("%s %s: LOCKED IN %s (HTTP %d %v)", ts(), p.cfg.Name, lp.Vote, code, err)
			}
			// The vote for the NEXT race: posted once the room is live (votes are locked
			// while starting), one step further down the phone's vote list per hand-over.
			if st.Phase == "live" && p.handovers > 0 && p.voteRound < p.handovers && len(p.cfg.Votes) > 1 && !p.dropped && *voteAt == "handover" {
				v := p.voteFor(p.handovers)
				if code, err := post("/api/lobby/player", map[string]any{"clientId": p.id, "vote": v}, nil); err != nil {
					log.Printf("%s %s: next vote HTTP %d: %v", ts(), p.cfg.Name, code, err)
				} else {
					log.Printf("%s %s: vote for the next race set to %s", ts(), p.cfg.Name, v)
				}
				p.voteRound = p.handovers
			}
		}
		if st.Phase == "live" && *resetAfter > 0 && !liveAt.IsZero() && time.Since(liveAt) >= *resetAfter {
			liveAt = time.Time{}
			if code, err := post("/api/lobby/reset", map[string]any{"clientId": host.id}, nil); err != nil {
				log.Printf("%s reset: HTTP %d %v", ts(), code, err)
			} else {
				log.Printf("%s host (%s) RESET the lobby", ts(), host.cfg.Name)
				for _, p := range phones {
					p.handovers = 0
				}
			}
		}
		time.Sleep(50 * time.Millisecond)
	}
}

// pollLobby long-polls GET /api/lobby in the background; the main loop reads the
// latest answer on its own clock (a long poll can hold for 25 s).
var (
	lobbyMu  sync.Mutex
	lobbyCur lobbyState
	lobbyOK  bool
)

func pollLobby() {
	var since uint64
	first := true
	for {
		var st lobbyState
		url := fmt.Sprintf("%s/api/lobby?since=%d", *base, since)
		if first {
			url = *base + "/api/lobby"
		}
		code, err := httpDo("GET", url, nil, &st)
		if err != nil || code != 200 {
			log.Printf("%s lobby GET: HTTP %d %v", ts(), code, err)
			time.Sleep(time.Second)
			continue
		}
		first = false
		since = st.Version
		lobbyMu.Lock()
		lobbyCur, lobbyOK = st, true
		lobbyMu.Unlock()
	}
}

// wiring: slot -> (instance, pad, mode), from the navigator view GET /api/nav/lobby
// (loopback only), so a fake phone drives with the picture of whatever instance the
// navigator put it on. Without it, slot k reads instance k (the old fixed layout).
var (
	wiringMu sync.Mutex
	wiring   = map[int]view{}
)

func pollWiring() {
	for {
		var st struct {
			Players []struct {
				Slot     int    `json:"slot"`
				Instance int    `json:"instance"`
				Pad      int    `json:"pad"`
				Mode     string `json:"mode"`
			} `json:"players"`
		}
		if code, err := httpDo("GET", *base+"/api/nav/lobby", nil, &st); err == nil && code == 200 {
			m := map[int]view{}
			for _, p := range st.Players {
				if p.Instance > 0 {
					m[p.Slot] = viewFor(p.Instance, max(p.Pad, 1), p.Mode)
				}
			}
			wiringMu.Lock()
			wiring = m
			wiringMu.Unlock()
		}
		time.Sleep(time.Second)
	}
}

func viewOf(slot int) view {
	wiringMu.Lock()
	defer wiringMu.Unlock()
	if v, ok := wiring[slot]; ok {
		return v
	}
	return viewFor(slot, 1, "1p")
}

func latestLobby() (lobbyState, bool) {
	lobbyMu.Lock()
	defer lobbyMu.Unlock()
	return lobbyCur, lobbyOK
}

// voteFor returns the phone's vote for its n-th race (0 = the first).
func (p *phone) voteFor(n int) string {
	if len(p.cfg.Votes) > 0 {
		return p.cfg.Votes[n%len(p.cfg.Votes)]
	}
	if p.cfg.Vote == "" {
		return "random"
	}
	return p.cfg.Vote
}

func (st lobbyState) player(slot int) lobbyPlayer {
	for _, p := range st.Players {
		if p.Slot == slot {
			return p
		}
	}
	return lobbyPlayer{}
}

func (p *phone) isReady(st lobbyState) bool {
	lp := st.player(p.slot)
	return lp.Occupied && lp.Connected && lp.Ready
}

var handedMu sync.Mutex
var handedSet = map[*phone]bool{}

func (p *phone) handed() bool {
	handedMu.Lock()
	defer handedMu.Unlock()
	return handedSet[p]
}

func (p *phone) setHanded(v bool) {
	handedMu.Lock()
	defer handedMu.Unlock()
	handedSet[p] = v
}

// setLive records whether this phone may send input now (after hand-over, not
// gated by the navigator). wiggleLoop reads it.
var (
	liveMu  sync.Mutex
	liveSet = map[*phone]bool{}
)

func setLive(p *phone, v bool) {
	liveMu.Lock()
	defer liveMu.Unlock()
	liveSet[p] = v
}

func isLive(p *phone) bool {
	liveMu.Lock()
	defer liveMu.Unlock()
	return liveSet[p]
}

// wiggleLoop sends input at 20 Hz for every phone that is live: A held and the
// stick swept left and right, like a player who keeps the kart moving.
func wiggleLoop(phones []*phone) {
	drivers := map[*phone]*driver{}
	for {
		time.Sleep(50 * time.Millisecond)
		now := time.Now()
		t := float64(now.UnixNano()) / 1e9
		for _, p := range phones {
			if !isLive(p) {
				continue
			}
			mode := *afterLive
			if p.cfg.AfterLive != "" {
				mode = p.cfg.AfterLive
			}
			if mode == "idle" {
				continue
			}
			if mode == "drive" {
				d := drivers[p]
				if d == nil {
					d = &driver{lastSeen: 1, mm: mmState{char: p.cfg.Character}}
					drivers[p] = d
				}
				x, b := d.next(viewOf(p.slot), now)
				p.sendInput(x, 0, b)
				p.sent++
				continue
			}
			// "wiggle": A held (released briefly every 2 s), stick swung left and right,
			// a short reverse every 14 s. Gets stuck on walls; kept for comparison.
			ph := t + 1.7*float64(p.slot)
			b := 1
			if math.Mod(t, 2.0) > 1.8 {
				b = 0
			}
			x := 0.9 * math.Sin(2*math.Pi*ph/3.6)
			if math.Mod(ph, 14.0) > 12.5 {
				b = 2
				x = 0.9
			}
			p.sendInput(x, 0, b)
			p.sent++
		}
	}
}
