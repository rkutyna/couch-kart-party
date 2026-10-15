package main

import (
	"errors"
	"fmt"
	"log"
	"sort"
	"sync"
	"time"

	"github.com/pion/webrtc/v4"
)

var (
	errNoSlot         = errors.New("no free slot")
	errSuperseded     = errors.New("join superseded by a newer join for the same slot")
	errGameInProgress = errors.New("game in progress — you can join the next round")
)

// slot is one player position. All mutable fields are guarded by Server.mu,
// except pipe, track and ctr which are set once at startup.
type slot struct {
	cfg   SlotConfig
	track *webrtc.TrackLocalStaticRTP
	pipe  *pipeWriter
	ctr   *slotCounters

	owner     string // clientId holding the slot; "" = free
	gen       uint64 // bumped on every join; stale callbacks compare against it
	pc        *webrtc.PeerConnection
	connected bool
	freeTimer *time.Timer
	ctrl      *controller

	pl       player // lobby choices and navigator state for this slot
	locked   bool   // in the race: set at Start for occupied, connected, ready slots; cleared on reset
	blocked  bool   // input currently gated (see inputBlockedLocked)
	latest   map[string]any
	latestAt time.Time
}

// Server holds the slot table, the lobby state, the Pion API and the viewer
// connections.
type Server struct {
	api            *webrtc.API
	freeDelay      time.Duration
	catalog        *Catalog
	catalogVersion string
	fakeNav        bool
	statsLog       *statsLog
	joinCfg        *joinSettings // phone URLs and QR codes (joininfo.go)

	mu      sync.Mutex
	slots   []*slot // sorted by slot number
	byNum   map[int]*slot
	viewers map[*webrtc.PeerConnection]struct{}

	phase   string        // phaseLobby | phaseStarting | phaseLive
	message string        // global status line from the navigator
	version uint64        // bumped on every lobby-visible change
	changed chan struct{} // closed and replaced on every bump (long-poll wakeup)
	navGen  uint64        // bumped to cancel a running fake navigator

	router sourceRouter // instance video sources -> slots (source.go)
}

func newServer(api *webrtc.API, freeDelay time.Duration) *Server {
	return &Server{
		api:       api,
		freeDelay: freeDelay,
		byNum:     map[int]*slot{},
		viewers:   map[*webrtc.PeerConnection]struct{}{},
		phase:     phaseLobby,
		version:   1,
		changed:   make(chan struct{}),
	}
}

func (s *Server) addSlot(sl *slot) {
	sl.pl = defaultPlayer()
	s.slots = append(s.slots, sl)
	sort.Slice(s.slots, func(i, j int) bool { return s.slots[i].cfg.Slot < s.slots[j].cfg.Slot })
	s.byNum[sl.cfg.Slot] = sl
	s.rebuildRoutesLocked()
}

// joinAllowedLocked: during a race only phones that hold a slot may (re)join.
// Caller holds s.mu.
func (s *Server) joinAllowedLocked(clientID string) bool {
	if s.phase != phaseStarting && s.phase != phaseLive {
		return true
	}
	return s.ownerSlotLocked(clientID) != nil
}

// pickSlotLocked: same clientId keeps its slot; otherwise wantSlot if free;
// otherwise the lowest free slot. Caller holds s.mu.
func (s *Server) pickSlotLocked(clientID string, want int) *slot {
	for _, sl := range s.slots {
		if sl.owner == clientID {
			return sl
		}
	}
	if sl := s.byNum[want]; want > 0 && sl != nil && sl.owner == "" {
		return sl
	}
	for _, sl := range s.slots {
		if sl.owner == "" {
			return sl
		}
	}
	return nil
}

// join assigns a slot to clientID and returns the answer SDP for its offer.
// While the race is starting or live only phones that already hold a slot may
// connect; anyone else gets errGameInProgress.
func (s *Server) join(clientID string, want int, offer string) (int, string, error) {
	s.mu.Lock()
	if !s.joinAllowedLocked(clientID) {
		s.mu.Unlock()
		return 0, "", errGameInProgress
	}
	sl := s.pickSlotLocked(clientID, want)
	if sl == nil {
		s.mu.Unlock()
		return 0, "", errNoSlot
	}
	sl.gen++
	gen := sl.gen
	oldPC := sl.pc
	sl.pc = nil
	sl.connected = false
	if sl.freeTimer != nil {
		sl.freeTimer.Stop()
		sl.freeTimer = nil
	}
	if sl.ctrl != nil {
		sl.ctrl.Close() // releases whatever the old phone was holding
	}
	ctrl := newController(sl.pipe.Send)
	sl.ctrl = ctrl
	if sl.owner != clientID {
		sl.pl = defaultPlayer() // a new occupant starts with a blank lobby record
	}
	sl.owner = clientID
	s.bumpLocked()
	s.applyBlockLocked()
	s.mu.Unlock()

	if oldPC != nil {
		_ = oldPC.Close() // old callbacks see a stale gen and do nothing
	}

	pc, answer, err := s.newPlayerPC(sl, gen, ctrl, offer)
	if err != nil {
		s.mu.Lock()
		if sl.gen == gen {
			ctrl.Close()
			sl.owner = ""
			sl.ctrl = nil
			s.bumpLocked()
		}
		s.mu.Unlock()
		return 0, "", err
	}

	s.mu.Lock()
	if sl.gen != gen {
		s.mu.Unlock()
		_ = pc.Close()
		return 0, "", errSuperseded
	}
	sl.pc = pc
	s.mu.Unlock()
	log.Printf("slot %d: client %s joined", sl.cfg.Slot, shortID(clientID))
	return sl.cfg.Slot, answer, nil
}

// newPlayerPC wires a PeerConnection for a player: state callbacks, the input
// data channel (gated by the navigator state), the ping echo, and the answer.
func (s *Server) newPlayerPC(sl *slot, gen uint64, ctrl *controller, offer string) (*webrtc.PeerConnection, string, error) {
	pc, answer, err := answerOffer(s.api, offer, sl.track, sl.ctr)
	if err != nil {
		return nil, "", err
	}
	pc.OnDataChannel(func(dc *webrtc.DataChannel) {
		switch dc.Label() {
		case "input":
			dc.OnMessage(func(m webrtc.DataChannelMessage) {
				// Under s.mu so a navigator transition cannot slip between the
				// gate check and the controller update.
				s.mu.Lock()
				defer s.mu.Unlock()
				if sl.gen != gen || s.inputBlockedLocked(sl) {
					return
				}
				ctrl.Handle(m.Data)
			})
			dc.OnClose(func() {
				ctrl.Release()
			})
		case "ping":
			// Latency probe: echo the bytes back unchanged.
			dc.OnMessage(func(m webrtc.DataChannelMessage) {
				_ = dc.Send(m.Data)
			})
		}
	})
	pc.OnConnectionStateChange(func(st webrtc.PeerConnectionState) {
		s.mu.Lock()
		defer s.mu.Unlock()
		if sl.gen != gen {
			return
		}
		switch st {
		case webrtc.PeerConnectionStateConnected:
			sl.connected = true
			s.bumpLocked()
			log.Printf("slot %d: connected", sl.cfg.Slot)
		case webrtc.PeerConnectionStateDisconnected:
			sl.connected = false
			ctrl.Release()
			s.bumpLocked()
			log.Printf("slot %d: disconnected (no packets from the phone for %s)", sl.cfg.Slot, iceDisconnectedTimeout)
		case webrtc.PeerConnectionStateFailed, webrtc.PeerConnectionStateClosed:
			sl.connected = false
			ctrl.Release()
			s.bumpLocked()
			log.Printf("slot %d: connection %s; slot stays reserved for %s", sl.cfg.Slot, st, s.freeDelay)
			s.scheduleFreeLocked(sl, gen)
		}
	})
	closeWhenDead(pc, fmt.Sprintf("slot %d", sl.cfg.Slot))
	return pc, answer, nil
}

// connectTimeout: a PeerConnection that has not connected this long after the answer
// (the phone went away during the handshake) is closed.
const connectTimeout = 30 * time.Second

// closeWhenDead closes pc when it fails, or when it has not connected within
// connectTimeout. A PeerConnection that is not closed stays bound to the slot's shared
// video track: every packet is still copied into its send path and NACK buffer (about
// 1.5 MB per binding), so before this every vanished phone leaked memory and CPU for
// the rest of the party (partyd reached 717 MB after 3 hours of test rounds).
func closeWhenDead(pc *webrtc.PeerConnection, what string) {
	pc.OnICEConnectionStateChange(func(st webrtc.ICEConnectionState) {
		if st == webrtc.ICEConnectionStateFailed {
			go func() { _ = pc.Close() }() // not from inside Pion's callback
		}
	})
	time.AfterFunc(connectTimeout, func() {
		switch pc.ConnectionState() {
		case webrtc.PeerConnectionStateNew, webrtc.PeerConnectionStateConnecting:
			log.Printf("%s: no connection %s after the answer; closing it", what, connectTimeout)
			_ = pc.Close()
		}
	})
}

// scheduleFreeLocked frees the slot after freeDelay unless a newer join
// happened first. Caller holds s.mu.
func (s *Server) scheduleFreeLocked(sl *slot, gen uint64) {
	if sl.freeTimer != nil {
		sl.freeTimer.Stop()
	}
	sl.freeTimer = time.AfterFunc(s.freeDelay, func() {
		s.mu.Lock()
		defer s.mu.Unlock()
		if sl.gen != gen {
			return
		}
		sl.freeTimer = nil
		sl.owner = ""
		if sl.pc != nil {
			go func(pc *webrtc.PeerConnection) { _ = pc.Close() }(sl.pc)
		}
		sl.pc = nil
		sl.pl = defaultPlayer()
		s.bumpLocked()
		log.Printf("slot %d: free", sl.cfg.Slot)
	})
}

// watch attaches a viewer to a slot's track. It takes no slot and opens no
// input channel.
func (s *Server) watch(slotNum int, offer string) (string, error) {
	s.mu.Lock()
	sl := s.byNum[slotNum]
	s.mu.Unlock()
	if sl == nil {
		return "", errUnknownSlot
	}
	pc, answer, err := answerOffer(s.api, offer, sl.track, nil)
	if err != nil {
		return "", err
	}
	s.mu.Lock()
	s.viewers[pc] = struct{}{}
	s.mu.Unlock()
	closeWhenDead(pc, fmt.Sprintf("viewer of slot %d", slotNum))
	pc.OnConnectionStateChange(func(st webrtc.PeerConnectionState) {
		if st == webrtc.PeerConnectionStateFailed || st == webrtc.PeerConnectionStateClosed {
			s.mu.Lock()
			delete(s.viewers, pc)
			s.mu.Unlock()
			_ = pc.Close()
		}
	})
	return answer, nil
}

var errUnknownSlot = errors.New("unknown slot")

type slotState struct {
	Slot      int    `json:"slot"`
	Label     string `json:"label"`
	Occupied  bool   `json:"occupied"`
	Connected bool   `json:"connected"`
	Locked    bool   `json:"locked"`
}

func (s *Server) state() []slotState {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]slotState, 0, len(s.slots))
	for _, sl := range s.slots {
		out = append(out, slotState{
			Slot:      sl.cfg.Slot,
			Label:     sl.cfg.Label,
			Occupied:  sl.owner != "",
			Connected: sl.connected,
			Locked:    sl.locked,
		})
	}
	return out
}

// ownerSlotLocked returns the slot held by clientID, or nil. Caller holds s.mu.
func (s *Server) ownerSlotLocked(clientID string) *slot {
	if clientID == "" {
		return nil
	}
	for _, sl := range s.slots {
		if sl.owner == clientID {
			return sl
		}
	}
	return nil
}

// bumpLocked records a lobby-visible change and wakes every long-poll waiter.
// Caller holds s.mu.
func (s *Server) bumpLocked() {
	s.version++
	close(s.changed)
	s.changed = make(chan struct{})
}

// inputBlockedLocked reports whether a slot's phone input must be dropped: the
// navigator is driving the emulator (starting), the slot is held by the
// navigator, or the race is live and this slot is not in it (it sat out).
// Caller holds s.mu.
func (s *Server) inputBlockedLocked(sl *slot) bool {
	if s.phase == phaseStarting || sl.pl.navControl {
		return true
	}
	return s.phase == phaseLive && !sl.locked
}

// applyBlockLocked recomputes the input gate for every slot. A slot that just
// became gated gets its held buttons and stick released, so nothing stays
// pressed in the emulator. Caller holds s.mu.
func (s *Server) applyBlockLocked() {
	for _, sl := range s.slots {
		b := s.inputBlockedLocked(sl)
		if b && !sl.blocked && sl.ctrl != nil {
			sl.ctrl.Release()
		}
		sl.blocked = b
	}
}

// closeAll tears down every PeerConnection and releases all inputs.
func (s *Server) closeAll() {
	s.mu.Lock()
	var pcs []*webrtc.PeerConnection
	for _, sl := range s.slots {
		if sl.pc != nil {
			pcs = append(pcs, sl.pc)
		}
		if sl.ctrl != nil {
			sl.ctrl.Close()
		}
	}
	for pc := range s.viewers {
		pcs = append(pcs, pc)
	}
	s.mu.Unlock()
	for _, pc := range pcs {
		_ = pc.Close()
	}
}

func shortID(id string) string {
	if len(id) <= 8 {
		return id
	}
	return id[:8]
}
