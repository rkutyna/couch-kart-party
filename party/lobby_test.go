package main

import (
	"bufio"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"testing"
	"testing/fstest"
	"time"
)

func TestRequireLoopback(t *testing.T) {
	cases := map[string]bool{
		"127.0.0.1:5000":     true,
		"127.3.4.5:5000":     true,
		"[::1]:5000":         true,
		"192.168.2.1:5000":   false,
		"100.64.0.10:5000":   false, // CGNAT (e.g. Tailscale)
		"[2001:db8::7]:5000": false,
		"garbage":            false,
	}
	for addr, want := range cases {
		r := httptest.NewRequest(http.MethodGet, "/api/nav/lobby", nil)
		r.RemoteAddr = addr
		w := httptest.NewRecorder()
		got := requireLoopback(w, r)
		if got != want {
			t.Errorf("requireLoopback(%q) = %v, want %v", addr, got, want)
		}
		if !want && w.Code != http.StatusForbidden {
			t.Errorf("requireLoopback(%q) wrote %d, want 403", addr, w.Code)
		}
	}
}

func TestCleanName(t *testing.T) {
	ok := map[string]string{" Rob ": "Rob", "ABCDEFGHIJ": "ABCDEFGHIJ", "Zoë": "Zoë"}
	for in, want := range ok {
		got, err := cleanName(in)
		if err != nil || got != want {
			t.Errorf("cleanName(%q) = %q, %v; want %q", in, got, err, want)
		}
	}
	for _, in := range []string{"", "   ", "ABCDEFGHIJK", "a\tb"} {
		if _, err := cleanName(in); err == nil {
			t.Errorf("cleanName(%q) accepted, want error", in)
		}
	}
}

func TestEmbeddedCatalogValid(t *testing.T) {
	web, err := webFS("")
	if err != nil {
		t.Fatal(err)
	}
	c, ver, err := loadCatalog(web)
	if err != nil {
		t.Fatal(err)
	}
	if len(c.Characters) != 24 || len(c.Vehicles) != 36 || len(c.Cups) != 8 || len(c.courses) != 32 {
		t.Errorf("catalog shape: %d chars, %d vehicles, %d cups, %d courses", len(c.Characters), len(c.Vehicles), len(c.Cups), len(c.courses))
	}
	if len(ver) != 12 {
		t.Errorf("catalog version %q", ver)
	}
}

func TestCatalogRejectsBadData(t *testing.T) {
	bad := fstest.MapFS{"catalog.json": &fstest.MapFile{Data: []byte(`{"characters":[{"id":"a","weight":"huge"}],"vehicles":[{"id":"v","weight":"light","type":"kart"}],"cups":[{"courses":[{"id":"c1"}]}]}`)}}
	if _, _, err := loadCatalog(bad); err == nil {
		t.Error("catalog with bad weight accepted")
	}
	dupe := fstest.MapFS{"catalog.json": &fstest.MapFile{Data: []byte(`{"characters":[{"id":"a","weight":"light"}],"vehicles":[{"id":"v","weight":"light","type":"kart"}],"cups":[{"courses":[{"id":"c1"},{"id":"c1"}]}]}`)}}
	if _, _, err := loadCatalog(dupe); err == nil {
		t.Error("catalog with duplicate course accepted")
	}
}

// testServer builds a lobby-only Server (no WebRTC) with the embedded catalog
// and two slots.
func testServer(t *testing.T) *Server {
	t.Helper()
	web, err := webFS("")
	if err != nil {
		t.Fatal(err)
	}
	cat, ver, err := loadCatalog(web)
	if err != nil {
		t.Fatal(err)
	}
	s := newServer(nil, time.Second)
	s.catalog = cat
	s.catalogVersion = ver
	for i := 1; i <= 2; i++ {
		s.addSlot(&slot{cfg: SlotConfig{Slot: i, Label: "P" + string(rune('0'+i))}})
	}
	return s
}

func sp(s string) *string { return &s }
func bp(b bool) *bool     { return &b }

func TestApplyPlayerRules(t *testing.T) {
	s := testServer(t)
	s.slots[0].owner = "A"
	s.slots[0].pl = defaultPlayer()
	s.bumpLocked()
	sl := s.slots[0]

	expect := func(desc string, req lobbyPlayerReq, want int) {
		t.Helper()
		code, msg := s.applyPlayerLocked(sl, req)
		if code != want {
			t.Errorf("%s: got %d (%s), want %d", desc, code, msg, want)
		}
	}
	expect("unknown character", lobbyPlayerReq{Character: sp("nobody")}, http.StatusBadRequest)
	expect("locked character", lobbyPlayerReq{Character: sp("metal-mario")}, http.StatusBadRequest)
	expect("unknown vehicle", lobbyPlayerReq{Vehicle: sp("nope")}, http.StatusBadRequest)
	expect("vehicle before character", lobbyPlayerReq{Vehicle: sp("standard-kart-s")}, http.StatusBadRequest)
	expect("light char + light vehicle", lobbyPlayerReq{Character: sp("toad"), Vehicle: sp("standard-kart-s")}, http.StatusOK)
	expect("heavy char keeps light vehicle", lobbyPlayerReq{Character: sp("bowser")}, http.StatusBadRequest)
	expect("heavy char + heavy vehicle", lobbyPlayerReq{Character: sp("bowser"), Vehicle: sp("standard-kart-l")}, http.StatusOK)
	expect("bad drift", lobbyPlayerReq{Drift: sp("sloppy")}, http.StatusBadRequest)
	expect("manual drift", lobbyPlayerReq{Drift: sp("manual")}, http.StatusOK)
	expect("unknown course", lobbyPlayerReq{Vote: sp("moon-base")}, http.StatusBadRequest)
	expect("course vote", lobbyPlayerReq{Vote: sp("luigi-circuit")}, http.StatusOK)
	expect("ready without name", lobbyPlayerReq{Ready: bp(true)}, http.StatusBadRequest)
	expect("name too long", lobbyPlayerReq{Name: sp("ABCDEFGHIJK")}, http.StatusBadRequest)
	expect("name", lobbyPlayerReq{Name: sp("Rob")}, http.StatusOK)
	expect("ready", lobbyPlayerReq{Ready: bp(true)}, http.StatusOK)
	if !sl.pl.ready || sl.pl.name != "Rob" || sl.pl.vote != "luigi-circuit" || sl.pl.drift != "manual" {
		t.Fatalf("state after accepted updates: %+v", sl.pl)
	}

	// Outside the lobby only the vote may change.
	s.phase = phaseLive
	expect("name change in live", lobbyPlayerReq{Name: sp("Bob")}, http.StatusConflict)
	expect("same name in live is a no-op", lobbyPlayerReq{Name: sp("Rob")}, http.StatusOK)
	expect("vote change in live", lobbyPlayerReq{Vote: sp("random")}, http.StatusOK)
	s.phase = phaseStarting
	expect("vote change while starting", lobbyPlayerReq{Vote: sp("moo-moo-meadows")}, http.StatusConflict)
	s.phase = phaseLobby
}

func TestNavTransitionsAndReset(t *testing.T) {
	s := testServer(t)
	s.slots[0].owner = "A"
	s.slots[0].pl = defaultPlayer()
	s.slots[0].pl.ready = true
	s.slots[0].pl.navControl = true
	s.phase = phaseStarting
	s.applyBlockLocked()
	if !s.slots[0].blocked {
		t.Fatal("starting phase should gate input")
	}
	s.slots[0].locked = true // a racer: input flows in live
	s.setPhaseLiveLocked()
	s.applyBlockLocked()
	if s.slots[0].pl.navControl {
		t.Error("going live should clear navControl")
	}
	if s.slots[0].blocked {
		t.Error("live with no navControl should not gate input")
	}
	s.slots[0].pl.navControl = true
	s.applyBlockLocked()
	if !s.slots[0].blocked {
		t.Error("navControl should gate input in live")
	}
	s.resetLocked()
	if s.phase != phaseLobby || s.slots[0].pl.ready || s.slots[0].pl.navControl || s.slots[0].blocked {
		t.Errorf("reset: phase %s pl %+v blocked %v", s.phase, s.slots[0].pl, s.slots[0].blocked)
	}
	if s.slots[0].pl.vote != "" && s.slots[0].pl.drift == "" {
		t.Error("reset must keep drift default")
	}
}

func TestViewHostAndNavFields(t *testing.T) {
	s := testServer(t)
	s.slots[1].owner = "B"
	s.slots[1].connected = true
	if got := s.hostSlotLocked(); got != 2 {
		t.Errorf("host = %d, want 2 (lowest occupied)", got)
	}
	s.slots[0].owner = "A"
	if got := s.hostSlotLocked(); got != 1 {
		t.Errorf("host = %d, want 1", got)
	}
	v := s.viewLocked(false).(lobbyView)
	if len(v.Players) != 2 || v.Players[0].Label != "P1" || v.HostSlot != 1 {
		t.Errorf("lobby view: %+v", v)
	}
	if nv := s.viewLocked(true).(navLobbyView); len(nv.Players) != 2 {
		t.Errorf("nav view players: %d", len(nv.Players))
	}
}

func writeTemp(t *testing.T, body string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "slots.json")
	if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	return p
}

func TestLoadConfigEmptyPipeAndSlotCap(t *testing.T) {
	cfg, err := loadConfig(writeTemp(t, `{"slots":[
		{"slot":1,"rtpPort":5004,"pipe":"/abs/p1"},
		{"slot":2,"rtpPort":5006,"pipe":""},
		{"slot":3,"rtpPort":5008}]}`))
	if err != nil {
		t.Fatalf("empty pipes rejected: %v", err)
	}
	if cfg.Slots[1].Pipe != "" || cfg.Slots[2].Pipe != "" || cfg.Slots[0].Pipe != "/abs/p1" {
		t.Errorf("pipes after load: %q %q %q", cfg.Slots[0].Pipe, cfg.Slots[1].Pipe, cfg.Slots[2].Pipe)
	}
	var many []string
	for i := 1; i <= 13; i++ {
		many = append(many, `{"slot":`+itoa(i)+`,"rtpPort":`+itoa(5000+2*i)+`}`)
	}
	if _, err := loadConfig(writeTemp(t, `{"slots":[`+strings.Join(many, ",")+`]}`)); err == nil {
		t.Error("13 slots accepted, want at most 12")
	}
	twelve := many[:12]
	if _, err := loadConfig(writeTemp(t, `{"slots":[`+strings.Join(twelve, ",")+`]}`)); err != nil {
		t.Errorf("12 slots rejected: %v", err)
	}
	if _, err := loadConfig(writeTemp(t, `{"slots":[{"slot":1,"rtpPort":5004,"pipe":"/x"},{"slot":2,"rtpPort":5006,"pipe":"/x"}]}`)); err == nil {
		t.Error("two slots on one pipe accepted")
	}
}

func itoa(i int) string { return strconv.Itoa(i) }

// setOccupied marks slot n as held by clientID, connected, optionally ready.
func setOccupied(s *Server, n int, clientID string, ready bool) {
	sl := s.byNum[n]
	sl.owner = clientID
	sl.connected = true
	sl.pl = defaultPlayer()
	sl.pl.name = "P" + itoa(n)
	sl.pl.character = "toad"
	sl.pl.vehicle = "standard-kart-s"
	sl.pl.ready = ready
}

func TestStartLocksReadySlotsAndSitOutsGated(t *testing.T) {
	s := testServer(t)
	s.addSlot(&slot{cfg: SlotConfig{Slot: 3, Label: "P3"}})
	setOccupied(s, 1, "A", true)
	setOccupied(s, 2, "B", false) // sits out
	setOccupied(s, 3, "C", true)
	s.bumpLocked()

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/api/lobby/start", strings.NewReader(`{"clientId":"A"}`))
	s.handleLobbyStart(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("start with 2 of 3 ready: %d %s", rec.Code, rec.Body.String())
	}
	if s.phase != phaseStarting {
		t.Fatalf("phase %s", s.phase)
	}
	if !s.byNum[1].locked || s.byNum[2].locked || !s.byNum[3].locked {
		t.Errorf("locked: 1=%v 2=%v 3=%v, want true false true", s.byNum[1].locked, s.byNum[2].locked, s.byNum[3].locked)
	}
	nv := s.viewLocked(true).(navLobbyView)
	if len(nv.LockedSlots) != 2 || nv.LockedSlots[0] != 1 || nv.LockedSlots[1] != 3 {
		t.Errorf("lockedSlots %v", nv.LockedSlots)
	}
	if !nv.Players[0].Locked || nv.Players[1].Locked {
		t.Errorf("per-player locked flags wrong: %+v", nv.Players)
	}

	// Input: sit-out slot 2 is dropped in starting and live; racers are gated only while starting.
	if !s.inputBlockedLocked(s.byNum[2]) || !s.inputBlockedLocked(s.byNum[1]) {
		t.Error("starting must gate every slot")
	}
	s.setPhaseLiveLocked()
	if s.inputBlockedLocked(s.byNum[1]) || s.inputBlockedLocked(s.byNum[3]) {
		t.Error("live: racers must not be gated")
	}
	if !s.inputBlockedLocked(s.byNum[2]) {
		t.Error("live: sit-out slot 2 must be gated")
	}

	// Late phones are refused; slot holders may still (re)connect.
	if s.joinAllowedLocked("D") {
		t.Error("new phone allowed during live")
	}
	if !s.joinAllowedLocked("A") || !s.joinAllowedLocked("B") {
		t.Error("slot holders refused during live")
	}
	s.phase = phaseStarting
	if s.joinAllowedLocked("D") {
		t.Error("new phone allowed during starting")
	}

	// Reset unlocks everything.
	s.resetLocked()
	if s.byNum[1].locked || s.byNum[3].locked || !s.joinAllowedLocked("D") {
		t.Error("reset did not unlock the room")
	}
}

func TestStartNeedsOneReadyAndHostOnly(t *testing.T) {
	s := testServer(t)
	setOccupied(s, 1, "A", false)
	setOccupied(s, 2, "B", false)
	post := func(id string) int {
		rec := httptest.NewRecorder()
		req := httptest.NewRequest(http.MethodPost, "/api/lobby/start", strings.NewReader(`{"clientId":"`+id+`"}`))
		s.handleLobbyStart(rec, req)
		return rec.Code
	}
	if c := post("B"); c != http.StatusForbidden {
		t.Errorf("non-host start: %d, want 403", c)
	}
	if c := post("A"); c != http.StatusConflict {
		t.Errorf("start with nobody ready: %d, want 409", c)
	}
	s.byNum[2].pl.ready = true
	if c := post("A"); c != http.StatusOK {
		t.Errorf("start with one ready (A not ready): %d, want 200", c)
	}
	if s.byNum[1].locked || !s.byNum[2].locked {
		t.Error("only the ready player should be locked")
	}
}

func TestNavAssignLoopbackAndValidation(t *testing.T) {
	s := testServer(t)
	s.slots[0].cfg.Pipe = "/abs/old"
	call := func(addr, body string) (int, navLobbyView) {
		req := httptest.NewRequest(http.MethodPost, "/api/nav/assign", strings.NewReader(body))
		req.RemoteAddr = addr
		rec := httptest.NewRecorder()
		s.handleNavAssign(rec, req)
		var v navLobbyView
		_ = json.Unmarshal(rec.Body.Bytes(), &v)
		return rec.Code, v
	}
	if c, _ := call("192.168.2.7:5000", `{"slots":[{"slot":1,"pipe":""}]}`); c != http.StatusForbidden {
		t.Errorf("LAN assign: %d, want 403", c)
	}
	if c, _ := call("127.0.0.1:5000", `{"slots":[{"slot":9,"pipe":""}]}`); c != http.StatusBadRequest {
		t.Errorf("unknown slot: %d, want 400", c)
	}
	if c, _ := call("127.0.0.1:5000", `{"slots":[{"slot":1,"pipe":"relative/p1"}]}`); c != http.StatusBadRequest {
		t.Errorf("relative pipe: %d, want 400", c)
	}
	if c, _ := call("127.0.0.1:5000", `{"slots":[{"slot":1,"pipe":"/x/a"},{"slot":2,"pipe":"/x/a"}]}`); c != http.StatusBadRequest {
		t.Errorf("duplicate pipe: %d, want 400", c)
	}
	if c, _ := call("127.0.0.1:5000", `{"slots":[]}`); c != http.StatusBadRequest {
		t.Errorf("empty list: %d, want 400", c)
	}
	c, v := call("127.0.0.1:5000", `{"slots":[{"slot":2,"pipe":"/abs/new","instance":2,"pad":1,"mode":"1p"},{"slot":1,"pipe":""}]}`)
	if c != http.StatusOK {
		t.Fatalf("assign: %d", c)
	}
	if v.Players[0].Pipe != "" || v.Players[1].Pipe != "/abs/new" || v.Players[1].Instance != 2 || v.Players[1].Mode != "1p" {
		t.Errorf("assignment not visible in nav view: %+v", v.Players)
	}
	// Swapping pipes between two slots in one request is fine (they stay unique).
	if c, _ := call("127.0.0.1:5000", `{"slots":[{"slot":1,"pipe":"/abs/new"},{"slot":2,"pipe":"/abs/old"}]}`); c != http.StatusOK {
		t.Errorf("swap pipes: %d, want 200", c)
	}
}

// fifoLines attaches a reader to a FIFO (creating it) and collects the lines.
func fifoLines(t *testing.T, path string) func() []string {
	t.Helper()
	if err := syscall.Mkfifo(path, 0o644); err != nil {
		t.Fatal(err)
	}
	fd, err := syscall.Open(path, syscall.O_RDONLY|syscall.O_NONBLOCK, 0)
	if err != nil {
		t.Fatal(err)
	}
	if err := syscall.SetNonblock(fd, false); err != nil {
		t.Fatal(err)
	}
	f := os.NewFile(uintptr(fd), path)
	var mu sync.Mutex
	var lines []string
	go func() {
		r := bufio.NewReader(f)
		for {
			l, err := r.ReadString('\n')
			if err == io.EOF {
				time.Sleep(5 * time.Millisecond)
				continue
			}
			if err != nil {
				return
			}
			mu.Lock()
			lines = append(lines, strings.TrimRight(l, "\r\n"))
			mu.Unlock()
		}
	}()
	t.Cleanup(func() { _ = f.Close() })
	return func() []string {
		mu.Lock()
		defer mu.Unlock()
		return append([]string(nil), lines...)
	}
}

func waitLine(t *testing.T, get func() []string, want string) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		for _, l := range get() {
			if l == want {
				return
			}
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("line %q never arrived; got %v", want, get())
}

func TestAssignSwitchesPipeAtRuntime(t *testing.T) {
	dir := t.TempDir()
	pathA := filepath.Join(dir, "a")
	pathB := filepath.Join(dir, "b")
	linesA := fifoLines(t, pathA)
	linesB := fifoLines(t, pathB)

	s := testServer(t)
	sl := s.slots[0]
	stop := make(chan struct{})
	t.Cleanup(func() { close(stop) })
	pw := newPipeWriter(pathA, "T", nil)
	go pw.run(stop)
	sl.pipe = pw
	sl.cfg.Pipe = pathA
	sl.owner = "A"
	sl.connected = true
	sl.ctrl = newController(pw.Send)
	t.Cleanup(sl.ctrl.Close)

	sl.ctrl.Handle([]byte(`{"s":1,"x":0.5,"y":0,"b":1}`))
	waitLine(t, linesA, "PRESS A")
	waitLine(t, linesA, "SET MAIN 0.750 0.500")

	req := httptest.NewRequest(http.MethodPost, "/api/nav/assign", strings.NewReader(`{"slots":[{"slot":1,"pipe":"`+pathB+`"}]}`))
	req.RemoteAddr = "127.0.0.1:5000"
	rec := httptest.NewRecorder()
	s.handleNavAssign(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("assign: %d %s", rec.Code, rec.Body.String())
	}
	waitLine(t, linesA, "SET MAIN 0.500 0.500")
	waitLine(t, linesA, "RELEASE A")
	time.Sleep(300 * time.Millisecond) // let the writer switch before the next input

	sl.ctrl.Handle([]byte(`{"s":2,"x":0,"y":0,"b":2}`))
	waitLine(t, linesB, "PRESS B")
	for _, l := range linesA() {
		if l == "PRESS B" {
			t.Error("new input leaked to the old FIFO")
		}
	}

	// Unassign: input is dropped everywhere.
	req = httptest.NewRequest(http.MethodPost, "/api/nav/assign", strings.NewReader(`{"slots":[{"slot":1,"pipe":""}]}`))
	req.RemoteAddr = "[::1]:5000"
	rec = httptest.NewRecorder()
	s.handleNavAssign(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("unassign: %d", rec.Code)
	}
	time.Sleep(300 * time.Millisecond)
	nB := len(linesB())
	sl.ctrl.Handle([]byte(`{"s":3,"x":0,"y":0,"b":0}`))
	sl.ctrl.Handle([]byte(`{"s":4,"x":0,"y":0,"b":1}`))
	time.Sleep(200 * time.Millisecond)
	if got := linesB()[nB:]; len(got) != 0 {
		t.Errorf("unassigned slot still wrote %v to B", got)
	}
}

// navUpdate posts /api/nav/update from addr and returns the status and view.
func navUpdate(t *testing.T, s *Server, addr, body string) (int, navLobbyView) {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, "/api/nav/update", strings.NewReader(body))
	req.RemoteAddr = addr
	rec := httptest.NewRecorder()
	s.handleNavUpdate(rec, req)
	var v navLobbyView
	_ = json.Unmarshal(rec.Body.Bytes(), &v)
	return rec.Code, v
}

// phonePost posts /api/lobby/player as clientID and returns the status and view.
func phonePost(t *testing.T, s *Server, clientID, body string) (int, lobbyView) {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, "/api/lobby/player", strings.NewReader(`{"clientId":"`+clientID+`",`+body+`}`))
	rec := httptest.NewRecorder()
	s.handleLobbyPlayer(rec, req)
	var v lobbyView
	_ = json.Unmarshal(rec.Body.Bytes(), &v)
	return rec.Code, v
}

func TestVoteFieldsWireNames(t *testing.T) {
	s := testServer(t)
	setOccupied(s, 1, "A", true)
	s.bumpLocked()
	raw, err := json.Marshal(s.viewLocked(false))
	if err != nil {
		t.Fatal(err)
	}
	var phone struct {
		Players []map[string]any `json:"players"`
	}
	if err := json.Unmarshal(raw, &phone); err != nil {
		t.Fatal(err)
	}
	for _, k := range []string{"voteOpen", "voteDeadlineMs", "voteLocked"} {
		if _, ok := phone.Players[0][k]; !ok {
			t.Errorf("GET /api/lobby player lacks %q", k)
		}
	}
	if phone.Players[0]["voteOpen"] != false || phone.Players[0]["voteDeadlineMs"] != float64(0) || phone.Players[0]["voteLocked"] != false {
		t.Errorf("vote defaults: %v", phone.Players[0])
	}
	raw, _ = json.Marshal(s.viewLocked(true))
	var nav struct {
		Players []map[string]any `json:"players"`
	}
	_ = json.Unmarshal(raw, &nav)
	for _, k := range []string{"voteOpen", "voteDeadlineMs", "voteLocked", "vote"} {
		if _, ok := nav.Players[0][k]; !ok {
			t.Errorf("GET /api/nav/lobby player lacks %q", k)
		}
	}
}

func TestNavVoteLoopbackAndValidation(t *testing.T) {
	s := testServer(t)
	setOccupied(s, 1, "A", true)
	s.bumpLocked()

	if c, _ := navUpdate(t, s, "192.168.2.7:5000", `{"slots":[{"slot":1,"voteOpen":true}]}`); c != http.StatusForbidden {
		t.Errorf("LAN vote update: %d, want 403", c)
	}
	if s.slots[0].pl.voteOpen {
		t.Error("refused LAN request still changed the vote")
	}
	if c, _ := navUpdate(t, s, "127.0.0.1:5000", `{"slots":[{"slot":1,"voteOpen":true,"voteDeadlineMs":-1}]}`); c != http.StatusBadRequest {
		t.Errorf("negative deadline: %d, want 400", c)
	}
	if s.slots[0].pl.voteOpen || s.slots[0].pl.voteDeadlineMs != 0 {
		t.Error("rejected deadline left partial state behind")
	}
	if c, _ := navUpdate(t, s, "127.0.0.1:5000", `{"slots":[{"slot":9,"voteOpen":true}]}`); c != http.StatusBadRequest {
		t.Errorf("unknown slot: %d, want 400", c)
	}

	deadline := time.Now().Add(30 * time.Second).UnixMilli()
	before := s.version
	c, v := navUpdate(t, s, "[::1]:5000", `{"slots":[{"slot":1,"voteOpen":true,"voteDeadlineMs":`+itoa64(deadline)+`,"voteLocked":false}]}`)
	if c != http.StatusOK {
		t.Fatalf("open vote: %d", c)
	}
	if s.version <= before {
		t.Error("opening a vote did not bump the lobby version")
	}
	if p := v.Players[0]; !p.VoteOpen || p.VoteDeadlineMs != deadline || p.VoteLocked {
		t.Errorf("nav view after open: %+v", p)
	}
	if lv := s.viewLocked(false).(lobbyView); !lv.Players[0].VoteOpen || lv.Players[0].VoteDeadlineMs != deadline {
		t.Errorf("phone view after open: %+v", lv.Players[0])
	}

	// Closing with voteOpen=false keeps the rest of the record; deadline 0 means none.
	before = s.version
	c, v = navUpdate(t, s, "127.0.0.1:5000", `{"slots":[{"slot":1,"voteOpen":false,"voteDeadlineMs":0}]}`)
	if c != http.StatusOK || v.Players[0].VoteOpen || v.Players[0].VoteDeadlineMs != 0 {
		t.Errorf("close vote: %d %+v", c, v.Players[0])
	}
	if s.version <= before {
		t.Error("closing a vote did not bump the lobby version")
	}
}

func itoa64(n int64) string { return strconv.FormatInt(n, 10) }

func TestPhoneVoteLockRules(t *testing.T) {
	s := testServer(t)
	setOccupied(s, 1, "A", true)
	setOccupied(s, 2, "B", true)
	s.bumpLocked()
	s.phase = phaseStarting
	s.slots[0].locked = true
	s.slots[1].locked = true

	lockBody := `"voteLocked":true`
	if c, _ := phonePost(t, s, "A", lockBody); c != http.StatusBadRequest {
		t.Errorf("lock while starting: %d, want 400", c)
	}
	s.phase = phaseLobby
	if c, _ := phonePost(t, s, "A", lockBody); c != http.StatusBadRequest {
		t.Errorf("lock in the lobby: %d, want 400", c)
	}
	if c, _ := phonePost(t, s, "A", `"voteLocked":false`); c != http.StatusBadRequest {
		t.Errorf("unlock in the lobby: %d, want 400", c)
	}
	if s.slots[0].pl.voteLocked {
		t.Error("refused lock changed state")
	}

	s.phase = phaseLive
	before := s.version
	if c, _ := phonePost(t, s, "C", lockBody); c != http.StatusForbidden {
		t.Errorf("lock from a phone with no slot: %d, want 403", c)
	}
	if c, v := phonePost(t, s, "A", lockBody); c != http.StatusOK || !v.Players[0].VoteLocked {
		t.Fatalf("lock in live: %d %+v", c, v.Players[0])
	}
	if s.version <= before {
		t.Error("lock did not bump the lobby version")
	}
	if !s.viewLocked(true).(navLobbyView).Players[0].VoteLocked {
		t.Error("navigator does not see the lock")
	}
	if s.slots[1].pl.voteLocked {
		t.Error("A's lock leaked into slot 2")
	}

	// Vote and lock in one request: the vote is applied, then the lock.
	if c, v := phonePost(t, s, "A", `"vote":"mario-circuit","voteLocked":false`); c != http.StatusOK || v.Players[0].Vote != "mario-circuit" || v.Players[0].VoteLocked {
		t.Errorf("vote then unlock: %d %+v", c, v.Players[0])
	}
	if c, _ := phonePost(t, s, "A", `"voteLocked":true`); c != http.StatusOK {
		t.Errorf("relock: %d", c)
	}

	// Reset clears the vote with the rest of the race state.
	s.resetLocked()
	if p := s.slots[0].pl; p.voteOpen || p.voteLocked || p.voteDeadlineMs != 0 {
		t.Errorf("reset kept vote state: %+v", p)
	}
}

func TestVoteRemainingMs(t *testing.T) {
	now := time.Now()
	if got := voteRemaining(0, now); got != 0 {
		t.Errorf("no deadline: %d, want 0", got)
	}
	if got := voteRemaining(now.Add(-time.Second).UnixMilli(), now); got != 0 {
		t.Errorf("past deadline: %d, want 0", got)
	}
	if got := voteRemaining(now.Add(5*time.Second).UnixMilli(), now); got != 5000 {
		t.Errorf("5 s deadline: %d, want 5000", got)
	}

	s := testServer(t)
	setOccupied(s, 1, "A", true)
	deadline := time.Now().Add(20 * time.Second).UnixMilli()
	s.slots[0].pl.voteOpen = true
	s.slots[0].pl.voteDeadlineMs = deadline
	s.bumpLocked()
	lv := s.viewLocked(false).(lobbyView)
	if r := lv.Players[0].VoteRemainingMs; r <= 0 || r > 20000 {
		t.Errorf("view voteRemainingMs %d, want (0, 20000]", r)
	}
	if lv.Players[0].VoteDeadlineMs != deadline {
		t.Errorf("voteDeadlineMs changed: %d", lv.Players[0].VoteDeadlineMs)
	}
	raw, _ := json.Marshal(lv)
	if !strings.Contains(string(raw), `"voteRemainingMs"`) {
		t.Error("GET /api/lobby JSON lacks voteRemainingMs")
	}
	s.slots[0].pl.voteDeadlineMs = time.Now().Add(-time.Second).UnixMilli()
	if r := s.viewLocked(false).(lobbyView).Players[0].VoteRemainingMs; r != 0 {
		t.Errorf("expired vote remaining %d, want 0", r)
	}
}
