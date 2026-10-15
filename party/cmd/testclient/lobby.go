package main

// Lobby, navigator, stats and ping checks. They run after the original join,
// stream and input checks, while the room is still in the lobby.

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/pion/webrtc/v4"
)

const (
	idA = "client-LA-0000000000000011"
	idB = "client-LB-0000000000000012"
	idC = "client-LC-0000000000000014" // slot 3: seated, not ready, sits out
	idD = "client-LD-0000000000000015" // late phone, refused mid-race
	idE = "client-LE-0000000000000016" // late phone, refused while live
	idN = "client-LN-0000000000000013" // never joins: must be refused
)

// lines2Pair and lines3Pair carry the FIFO readers for slots 1-2 and 3.
type lines2Pair struct{ a, b *lineLog }
type lines3Pair struct{ a, b *lineLog }

func contains(lines []string, s string) bool {
	for _, l := range lines {
		if l == s {
			return true
		}
	}
	return false
}

type lobbyPlayer struct {
	Slot       int    `json:"slot"`
	Label      string `json:"label"`
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
	Locked     bool   `json:"locked"`
	Pipe       string `json:"pipe"`
	Instance   int    `json:"instance"`

	VoteOpen        bool  `json:"voteOpen"`
	VoteDeadlineMs  int64 `json:"voteDeadlineMs"`
	VoteRemainingMs int64 `json:"voteRemainingMs"`
	VoteLocked      bool  `json:"voteLocked"`
}

type lobbyState struct {
	Version        uint64        `json:"version"`
	Phase          string        `json:"phase"`
	Message        string        `json:"message"`
	HostSlot       int           `json:"hostSlot"`
	CatalogVersion string        `json:"catalogVersion"`
	LockedSlots    []int         `json:"lockedSlots"`
	Players        []lobbyPlayer `json:"players"`
	Error          string        `json:"error"`
}

func (l lobbyState) player(slot int) lobbyPlayer {
	for _, p := range l.Players {
		if p.Slot == slot {
			return p
		}
	}
	return lobbyPlayer{}
}

// httpDo sends one request. body is JSON-encoded when non-nil; out (if set)
// receives the JSON response, error bodies included.
func httpDo(method, u string, body any, out any) (int, error) {
	var rdr io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return 0, err
		}
		rdr = bytes.NewReader(b)
	}
	req, err := http.NewRequest(method, u, rdr)
	if err != nil {
		return 0, err
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := (&http.Client{Timeout: 40 * time.Second}).Do(req)
	if err != nil {
		return 0, err
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	if out != nil {
		_ = json.Unmarshal(raw, out)
	}
	return resp.StatusCode, nil
}

// lobbyGet is GET /api/lobby?since=N. since=0 returns at once.
func lobbyGet(since uint64) (lobbyState, int, error) {
	var st lobbyState
	code, err := httpDo("GET", fmt.Sprintf("%s/api/lobby?since=%d", *base, since), nil, &st)
	return st, code, err
}

// lobbyPost posts to a lobby endpoint as clientID and returns the status and
// the view (or the error).
func lobbyPost(path, clientID string, body map[string]any) (int, lobbyState) {
	if body == nil {
		body = map[string]any{}
	}
	body["clientId"] = clientID
	var st lobbyState
	code, err := httpDo("POST", *base+path, body, &st)
	if err != nil {
		return 0, lobbyState{Error: err.Error()}
	}
	return code, st
}

// navPost posts to a navigator endpoint (the test client is on loopback).
func navPost(path string, body any) (int, lobbyState) {
	var st lobbyState
	code, err := httpDo("POST", *base+path, body, &st)
	if err != nil {
		return 0, lobbyState{Error: err.Error()}
	}
	return code, st
}

// stableVersion reads the lobby until the version stops moving, so that a
// connection-state bump from a join does not race the next check.
func stableVersion() lobbyState {
	var last lobbyState
	for i := 0; i < 10; i++ {
		st, _, _ := lobbyGet(0)
		if i > 0 && st.Version == last.Version {
			return st
		}
		last = st
		time.Sleep(300 * time.Millisecond)
	}
	return last
}

// waitPhase polls until the phase is want. It returns the state and the
// statuses and messages seen on the way.
func waitPhase(want string, timeout time.Duration) (lobbyState, map[string]bool) {
	seen := map[string]bool{}
	deadline := time.Now().Add(timeout)
	var st lobbyState
	for time.Now().Before(deadline) {
		st, _, _ = lobbyGet(0)
		if st.Message != "" {
			seen["msg: "+st.Message] = true
		}
		for _, p := range st.Players {
			if p.Occupied && p.Status != "" {
				seen[p.Status] = true
			}
		}
		if st.Phase == want {
			return st, seen
		}
		time.Sleep(200 * time.Millisecond)
	}
	return st, seen
}

// allFree reports whether no slot is held.
func allFree(st map[int]bool) bool {
	for _, occ := range st {
		if occ {
			return false
		}
	}
	return true
}

// waitFree waits until no slot is held.
func waitFree(timeout time.Duration) bool {
	start := time.Now()
	for time.Since(start) < timeout {
		st, err := slotsState()
		if err == nil && allFree(st) {
			return true
		}
		time.Sleep(time.Second)
	}
	return false
}

func skip(format string, args ...any) {
	fmt.Println("  SKIP  " + fmt.Sprintf(format, args...))
}

func sortedKeys(m map[string]bool) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// quietFor waits d, then returns the lines written to ll after mark.
func quietFor(ll *lineLog, mark int, d time.Duration) []string {
	time.Sleep(d)
	all := ll.snapshot()
	if mark > len(all) {
		return nil
	}
	return all[mark:]
}

func lanIPv4() []string {
	addrs, _ := net.InterfaceAddrs()
	var out []string
	for _, a := range addrs {
		ipn, ok := a.(*net.IPNet)
		if !ok {
			continue
		}
		ip := ipn.IP.To4()
		if ip == nil || ip.IsLoopback() {
			continue
		}
		out = append(out, ip.String())
	}
	return out
}

// navGetLobby is GET /api/nav/lobby (loopback).
func navGetLobby() (lobbyState, int) {
	var st lobbyState
	code, _ := httpDo("GET", *base+"/api/nav/lobby", nil, &st)
	return st, code
}

func waitOpen(dc *webrtc.DataChannel, timeout time.Duration) bool {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if dc != nil && dc.ReadyState() == webrtc.DataChannelStateOpen {
			return true
		}
		time.Sleep(50 * time.Millisecond)
	}
	return false
}

func newestStatsFile(dir string) (string, error) {
	files, err := filepath.Glob(filepath.Join(dir, "phonestats-*.jsonl"))
	if err != nil {
		return "", err
	}
	if len(files) == 0 {
		return "", fmt.Errorf("no phonestats-*.jsonl in %s", dir)
	}
	sort.Strings(files)
	return files[len(files)-1], nil
}

func runLobbyTests(api *webrtc.API, p12 lines2Pair, p3 lines3Pair, pipe3a, pipe3b string) {
	lines1, lines2 := p12.a, p12.b
	lines3a, lines3b := p3.a, p3.b
	// ============ 6. Catalog and basics ============
	fmt.Println("\n== 6. Catalog and lobby basics")
	check(waitFree(60*time.Second), "all slots are free before the lobby tests")
	var cat struct {
		Characters []json.RawMessage `json:"characters"`
		Vehicles   []json.RawMessage `json:"vehicles"`
		Cups       []json.RawMessage `json:"cups"`
	}
	code, err := httpDo("GET", *base+"/catalog.json", nil, &cat)
	check(err == nil && code == 200 && len(cat.Characters) == 24 && len(cat.Vehicles) == 36 && len(cat.Cups) == 8,
		"GET /catalog.json: 24 characters, 36 vehicles, 8 cups (code %d, err %v)", code, err)
	st, code, _ := lobbyGet(0)
	check(code == 200 && st.Phase == "lobby" && len(st.Players) == 4 && st.CatalogVersion != "" && len(st.LockedSlots) == 0,
		"GET /api/lobby starts in phase lobby with 4 slots, none locked (phase %q, %d slots, catalogVersion %q)", st.Phase, len(st.Players), st.CatalogVersion)

	// ============ 7. Two phones, long-poll ============
	fmt.Println("\n== 7. Two phones join the lobby; long-poll wakes on change")
	cLA := newClient(api, "LA", idA)
	cLB := newClient(api, "LB", idB)
	codeA, slotA, errA := cLA.join(0)
	check(errA == nil && codeA == 200 && slotA == 1, "phone LA joins and gets slot 1 (slot %d, code %d, err %v)", slotA, codeA, errA)
	cLA.waitConnected(10 * time.Second)
	codeB, slotB, errB := cLB.join(0)
	check(errB == nil && codeB == 200 && slotB == 2, "phone LB joins and gets slot 2 (slot %d, code %d, err %v)", slotB, codeB, errB)
	cLB.waitConnected(10 * time.Second)
	st = stableVersion()
	check(st.HostSlot == 1, "host is the lowest occupied slot (hostSlot %d)", st.HostSlot)
	check(st.player(1).Occupied && st.player(2).Occupied && st.player(1).Drift == "auto" && st.player(1).Vote == "random",
		"new occupants start with drift auto and vote random")

	// Slot 3 is unassigned in slots.json. Give it a FIFO at runtime, then a
	// third phone takes it. Its FIFO is switched later while it still holds input.
	code, st = navPost("/api/nav/assign", map[string]any{"slots": []map[string]any{{"slot": 3, "pipe": pipe3a, "instance": 3, "pad": 1, "mode": "1p"}}})
	check(code == 200 && st.player(3).Pipe == pipe3a && st.player(3).Instance == 3,
		"POST /api/nav/assign gives unassigned slot 3 the pipe %s (code %d, pipe %q)", pipe3a, code, st.player(3).Pipe)
	cLC := newClient(api, "LC", idC)
	codeC, slotC, errC := cLC.join(0)
	check(errC == nil && codeC == 200 && slotC == 3, "phone LC joins and gets slot 3 (slot %d, code %d, err %v)", slotC, codeC, errC)
	cLC.waitConnected(10 * time.Second)
	check(waitOpen(cLC.dc, 10*time.Second), "LC's input data channel is open")
	m3a := len(lines3a.snapshot())
	cLC.send(1, 0, 0, 1)
	_, ok := waitSeq(lines3a, m3a, []string{"PRESS A"}, 2*time.Second)
	check(ok, "lobby: LC's input reaches its FIFO (PRESS A on the slot 3 pipe)")
	code, st = navPost("/api/nav/assign", map[string]any{"slots": []map[string]any{{"slot": 3, "pipe": pipe3b}}})
	check(code == 200 && st.player(3).Pipe == pipe3b, "reassign slot 3 while LC holds A: pipe now %s (code %d)", pipe3b, code)
	_, ok = waitSeq(lines3a, m3a, []string{"PRESS A", "RELEASE A"}, 2*time.Second)
	check(ok, "reassign released the held button on the OLD FIFO first (RELEASE A on the old pipe)")
	m3b := len(lines3b.snapshot())
	cLC.send(2, 0, 0, 2)
	_, ok = waitSeq(lines3b, m3b, []string{"PRESS B"}, 2*time.Second)
	check(ok, "after reassign, new input reaches the NEW FIFO (PRESS B on the new pipe)")
	check(!contains(lines3a.snapshot()[m3a:], "PRESS B"), "after reassign, nothing new went to the old FIFO")
	cLC.send(3, 0, 0, 0)
	_, ok = waitSeq(lines3b, m3b, []string{"RELEASE B"}, 2*time.Second)
	check(ok, "after reassign, release reaches the new FIFO (RELEASE B)")

	cur := st.Version
	wake := make(chan lobbyState, 1)
	go func() {
		s, _, _ := lobbyGet(cur)
		wake <- s
	}()
	time.Sleep(300 * time.Millisecond)
	t0 := time.Now()
	lobbyPost("/api/lobby/player", idA, map[string]any{"name": "Ann"})
	select {
	case s := <-wake:
		lat := time.Since(t0)
		check(s.Version > cur && s.player(1).Name == "Ann" && lat < time.Second,
			"long-poll returned the change in %s (need < 1 s)", lat.Round(time.Millisecond))
	case <-time.After(5 * time.Second):
		check(false, "long-poll did not return within 5 s of a change")
	}

	// ============ 8. Validation ============
	fmt.Println("\n== 8. Validation: ownership, ids, weight class, name, ready rules")
	expectPost := func(desc, id string, body map[string]any, want int) lobbyState {
		code, s := lobbyPost("/api/lobby/player", id, body)
		check(code == want, "%s -> %d (want %d) %s", desc, code, want, s.Error)
		return s
	}
	expectPost("phone with no slot (not owner) -> 403", idN, map[string]any{"name": "Eve"}, 403)
	expectPost("unknown character -> 400", idA, map[string]any{"character": "nobody"}, 400)
	expectPost("locked character -> 400", idA, map[string]any{"character": "metal-mario"}, 400)
	expectPost("vehicle before any character -> 400", idA, map[string]any{"vehicle": "standard-kart-s"}, 400)
	expectPost("unknown vehicle -> 400", idA, map[string]any{"character": "toad", "vehicle": "nope"}, 400)
	expectPost("light character + heavy vehicle -> 400", idA, map[string]any{"character": "toad", "vehicle": "standard-kart-l"}, 400)
	expectPost("light character + light vehicle -> 200", idA, map[string]any{"character": "toad", "vehicle": "standard-kart-s"}, 200)
	expectPost("heavy character alone keeps a light vehicle -> 400", idA, map[string]any{"character": "bowser"}, 400)
	st = expectPost("heavy character + heavy vehicle -> 200", idA, map[string]any{"character": "bowser", "vehicle": "standard-kart-l"}, 200)
	check(st.player(1).Character == "bowser" && st.player(1).Vehicle == "standard-kart-l", "A's record holds bowser + standard-kart-l")
	expectPost("bad drift -> 400", idA, map[string]any{"drift": "sloppy"}, 400)
	expectPost("unknown course vote -> 400", idA, map[string]any{"vote": "moon-base"}, 400)
	expectPost("name longer than 10 characters -> 400", idA, map[string]any{"name": "ABCDEFGHIJK"}, 400)
	st = expectPost("set name Ann -> 200", idA, map[string]any{"name": "Ann"}, 200)
	check(st.player(1).Name == "Ann", "A's name is Ann")
	expectPost("B ready with nothing set -> 400", idB, map[string]any{"ready": true}, 400)
	expectPost("B name + character + vehicle + vote -> 200", idB, map[string]any{"name": "Ben", "character": "mario", "vehicle": "standard-kart-m", "vote": "luigi-circuit"}, 200)
	expectPost("B vote back to random -> 200", idB, map[string]any{"vote": "random"}, 200)
	expectPost("B drift manual -> 200", idB, map[string]any{"drift": "manual"}, 200)

	// ============ 9. Start and starting ============
	fmt.Println("\n== 9. Start: host only, everyone ready; starting; input dropped; live")
	code, _ = lobbyPost("/api/lobby/start", idA, nil)
	check(code == 409, "start refused while A is not ready (code %d)", code)
	code, _ = lobbyPost("/api/lobby/start", idB, nil)
	check(code == 403, "start refused for B, who is not the host (code %d)", code)
	expectPost("A ready -> 200", idA, map[string]any{"ready": true}, 200)
	expectPost("B ready -> 200", idB, map[string]any{"ready": true}, 200)
	code, st = lobbyPost("/api/lobby/start", idA, nil)
	check(code == 200 && st.Phase == "starting", "host A starts the race (code %d, phase %q)", code, st.Phase)
	check(len(st.LockedSlots) == 2 && st.LockedSlots[0] == 1 && st.LockedSlots[1] == 2 && st.player(3).Occupied && !st.player(3).Locked,
		"start locks the two ready slots (lockedSlots %v); LC in slot 3 sits out", st.LockedSlots)
	code, _ = lobbyPost("/api/lobby/start", idA, nil)
	check(code == 409, "second start refused while starting (code %d)", code)
	expectPost("vote change while starting -> 409", idA, map[string]any{"vote": "moo-moo-meadows"}, 409)
	expectPost("name change while starting -> 409", idA, map[string]any{"name": "Zed"}, 409)
	expectPost("lock in a vote while starting -> 400", idA, map[string]any{"voteLocked": true}, 400)
	cLD := newClient(api, "LD", idD)
	codeD, _, _ := cLD.join(0)
	check(codeD == http.StatusConflict && strings.Contains(cLD.lastErr, "game in progress"),
		"new phone refused while starting: 409 %q (code %d)", cLD.lastErr, codeD)
	cLD.closeClient()

	// Input is dropped for both slots while starting.
	cLA.send(1000, 0, 0, 0)
	cLB.send(1000, 0, 0, 0)
	time.Sleep(100 * time.Millisecond)
	m1, m2 := len(lines1.snapshot()), len(lines2.snapshot())
	m3 := len(lines3b.snapshot())
	cLA.send(1001, 0, 0, 1)
	cLB.send(1001, 0, 0, 1)
	cLA.send(1002, 0, 1, 1)
	cLB.send(1002, 0, 1, 1)
	cLC.send(1001, 0, 0, 1)
	cLC.send(1002, 0, 1, 1)
	newA := quietFor(lines1, m1, 900*time.Millisecond)
	newB := quietFor(lines2, m2, 0)
	newC := lines3b.snapshot()[m3:]
	check(len(newA) == 0 && len(newB) == 0, "input dropped while starting (slot 1 new lines %v, slot 2 new lines %v)", newA, newB)
	check(len(newC) == 0, "sit-out LC's input dropped while starting (slot 3 new lines %v)", newC)

	st, seen := waitPhase("live", 15*time.Second)
	check(st.Phase == "live", "fake navigator took the room to live (phase %q)", st.Phase)
	statuses := 0
	for k := range seen {
		if len(k) < 5 || k[:5] != "msg: " {
			statuses++
		}
	}
	check(statuses >= 3, "fake navigator walked %d distinct player statuses (%v)", statuses, sortedKeys(seen))
	check(st.player(1).ShowVideo && st.player(2).ShowVideo && !st.player(1).NavControl && !st.player(2).NavControl,
		"live: showVideo set on both slots, navControl cleared (showVideo A=%v B=%v)", st.player(1).ShowVideo, st.player(2).ShowVideo)
	check(!st.player(3).ShowVideo && len(st.LockedSlots) == 2, "live: sit-out slot 3 gets no video (showVideo %v, lockedSlots %v)", st.player(3).ShowVideo, st.LockedSlots)
	m3 = len(lines3b.snapshot())
	cLC.send(1020, 0, 0, 1)
	cLC.send(1021, 0, 0, 0)
	newC = quietFor(lines3b, m3, 600*time.Millisecond)
	check(len(newC) == 0, "live: sit-out LC's input is dropped (slot 3 new lines %v)", newC)
	cLE := newClient(api, "LE", idE)
	codeE2, _, _ := cLE.join(0)
	check(codeE2 == http.StatusConflict && strings.Contains(cLE.lastErr, "game in progress"),
		"new phone refused while live: 409 %q (code %d)", cLE.lastErr, codeE2)
	cLE.closeClient()

	m1 = len(lines1.snapshot())
	cLA.send(1003, 0, 0, 1)
	end, ok := waitSeq(lines1, m1, []string{"PRESS A"}, 2*time.Second)
	check(ok, "after live, slot 1 input flows again (PRESS A)")
	cLA.send(1004, 0, 0, 0)
	_, ok = waitSeq(lines1, end, []string{"RELEASE A"}, 2*time.Second)
	check(ok, "after live, slot 1 release flows (RELEASE A)")
	m2 = len(lines2.snapshot())
	cLB.send(1003, 0, 0, 1)
	_, ok = waitSeq(lines2, m2, []string{"PRESS A"}, 2*time.Second)
	check(ok, "after live, slot 2 input flows (PRESS A on the slot 2 pipe; B button A)")
	cLB.send(1004, 0, 0, 0)

	// ============ 10. Navigator control and loopback ============
	fmt.Println("\n== 10. Navigator: input gated by navControl; invalid transitions; reconnect; reset")
	code, _ = navPost("/api/nav/update", map[string]any{"slots": []map[string]any{{"slot": 1, "navControl": true}}})
	check(code == 200, "nav/update navControl=true on slot 1 (code %d)", code)
	m1 = len(lines1.snapshot())
	cLA.send(1010, 0, 0, 2)
	newA = quietFor(lines1, m1, 600*time.Millisecond)
	check(len(newA) == 0, "slot 1 input dropped while navControl is set (new lines %v)", newA)

	code, _ = navPost("/api/nav/update", map[string]any{"slots": []map[string]any{{"slot": 1, "navControl": false}}})
	check(code == 200, "nav/update navControl=false on slot 1 (code %d)", code)
	m1 = len(lines1.snapshot())
	cLA.send(1011, 0, 0, 2)
	end, ok = waitSeq(lines1, m1, []string{"PRESS B"}, 2*time.Second)
	check(ok, "slot 1 input flows again after navControl cleared (PRESS B)")
	cLA.send(1012, 0, 0, 0)
	_, ok = waitSeq(lines1, end, []string{"RELEASE B"}, 2*time.Second)
	check(ok, "slot 1 release flows (RELEASE B)")

	// Setting navControl while a button is held releases it.
	m1 = len(lines1.snapshot())
	cLA.send(1013, 0, 0, 1)
	end, ok = waitSeq(lines1, m1, []string{"PRESS A"}, 2*time.Second)
	check(ok, "A held (PRESS A) before the navigator takes the slot")
	code, _ = navPost("/api/nav/update", map[string]any{"slots": []map[string]any{{"slot": 1, "navControl": true}}})
	_, ok = waitSeq(lines1, end, []string{"RELEASE A"}, 2*time.Second)
	check(ok && code == 200, "taking the slot (navControl=true) releases the held button (RELEASE A)")
	cLA.send(1014, 0, 0, 1)
	newA = quietFor(lines1, len(lines1.snapshot()), 400*time.Millisecond)
	check(len(newA) == 0, "held button not re-pressed while navControl is set")
	navPost("/api/nav/update", map[string]any{"slots": []map[string]any{{"slot": 1, "navControl": false}}})
	m1 = len(lines1.snapshot())
	cLA.send(1015, 0, 0, 1)
	_, ok = waitSeq(lines1, m1, []string{"PRESS A"}, 2*time.Second)
	check(ok, "button pressed again after navControl is cleared (PRESS A)")
	cLA.send(1016, 0, 0, 0)

	code, _ = navPost("/api/nav/update", map[string]any{"phase": "lobby"})
	check(code == 400, "nav/update phase=lobby refused, use nav/reset (code %d)", code)
	code, _ = navPost("/api/nav/update", map[string]any{"phase": "starting"})
	check(code == 409, "nav/update phase=starting refused while live (code %d)", code)
	code, _ = navPost("/api/nav/update", map[string]any{"phase": "live"})
	check(code == 200, "nav/update phase=live while already live is a no-op (code %d)", code)
	code, _ = navPost("/api/nav/update", map[string]any{"slots": []map[string]any{{"slot": 9, "status": "x"}}})
	check(code == 400, "nav/update with an unknown slot refused (code %d)", code)
	code, st = navPost("/api/nav/update", map[string]any{"slots": []map[string]any{{"slot": 2, "status": "Driving the fake race"}}})
	check(code == 200 && st.player(2).Status == "Driving the fake race", "nav/update sets a slot status (code %d)", code)

	expectPost("vote change while live -> 200", idA, map[string]any{"vote": "luigi-circuit"}, 200)
	expectPost("name change while live -> 409", idA, map[string]any{"name": "Bob"}, 409)

	// Reconnect inside the reserved window keeps the choices and the video flag.
	cLA.closeClient()
	cLA2 := &client{name: "LA2", id: idA, api: api, connCh: make(chan struct{})}
	codeR, slotR, errR := cLA2.join(0)
	check(errR == nil && codeR == 200 && slotR == 1, "phone LA reconnects in the reserved window (slot %d, code %d, err %v)", slotR, codeR, errR)
	check(cLA2.waitConnected(10*time.Second), "phone LA reconnected over ICE/DTLS")
	st = stableVersion()
	check(st.player(1).Name == "Ann" && st.player(1).ShowVideo && st.Phase == "live" && st.player(1).Vote == "luigi-circuit",
		"reconnect keeps name, vote and showVideo (name %q showVideo %v vote %q)", st.player(1).Name, st.player(1).ShowVideo, st.player(1).Vote)
	cLA = cLA2
	st = stableVersion()
	check(st.player(3).Occupied && st.player(3).Connected && len(st.LockedSlots) == 2,
		"sit-out LC stays seated and connected through the race (lockedSlots %v)", st.LockedSlots)

	// ============ 10b. Next-race vote ============
	fmt.Println("\n== 10b. Next-race vote: nav opens it with a deadline; phone votes and locks; nav closes it")
	deadline := time.Now().Add(60 * time.Second).UnixMilli()
	code, nv := navPost("/api/nav/update", map[string]any{"slots": []map[string]any{
		{"slot": 1, "voteLocked": false, "voteOpen": true, "voteDeadlineMs": deadline}}})
	check(code == 200 && nv.player(1).VoteOpen && nv.player(1).VoteDeadlineMs == deadline && !nv.player(1).VoteLocked,
		"nav opens slot 1's vote with a deadline (code %d, voteOpen %v, voteDeadlineMs %d)", code, nv.player(1).VoteOpen, nv.player(1).VoteDeadlineMs)
	pv, _, _ := lobbyGet(0)
	check(pv.player(1).VoteOpen && pv.player(1).VoteDeadlineMs == deadline && !pv.player(1).VoteLocked,
		"phone view (GET /api/lobby) shows voteOpen with the same deadline")
	check(pv.player(1).VoteRemainingMs > 0 && pv.player(1).VoteRemainingMs <= 60000,
		"phone view carries the server-computed voteRemainingMs (%d, about 60000 ms)", pv.player(1).VoteRemainingMs)
	verBefore := pv.Version
	code, pv = lobbyPost("/api/lobby/player", idA, map[string]any{"vote": "mario-circuit"})
	check(code == 200 && pv.player(1).Vote == "mario-circuit" && pv.Version > verBefore,
		"phone A saves a new vote during the open vote (code %d, vote %q, version bumped %v)", code, pv.player(1).Vote, pv.Version > verBefore)
	verBefore = pv.Version
	code, pv = lobbyPost("/api/lobby/player", idA, map[string]any{"voteLocked": true})
	check(code == 200 && pv.player(1).VoteLocked && pv.Version > verBefore,
		"phone A locks in (code %d, voteLocked %v, version bumped %v)", code, pv.player(1).VoteLocked, pv.Version > verBefore)
	nv, _ = navGetLobby()
	check(nv.player(1).VoteOpen && nv.player(1).Vote == "mario-circuit" && nv.player(1).VoteLocked,
		"nav sees the vote and the lock (voteOpen %v, vote %q, voteLocked %v)", nv.player(1).VoteOpen, nv.player(1).Vote, nv.player(1).VoteLocked)
	check(!nv.player(2).VoteLocked, "B has not locked in and nav shows it (voteLocked %v)", nv.player(2).VoteLocked)
	code, nv = navPost("/api/nav/update", map[string]any{"slots": []map[string]any{{"slot": 1, "voteOpen": false}}})
	check(code == 200 && !nv.player(1).VoteOpen, "nav closes the vote (code %d, voteOpen %v)", code, nv.player(1).VoteOpen)
	pv, _, _ = lobbyGet(0)
	check(!pv.player(1).VoteOpen && pv.player(1).VoteLocked && pv.player(1).Vote == "mario-circuit",
		"phone view after close: voteOpen false, the locked vote is kept (voteLocked %v, vote %q)", pv.player(1).VoteLocked, pv.player(1).Vote)

	// Loopback only: the test client is on 127.0.0.1 and ::1, so those must work.
	code, _ = httpDo("GET", *base+"/api/nav/lobby", nil, nil)
	check(code == 200, "GET /api/nav/lobby from 127.0.0.1 accepted (code %d)", code)
	u, _ := url.Parse(*base)
	port := u.Port()
	code, err = httpDo("GET", "http://[::1]:"+port+"/api/nav/lobby", nil, nil)
	if err != nil {
		skip("IPv6 loopback not reachable here (%v)", err)
	} else {
		check(code == 200, "GET /api/nav/lobby from ::1 accepted (code %d)", code)
	}
	ips := lanIPv4()
	if len(ips) == 0 {
		skip("no non-loopback IPv4 address on this machine; the 403 path is covered by the Go unit test")
	}
	for _, ip := range ips {
		client := &http.Client{Timeout: 2 * time.Second}
		resp, err := client.Get("http://" + ip + ":" + port + "/api/nav/lobby")
		if err != nil {
			skip("peer %s cannot reach partyd here (%v); loopback rule covered by the Go unit test", ip, err)
			continue
		}
		resp.Body.Close()
		check(resp.StatusCode == 403, "GET /api/nav/lobby from LAN address %s refused (code %d)", ip, resp.StatusCode)
		break
	}

	code, st = navPost("/api/nav/reset", map[string]any{"message": "Reset by test"})
	check(code == 200 && st.Phase == "lobby" && st.Message == "Reset by test" && !st.player(1).Ready && !st.player(1).ShowVideo,
		"nav/reset: back to lobby, ready and showVideo cleared (phase %q)", st.Phase)

	check(len(st.LockedSlots) == 0 && st.Phase == "lobby", "nav/reset unlocks every slot (lockedSlots %v)", st.LockedSlots)
	cLD2 := newClient(api, "LD2", idD)
	codeD2, slotD2, errD2 := cLD2.join(0)
	check(errD2 == nil && codeD2 == 200 && slotD2 == 4, "after reset a new phone joins the free slot 4 (slot %d, code %d, err %v)", slotD2, codeD2, errD2)
	cLD2.waitConnected(10 * time.Second)

	// ============ 11. Host-only lobby reset after a race ============
	fmt.Println("\n== 11. Start again; only the host can send the room back to the lobby")
	expectPost("A ready again -> 200", idA, map[string]any{"ready": true}, 200)
	expectPost("B ready again -> 200", idB, map[string]any{"ready": true}, 200)
	code, _ = lobbyPost("/api/lobby/start", idA, nil)
	check(code == 200, "host starts the race again (code %d)", code)
	st, _ = waitPhase("live", 15*time.Second)
	check(st.Phase == "live" && len(st.LockedSlots) == 2, "race is live again with 2 racers (phase %q, lockedSlots %v)", st.Phase, st.LockedSlots)
	code, _ = lobbyPost("/api/lobby/reset", idB, nil)
	check(code == 403, "lobby reset refused for B, who is not the host (code %d)", code)
	code, st = lobbyPost("/api/lobby/reset", idA, nil)
	check(code == 200 && st.Phase == "lobby" && !st.player(1).Ready && !st.player(2).Ready,
		"host lobby reset returns to lobby and clears ready (code %d, phase %q)", code, st.Phase)

	// ============ 12. Phone stats and ping ============
	fmt.Println("\n== 12. Phone stats: POST /api/stats -> jsonl line -> GET /api/stats; ping echo")
	fpsSeq := []float64{30, 30, 29.5, 12, 30, 30, 15, 30, 30, 30, 28, 31.25}
	allOK := true
	for i, fps := range fpsSeq {
		body := map[string]any{
			"clientId": idA, "seq": i + 1, "clientT": time.Now().UnixMilli(),
			"fps": fps, "lossPct": float64(i%3) * 0.5, "rttMs": 20.5, "jbMs": 45.0,
			"kbps": 2100.0, "pingMs": 18, "packetsLost": i * 2, "packetsReceived": 1000 + i*100,
			"freezeCount": i / 4, "freezeSec": 0.5 * float64(i/4), "localType": "host", "remoteType": "host",
		}
		c, _ := httpDo("POST", *base+"/api/stats", body, nil)
		if c != 200 {
			allOK = false
		}
		time.Sleep(50 * time.Millisecond)
	}
	check(allOK, "12 stats messages accepted from the slot 1 owner")
	code, _ = httpDo("POST", *base+"/api/stats", map[string]any{"clientId": idN, "seq": 99, "fps": 1}, nil)
	check(code == 403, "stats from a clientId with no slot refused (code %d)", code)

	var sv struct {
		Slots []struct {
			Slot   int            `json:"slot"`
			Stats  map[string]any `json:"stats"`
			Server struct {
				RTPPacketsTotal uint64 `json:"rtpPacketsTotal"`
				TrackWrites     uint64 `json:"trackWrites"`
			} `json:"server"`
		} `json:"slots"`
	}
	code, _ = httpDo("GET", *base+"/api/stats", nil, &sv)
	var s1 struct {
		Slot   int            `json:"slot"`
		Stats  map[string]any `json:"stats"`
		Server struct {
			RTPPacketsTotal uint64 `json:"rtpPacketsTotal"`
			TrackWrites     uint64 `json:"trackWrites"`
		} `json:"server"`
	}
	for _, s := range sv.Slots {
		if s.Slot == 1 {
			s1 = s
		}
	}
	check(code == 200 && s1.Stats != nil && s1.Stats["fps"] == 31.25 && s1.Stats["seq"] == float64(12),
		"GET /api/stats: slot 1 latest is the last sample (fps %v seq %v)", s1.Stats["fps"], s1.Stats["seq"])
	check(s1.Server.RTPPacketsTotal > 1000 && s1.Server.TrackWrites > 0,
		"GET /api/stats: slot 1 server counters show RTP in (%d packets) and track writes (%d)", s1.Server.RTPPacketsTotal, s1.Server.TrackWrites)

	if *statsDir != "" {
		file, err := newestStatsFile(*statsDir)
		check(err == nil, "phonestats jsonl file found (%v)", err)
		if err == nil {
			raw, _ := os.ReadFile(file)
			slot1, withSeq, lastOK := 0, 0, false
			for _, ln := range bytes.Split(bytes.TrimSpace(raw), []byte("\n")) {
				var rec struct {
					ServerMs int64          `json:"serverMs"`
					Slot     int            `json:"slot"`
					Stats    map[string]any `json:"stats"`
					Server   map[string]any `json:"server"`
				}
				if json.Unmarshal(ln, &rec) != nil {
					continue
				}
				if rec.Slot == 1 {
					slot1++
					if _, ok := rec.Stats["seq"]; ok {
						withSeq++
					}
				}
				if rec.Slot == 1 && rec.Stats["seq"] == float64(12) && rec.ServerMs > 0 && rec.Server != nil && rec.Stats["clientId"] == nil {
					lastOK = true
				}
			}
			check(withSeq == 12 && lastOK, "jsonl %s: 12 slot 1 lines, last one carries stats + server counters (slot 1 lines %d, with seq %d)",
				filepath.Base(file), slot1, withSeq)
			fmt.Println("  phonestats file:", file)
		}
	} else {
		skip("jsonl check (pass -statsdir)")
	}

	// Ping echo on the reliable "ping" channel.
	pingOK := waitOpen(cLA.pdc, 10*time.Second)
	fmt.Printf("DEBUG pdc state=%v pingCh=%v client=%s\n", cLA.pdc.ReadyState(), cLA.pingCh != nil, cLA.id[:10])
	check(pingOK, "ping data channel open on the phone side")
	var rtts []time.Duration
	for i := 0; i < 5 && pingOK; i++ {
		t := 12345.25 + float64(i)
		payload := []byte(fmt.Sprintf(`{"t":%g}`, t))
		start := time.Now()
		if err := cLA.pdc.Send(payload); err != nil {
			check(false, "ping send: %v", err)
			break
		}
		select {
		case echo := <-cLA.pingCh:
			rtt := time.Since(start)
			var got struct {
				T float64 `json:"t"`
			}
			_ = json.Unmarshal(echo, &got)
			if got.T != t {
				check(false, "ping echo payload %s, want t=%g", echo, t)
			}
			rtts = append(rtts, rtt)
		case <-time.After(2 * time.Second):
			check(false, "no ping echo within 2 s")
		}
	}
	if len(rtts) > 0 {
		var max time.Duration
		for _, r := range rtts {
			if r > max {
				max = r
			}
		}
		check(len(rtts) == 5 && max < time.Second, "ping echo: %d round trips, slowest %s", len(rtts), max.Round(time.Millisecond))
	}

	cLA.closeClient()
	cLB.closeClient()
	cLC.closeClient()
	cLD2.closeClient()
}
