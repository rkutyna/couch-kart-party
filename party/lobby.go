package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"path/filepath"
	"strconv"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"
)

// Lobby phases. lobby -> starting (host calls /api/lobby/start) -> live (the
// navigator reports the race is shown). /api/lobby/reset or /api/nav/reset
// goes back to lobby from any phase.
const (
	phaseLobby    = "lobby"
	phaseStarting = "starting"
	phaseLive     = "live"
)

// longPollWait is how long GET /api/lobby?since=N holds a request open when
// nothing changed. Changes wake waiters immediately.
const longPollWait = 25 * time.Second

const (
	maxNameRunes  = 10
	maxMessageLen = 200
	maxStatusLen  = 120
)

// player is one slot's lobby record and navigator state. It survives a
// reconnect by the same clientId and is reset when a different phone takes the
// slot.
type player struct {
	name       string
	character  string
	vehicle    string
	drift      string // "auto" | "manual"
	vote       string // course id or "random"
	ready      bool
	showVideo  bool
	navControl bool
	status     string

	// Next-race vote. The navigator opens it (voteOpen, voteDeadlineMs) and
	// resets voteLocked when it opens a new one; the phone locks in with
	// voteLocked. The vote itself is the vote field above.
	voteOpen       bool
	voteDeadlineMs int64 // unix ms the vote closes; 0 = no deadline
	voteLocked     bool
}

func defaultPlayer() player { return player{drift: "auto", vote: "random"} }

// playerView is one slot as the API reports it.
type playerView struct {
	Slot       int    `json:"slot"`
	Label      string `json:"label"`
	Occupied   bool   `json:"occupied"`
	Connected  bool   `json:"connected"`
	Locked     bool   `json:"locked"` // in the race (set at Start)
	Name       string `json:"name"`
	Character  string `json:"character"`
	Vehicle    string `json:"vehicle"`
	Drift      string `json:"drift"`
	Vote       string `json:"vote"`
	Ready      bool   `json:"ready"`
	ShowVideo  bool   `json:"showVideo"`
	NavControl bool   `json:"navControl"`
	Status     string `json:"status"`

	VoteOpen        bool  `json:"voteOpen"`
	VoteDeadlineMs  int64 `json:"voteDeadlineMs"`
	VoteRemainingMs int64 `json:"voteRemainingMs"` // max(0, voteDeadlineMs - server now); 0 when no deadline
	VoteLocked      bool  `json:"voteLocked"`
}

// lobbyView is the body of GET /api/lobby and of every lobby POST response.
type lobbyView struct {
	Version        uint64       `json:"version"`
	Phase          string       `json:"phase"`
	Message        string       `json:"message"`
	HostSlot       int          `json:"hostSlot"`
	CatalogVersion string       `json:"catalogVersion"`
	LockedSlots    []int        `json:"lockedSlots"`
	Players        []playerView `json:"players"`
}

// navPlayerView adds the emulator wiring for the navigator.
type navPlayerView struct {
	playerView
	Pipe     string `json:"pipe"`
	RTPPort  int    `json:"rtpPort"`
	Instance int    `json:"instance"`
	Pad      int    `json:"pad"`
	Mode     string `json:"mode"`
}

// navLobbyView is the body of GET /api/nav/lobby (loopback only).
type navLobbyView struct {
	Version        uint64          `json:"version"`
	Phase          string          `json:"phase"`
	Message        string          `json:"message"`
	HostSlot       int             `json:"hostSlot"`
	CatalogVersion string          `json:"catalogVersion"`
	LockedSlots    []int           `json:"lockedSlots"`
	Players        []navPlayerView `json:"players"`
}

func (sl *slot) view() playerView {
	return playerView{
		Slot:       sl.cfg.Slot,
		Label:      sl.cfg.Label,
		Occupied:   sl.owner != "",
		Connected:  sl.connected,
		Locked:     sl.locked,
		Name:       sl.pl.name,
		Character:  sl.pl.character,
		Vehicle:    sl.pl.vehicle,
		Drift:      sl.pl.drift,
		Vote:       sl.pl.vote,
		Ready:      sl.pl.ready,
		ShowVideo:  sl.pl.showVideo,
		NavControl: sl.pl.navControl,
		Status:     sl.pl.status,

		VoteOpen:        sl.pl.voteOpen,
		VoteDeadlineMs:  sl.pl.voteDeadlineMs,
		VoteRemainingMs: voteRemaining(sl.pl.voteDeadlineMs, time.Now()),
		VoteLocked:      sl.pl.voteLocked,
	}
}

// voteRemaining is the time left before a vote closes, in ms, as the server
// sees it. Phones count down from this plus their own arrival time, so they
// never compare the navigator's deadline with their own clock.
func voteRemaining(deadlineMs int64, now time.Time) int64 {
	if deadlineMs <= 0 {
		return 0
	}
	if left := deadlineMs - now.UnixMilli(); left > 0 {
		return left
	}
	return 0
}

// hostSlotLocked is the lowest occupied slot, or 0. Caller holds s.mu.
func (s *Server) hostSlotLocked() int {
	for _, sl := range s.slots {
		if sl.owner != "" {
			return sl.cfg.Slot
		}
	}
	return 0
}

// lockedSlotsLocked lists the slot numbers in the race, lowest first (never
// nil, so it encodes as []). Caller holds s.mu.
func (s *Server) lockedSlotsLocked() []int {
	out := []int{}
	for _, sl := range s.slots {
		if sl.locked {
			out = append(out, sl.cfg.Slot)
		}
	}
	return out
}

// viewLocked builds the lobby (or navigator) view. Caller holds s.mu.
func (s *Server) viewLocked(nav bool) any {
	host := s.hostSlotLocked()
	if !nav {
		players := make([]playerView, 0, len(s.slots))
		for _, sl := range s.slots {
			players = append(players, sl.view())
		}
		return lobbyView{
			Version: s.version, Phase: s.phase, Message: s.message, HostSlot: host,
			CatalogVersion: s.catalogVersion, LockedSlots: s.lockedSlotsLocked(), Players: players,
		}
	}
	players := make([]navPlayerView, 0, len(s.slots))
	for _, sl := range s.slots {
		players = append(players, navPlayerView{
			playerView: sl.view(),
			Pipe:       sl.cfg.Pipe,
			RTPPort:    sl.cfg.RTPPort,
			Instance:   sl.cfg.Instance,
			Pad:        sl.cfg.Pad,
			Mode:       sl.cfg.Mode,
		})
	}
	return navLobbyView{
		Version: s.version, Phase: s.phase, Message: s.message, HostSlot: host,
		CatalogVersion: s.catalogVersion, LockedSlots: s.lockedSlotsLocked(), Players: players,
	}
}

// setPhaseLiveLocked moves to live, cancels a running fake navigator and
// clears navControl; explicit navControl in the same request is applied after.
// Caller holds s.mu.
func (s *Server) setPhaseLiveLocked() {
	s.phase = phaseLive
	s.navGen++
	for _, sl := range s.slots {
		sl.pl.navControl = false
	}
}

// resetLocked returns the room to the lobby. Choices are kept; ready, video,
// race membership and navigator state are cleared. Caller holds s.mu.
func (s *Server) resetLocked() {
	s.phase = phaseLobby
	s.message = ""
	s.navGen++
	for _, sl := range s.slots {
		sl.locked = false
		sl.pl.ready = false
		sl.pl.showVideo = false
		sl.pl.navControl = false
		sl.pl.status = ""
		sl.pl.voteOpen = false
		sl.pl.voteDeadlineMs = 0
		sl.pl.voteLocked = false
	}
	s.applyBlockLocked()
	s.bumpLocked()
}

// cleanName trims and checks a display name: 1 to 10 characters, no control
// characters.
func cleanName(raw string) (string, error) {
	n := strings.TrimSpace(raw)
	if n == "" {
		return "", fmt.Errorf("name must not be empty")
	}
	if utf8.RuneCountInString(n) > maxNameRunes {
		return "", fmt.Errorf("name must be at most %d characters", maxNameRunes)
	}
	for _, r := range n {
		if unicode.IsControl(r) {
			return "", fmt.Errorf("name contains control characters")
		}
	}
	return n, nil
}

type lobbyPlayerReq struct {
	ClientID   string  `json:"clientId"`
	Name       *string `json:"name"`
	Character  *string `json:"character"`
	Vehicle    *string `json:"vehicle"`
	Drift      *string `json:"drift"`
	Vote       *string `json:"vote"`
	Ready      *bool   `json:"ready"`
	VoteLocked *bool   `json:"voteLocked"`
}

// applyPlayerLocked validates a player update and applies it all or nothing.
// It returns an HTTP status (200 = applied) and a message. Caller holds s.mu.
func (s *Server) applyPlayerLocked(sl *slot, req lobbyPlayerReq) (int, string) {
	next := sl.pl
	if req.Name != nil {
		n, err := cleanName(*req.Name)
		if err != nil {
			return http.StatusBadRequest, err.Error()
		}
		next.name = n
	}
	if req.Character != nil {
		ch, ok := s.catalog.character(*req.Character)
		if !ok {
			return http.StatusBadRequest, "unknown character " + strconv.Quote(*req.Character)
		}
		if !ch.Unlocked {
			return http.StatusBadRequest, "character " + ch.ID + " is locked"
		}
		next.character = ch.ID
	}
	if req.Vehicle != nil {
		v, ok := s.catalog.vehicle(*req.Vehicle)
		if !ok {
			return http.StatusBadRequest, "unknown vehicle " + strconv.Quote(*req.Vehicle)
		}
		if !v.Unlocked {
			return http.StatusBadRequest, "vehicle " + v.ID + " is locked"
		}
		next.vehicle = v.ID
	}
	if req.Drift != nil {
		if *req.Drift != "auto" && *req.Drift != "manual" {
			return http.StatusBadRequest, "drift must be \"auto\" or \"manual\""
		}
		next.drift = *req.Drift
	}
	if req.Vote != nil {
		if !s.catalog.validVote(*req.Vote) {
			return http.StatusBadRequest, "unknown course " + strconv.Quote(*req.Vote)
		}
		next.vote = *req.Vote
	}
	if req.VoteLocked != nil {
		if s.phase != phaseLive {
			return http.StatusBadRequest, "the vote can only be locked in while the race is live"
		}
		next.voteLocked = *req.VoteLocked
	}
	if next.vehicle != "" {
		if next.character == "" {
			return http.StatusBadRequest, "choose a character before a vehicle"
		}
		ch, _ := s.catalog.character(next.character)
		v, _ := s.catalog.vehicle(next.vehicle)
		if ch.Weight != v.Weight {
			return http.StatusBadRequest, fmt.Sprintf("%s is a %s vehicle but %s is %s weight; send a vehicle of the same weight",
				v.Name, v.Weight, ch.Name, ch.Weight)
		}
	}
	if req.Ready != nil {
		if *req.Ready && (next.name == "" || next.character == "" || next.vehicle == "") {
			return http.StatusBadRequest, "fill in name, character and vehicle before readying up"
		}
		next.ready = *req.Ready
	}
	if s.phase != phaseLobby {
		if next.name != sl.pl.name || next.character != sl.pl.character || next.vehicle != sl.pl.vehicle ||
			next.drift != sl.pl.drift || next.ready != sl.pl.ready {
			return http.StatusConflict, "the room is no longer in the lobby; only the course vote can change"
		}
		if next.vote != sl.pl.vote && s.phase == phaseStarting {
			return http.StatusConflict, "votes are locked while the race is starting"
		}
	}
	if next != sl.pl {
		sl.pl = next
		s.bumpLocked()
	}
	return http.StatusOK, ""
}

// readOptionalJSON decodes a JSON body when one is present. An empty body is
// fine and leaves v untouched.
func readOptionalJSON(w http.ResponseWriter, r *http.Request, v any) bool {
	if r.Method != http.MethodPost {
		writeErr(w, http.StatusMethodNotAllowed, "POST only")
		return false
	}
	r.Body = http.MaxBytesReader(w, r.Body, maxBody)
	raw, err := io.ReadAll(r.Body)
	if err != nil {
		writeErr(w, http.StatusBadRequest, "bad body: "+err.Error())
		return false
	}
	if len(bytes.TrimSpace(raw)) == 0 {
		return true
	}
	if err := json.Unmarshal(raw, v); err != nil {
		writeErr(w, http.StatusBadRequest, "bad JSON: "+err.Error())
		return false
	}
	return true
}

// requireLoopback restricts the navigator endpoints to connections from this
// machine (127.0.0.0/8 or ::1). Phones on the LAN are refused.
func requireLoopback(w http.ResponseWriter, r *http.Request) bool {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		host = r.RemoteAddr
	}
	ip := net.ParseIP(host)
	if ip == nil || !ip.IsLoopback() {
		writeErr(w, http.StatusForbidden, "navigator endpoints accept loopback connections only")
		return false
	}
	return true
}

// ---------- phone-facing lobby endpoints ----------

// handleLobbyGet serves GET /api/lobby?since=N. It returns at once when the
// version differs from N, otherwise it waits up to longPollWait for a change.
// nav selects the navigator view (the caller must check loopback first).
func (s *Server) handleLobbyGet(nav bool) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			writeErr(w, http.StatusMethodNotAllowed, "GET only")
			return
		}
		since, _ := strconv.ParseUint(r.URL.Query().Get("since"), 10, 64)
		timer := time.NewTimer(longPollWait)
		defer timer.Stop()
		for {
			s.mu.Lock()
			if s.version != since {
				v := s.viewLocked(nav)
				s.mu.Unlock()
				writeJSON(w, http.StatusOK, v)
				return
			}
			ch := s.changed
			s.mu.Unlock()
			select {
			case <-ch:
			case <-timer.C:
				s.mu.Lock()
				v := s.viewLocked(nav)
				s.mu.Unlock()
				writeJSON(w, http.StatusOK, v)
				return
			case <-r.Context().Done():
				return
			}
		}
	}
}

func (s *Server) handleLobbyPlayer(w http.ResponseWriter, r *http.Request) {
	var req lobbyPlayerReq
	if !decode(w, r, &req) {
		return
	}
	s.mu.Lock()
	sl := s.ownerSlotLocked(req.ClientID)
	if sl == nil {
		s.mu.Unlock()
		writeErr(w, http.StatusForbidden, "this phone does not hold a player slot; join first")
		return
	}
	if code, msg := s.applyPlayerLocked(sl, req); code != http.StatusOK {
		s.mu.Unlock()
		writeErr(w, code, msg)
		return
	}
	v := s.viewLocked(false)
	s.mu.Unlock()
	writeJSON(w, http.StatusOK, v)
}

type clientReq struct {
	ClientID string `json:"clientId"`
}

// hostFromClientLocked returns the caller's slot when the caller is the host.
// It writes the error and returns nil otherwise. Caller holds s.mu.
func (s *Server) hostFromClientLocked(w http.ResponseWriter, clientID string) *slot {
	sl := s.ownerSlotLocked(clientID)
	if sl == nil {
		writeErr(w, http.StatusForbidden, "this phone does not hold a player slot; join first")
		return nil
	}
	if host := s.hostSlotLocked(); host != sl.cfg.Slot {
		writeErr(w, http.StatusForbidden, fmt.Sprintf("only the host (slot %d) can do this", host))
		return nil
	}
	return sl
}

func (s *Server) handleLobbyStart(w http.ResponseWriter, r *http.Request) {
	var req clientReq
	if !decode(w, r, &req) {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.hostFromClientLocked(w, req.ClientID) == nil {
		return
	}
	if s.phase != phaseLobby {
		writeErr(w, http.StatusConflict, "the room is already "+s.phase)
		return
	}
	// The race is locked to the slots that are occupied, connected and ready
	// now. Anyone else sits this round out.
	connected, ready := 0, 0
	for _, sl := range s.slots {
		if sl.owner == "" || !sl.connected {
			continue
		}
		connected++
		if sl.pl.ready {
			ready++
		}
	}
	if connected == 0 {
		writeErr(w, http.StatusConflict, "no connected players")
		return
	}
	if ready == 0 {
		writeErr(w, http.StatusConflict, "at least one player must be ready")
		return
	}
	for _, sl := range s.slots {
		sl.locked = sl.owner != "" && sl.connected && sl.pl.ready
	}
	s.phase = phaseStarting
	s.message = ""
	s.applyBlockLocked()
	log.Printf("lobby: race starting with %d players (slots %v), %d sitting out",
		ready, s.lockedSlotsLocked(), connected-ready)
	if s.fakeNav {
		s.startFakeNavLocked()
	}
	s.bumpLocked()
	writeJSON(w, http.StatusOK, s.viewLocked(false))
}

func (s *Server) handleLobbyReset(w http.ResponseWriter, r *http.Request) {
	var req clientReq
	if !decode(w, r, &req) {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.hostFromClientLocked(w, req.ClientID) == nil {
		return
	}
	s.resetLocked()
	log.Printf("lobby: reset by host")
	writeJSON(w, http.StatusOK, s.viewLocked(false))
}

// ---------- navigator endpoints (loopback only) ----------

func (s *Server) handleNavLobby(w http.ResponseWriter, r *http.Request) {
	if !requireLoopback(w, r) {
		return
	}
	s.handleLobbyGet(true)(w, r)
}

type navUpdateReq struct {
	Phase   *string      `json:"phase"`
	Message *string      `json:"message"`
	Slots   []navSlotReq `json:"slots"`
}

type navSlotReq struct {
	Slot           int     `json:"slot"`
	Status         *string `json:"status"`
	ShowVideo      *bool   `json:"showVideo"`
	NavControl     *bool   `json:"navControl"`
	VoteOpen       *bool   `json:"voteOpen"`
	VoteDeadlineMs *int64  `json:"voteDeadlineMs"`
	VoteLocked     *bool   `json:"voteLocked"`
}

func (s *Server) handleNavUpdate(w http.ResponseWriter, r *http.Request) {
	if !requireLoopback(w, r) {
		return
	}
	var req navUpdateReq
	if !decode(w, r, &req) {
		return
	}
	if req.Phase != nil && *req.Phase != phaseStarting && *req.Phase != phaseLive {
		writeErr(w, http.StatusBadRequest, "phase must be \"starting\" or \"live\" (use /api/nav/reset to return to the lobby)")
		return
	}
	if req.Message != nil && len(*req.Message) > maxMessageLen {
		writeErr(w, http.StatusBadRequest, fmt.Sprintf("message longer than %d bytes", maxMessageLen))
		return
	}
	for _, e := range req.Slots {
		if e.Status != nil && len(*e.Status) > maxStatusLen {
			writeErr(w, http.StatusBadRequest, fmt.Sprintf("status longer than %d bytes", maxStatusLen))
			return
		}
		if e.VoteDeadlineMs != nil && *e.VoteDeadlineMs < 0 {
			writeErr(w, http.StatusBadRequest, fmt.Sprintf("slot %d: voteDeadlineMs must be 0 (no deadline) or a unix ms time", e.Slot))
			return
		}
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, e := range req.Slots {
		if s.byNum[e.Slot] == nil {
			writeErr(w, http.StatusBadRequest, fmt.Sprintf("unknown slot %d", e.Slot))
			return
		}
	}
	if req.Phase != nil && *req.Phase != s.phase {
		switch {
		case *req.Phase == phaseLive && s.phase == phaseStarting:
			s.setPhaseLiveLocked()
		default:
			writeErr(w, http.StatusConflict, fmt.Sprintf("cannot move from %s to %s", s.phase, *req.Phase))
			return
		}
	}
	if req.Message != nil {
		s.message = *req.Message
	}
	for _, e := range req.Slots {
		sl := s.byNum[e.Slot]
		if e.Status != nil {
			sl.pl.status = *e.Status
		}
		if e.ShowVideo != nil {
			sl.pl.showVideo = *e.ShowVideo
		}
		if e.NavControl != nil {
			sl.pl.navControl = *e.NavControl
		}
		if e.VoteOpen != nil {
			sl.pl.voteOpen = *e.VoteOpen
		}
		if e.VoteDeadlineMs != nil {
			sl.pl.voteDeadlineMs = *e.VoteDeadlineMs
		}
		if e.VoteLocked != nil {
			sl.pl.voteLocked = *e.VoteLocked
		}
	}
	s.applyBlockLocked()
	s.bumpLocked()
	writeJSON(w, http.StatusOK, s.viewLocked(true))
}

func (s *Server) handleNavReset(w http.ResponseWriter, r *http.Request) {
	if !requireLoopback(w, r) {
		return
	}
	var req struct {
		Message *string `json:"message"`
	}
	if !readOptionalJSON(w, r, &req) {
		return
	}
	if req.Message != nil && len(*req.Message) > maxMessageLen {
		writeErr(w, http.StatusBadRequest, fmt.Sprintf("message longer than %d bytes", maxMessageLen))
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.resetLocked()
	if req.Message != nil {
		s.message = *req.Message
		s.bumpLocked()
	}
	writeJSON(w, http.StatusOK, s.viewLocked(true))
}

// handleNavFake is the testing stand-in for the navigator. It needs phase
// starting (POST /api/lobby/start first).
func (s *Server) handleNavFake(w http.ResponseWriter, r *http.Request) {
	if !requireLoopback(w, r) {
		return
	}
	if r.Method != http.MethodPost {
		writeErr(w, http.StatusMethodNotAllowed, "POST only")
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.phase != phaseStarting {
		writeErr(w, http.StatusConflict, "fake navigator runs in phase starting; POST /api/lobby/start first")
		return
	}
	s.startFakeNavLocked()
	writeJSON(w, http.StatusOK, s.viewLocked(true))
}

// ---------- fake navigator ----------

var fakeNavSteps = []struct {
	at      time.Duration
	message string
	status  string
}{
	{0, "Navigator (fake): joining rooms", "Joining room..."},
	{1500 * time.Millisecond, "Navigator (fake): choosing characters", "Choosing character..."},
	{3000 * time.Millisecond, "Navigator (fake): choosing courses", "Choosing course..."},
	{4500 * time.Millisecond, "Navigator (fake): waiting for votes", "Waiting for votes..."},
}

const fakeNavLiveAt = 6 * time.Second

// startFakeNavLocked starts the simulated navigator. Caller holds s.mu and has
// checked phase == starting.
func (s *Server) startFakeNavLocked() {
	s.navGen++
	gen := s.navGen
	go s.runFakeNav(gen)
}

func (s *Server) runFakeNav(gen uint64) {
	begin := time.Now()
	for _, st := range fakeNavSteps {
		time.Sleep(time.Until(begin.Add(st.at)))
		s.mu.Lock()
		if s.navGen != gen || s.phase != phaseStarting {
			s.mu.Unlock()
			return
		}
		s.message = st.message
		for _, sl := range s.slots {
			sl.pl.status = st.status
		}
		s.bumpLocked()
		s.mu.Unlock()
	}
	time.Sleep(time.Until(begin.Add(fakeNavLiveAt)))
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.navGen != gen || s.phase != phaseStarting {
		return
	}
	s.setPhaseLiveLocked()
	for _, sl := range s.slots {
		sl.pl.showVideo = sl.locked
		sl.pl.status = ""
	}
	s.message = "Live (fake navigator)"
	s.applyBlockLocked()
	s.bumpLocked()
	log.Printf("fake navigator: live")
}

// ---------- navigator: slot assignment (loopback only) ----------

type navAssignReq struct {
	Slots []navAssignSlot `json:"slots"`
}

// navAssignSlot changes one slot's emulator wiring. A nil field is left as it
// is; pipe "" unassigns the slot.
type navAssignSlot struct {
	Slot     int     `json:"slot"`
	Pipe     *string `json:"pipe"`
	Instance *int    `json:"instance"`
	Pad      *int    `json:"pad"`
	Mode     *string `json:"mode"`
}

// handleNavAssign serves POST /api/nav/assign. When a slot's pipe changes, the
// slot's held input is released on the old FIFO first (stick centred, buttons
// released), then the pipe writer switches to the new FIFO. Returns the nav view.
func (s *Server) handleNavAssign(w http.ResponseWriter, r *http.Request) {
	if !requireLoopback(w, r) {
		return
	}
	var req navAssignReq
	if !decode(w, r, &req) {
		return
	}
	if len(req.Slots) == 0 {
		writeErr(w, http.StatusBadRequest, "slots required")
		return
	}
	s.mu.Lock()
	seen := map[int]bool{}
	for _, e := range req.Slots {
		if s.byNum[e.Slot] == nil {
			s.mu.Unlock()
			writeErr(w, http.StatusBadRequest, fmt.Sprintf("unknown slot %d", e.Slot))
			return
		}
		if seen[e.Slot] {
			s.mu.Unlock()
			writeErr(w, http.StatusBadRequest, fmt.Sprintf("slot %d listed twice", e.Slot))
			return
		}
		seen[e.Slot] = true
		if e.Pipe != nil && *e.Pipe != "" && !filepath.IsAbs(*e.Pipe) {
			s.mu.Unlock()
			writeErr(w, http.StatusBadRequest, fmt.Sprintf("slot %d: pipe must be an absolute path or \"\"", e.Slot))
			return
		}
	}
	// Resulting pipes must be unique: two slots on one FIFO would mix inputs.
	next := map[int]string{}
	for _, sl := range s.slots {
		next[sl.cfg.Slot] = sl.cfg.Pipe
	}
	for _, e := range req.Slots {
		if e.Pipe != nil {
			next[e.Slot] = ""
			if *e.Pipe != "" {
				next[e.Slot] = filepath.Clean(*e.Pipe)
			}
		}
	}
	owner := map[string]int{}
	for _, sl := range s.slots {
		p := next[sl.cfg.Slot]
		if p == "" {
			continue
		}
		if other, dup := owner[p]; dup {
			s.mu.Unlock()
			writeErr(w, http.StatusBadRequest, fmt.Sprintf("pipe %s would be used by slots %d and %d", p, other, sl.cfg.Slot))
			return
		}
		owner[p] = sl.cfg.Slot
	}

	type switchWait struct {
		pw   *pipeWriter
		path string
	}
	var waits []switchWait
	for _, e := range req.Slots {
		sl := s.byNum[e.Slot]
		if e.Pipe != nil && next[e.Slot] != sl.cfg.Pipe {
			if sl.ctrl != nil {
				sl.ctrl.Release() // centre the stick and release buttons on the old FIFO
			}
			sl.cfg.Pipe = next[e.Slot]
			if sl.pipe != nil {
				sl.pipe.setPath(sl.cfg.Pipe)
				waits = append(waits, switchWait{sl.pipe, sl.cfg.Pipe})
			}
			log.Printf("slot %d: pipe now %s", sl.cfg.Slot, orUnassigned(sl.cfg.Pipe))
		}
		if e.Instance != nil {
			sl.cfg.Instance = *e.Instance
		}
		if e.Pad != nil {
			sl.cfg.Pad = *e.Pad
		}
		if e.Mode != nil {
			sl.cfg.Mode = *e.Mode
		}
	}
	s.rebuildRoutesLocked()
	s.bumpLocked()
	view := s.viewLocked(true)
	s.mu.Unlock()
	// Wait for the pipe writers to switch (they drain the old FIFO first), so
	// that once this call returns, new input goes to the new FIFO.
	for _, sw := range waits {
		sw.pw.waitApplied(sw.path, 2*time.Second)
	}
	writeJSON(w, http.StatusOK, view)
}
