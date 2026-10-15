// Command testclient is an end-to-end check for a running partyd -testpattern.
//
// It uses Pion as the phone (and as a TV viewer) and checks:
//   - RTP video arrives (>100 packets within 5 s of connecting)
//   - input messages become Dolphin FIFO lines in the right order
//   - the 500 ms watchdog and client disconnect both release everything
//   - stale sequence numbers are ignored
//   - slot assignment: distinct slots for simultaneous joins, 409 when full,
//     same clientId gets the same slot back, wantSlot is honoured when free
//   - a slot stays reserved after disconnect and frees afterwards
//   - /api/watch streams the slot without taking it
//   - lobby: catalog, long-poll wake-up, ownership, validation, weight classes,
//     ready rules, host-only start, input gating while starting and while
//     navControl is set, fake navigator to live, reconnect keeps choices,
//     reset (nav and host), loopback-only navigator endpoints
//   - 4-slot config with unassigned slots: POST /api/nav/assign switches a
//     slot's FIFO while it holds input (release on the old FIFO first); a
//     seated but not-ready phone sits out the race (input dropped, no video);
//     late phones get 409 while starting/live and join again after reset
//   - phone stats: POST /api/stats -> jsonl line -> GET /api/stats
//   - ping/pong echo on the "ping" data channel
//   - a phone process killed with SIGKILL: connected=false within 6 s, slot kept,
//     same clientId rejoins the slot (section 7)
//
// Run against: partyd -testpattern -fake-nav -statsdir <dir> on a test port.
// Usage: go run ./cmd/testclient -base http://127.0.0.1:18000 -pipe /abs/path/p1 -statsdir <dir>
package main

import (
	"bufio"
	"bytes"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/pion/ice/v4"
	"github.com/pion/interceptor"
	"github.com/pion/webrtc/v4"
)

var (
	base       = flag.String("base", "http://127.0.0.1:8080", "partyd base URL")
	pipePath   = flag.String("pipe", "", "FIFO that partyd writes slot 1 commands to (created if missing)")
	mdns       = flag.Bool("mdns", true, "advertise host candidates as mDNS names like a browser does")
	freeWait   = flag.Duration("free-wait", 45*time.Second, "max time to wait for a slot to free after disconnect")
	pipe2      = flag.String("pipe2", "", "FIFO for slot 2 (default: p2 next to -pipe)")
	statsDir   = flag.String("statsdir", "", "partyd -statsdir, to check the phonestats jsonl (empty skips the file check)")
	ghostPhone = flag.Bool("ghost-phone", false, "internal: act as the phone that the vanished-phone check kills (section 7)")
)

var failures []string

func check(ok bool, format string, args ...any) {
	msg := fmt.Sprintf(format, args...)
	if ok {
		fmt.Println("  PASS  " + msg)
	} else {
		fmt.Println("  FAIL  " + msg)
		failures = append(failures, msg)
	}
}

func must(err error, what string) {
	if err != nil {
		log.Fatalf("%s: %v", what, err)
	}
}

// ---------- FIFO reader ----------

type lineLog struct {
	mu    sync.Mutex
	lines []string
}

func (l *lineLog) add(s string) {
	l.mu.Lock()
	l.lines = append(l.lines, s)
	l.mu.Unlock()
}

func (l *lineLog) snapshot() []string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return append([]string(nil), l.lines...)
}

// openFIFO attaches a reader without blocking. O_NONBLOCK open of a FIFO
// succeeds immediately, which lets partyd's O_WRONLY|O_NONBLOCK open succeed.
func openFIFO(path string) (*lineLog, error) {
	if _, err := os.Stat(path); os.IsNotExist(err) {
		if err := syscall.Mkfifo(path, 0o644); err != nil {
			return nil, err
		}
	}
	fd, err := syscall.Open(path, syscall.O_RDONLY|syscall.O_NONBLOCK, 0)
	if err != nil {
		return nil, err
	}
	if err := syscall.SetNonblock(fd, false); err != nil {
		return nil, err
	}
	f := os.NewFile(uintptr(fd), path)
	ll := &lineLog{}
	go func() {
		// A FIFO read returns EOF while no writer is attached, so EOF is not
		// the end: keep polling. Holding f in this goroutine keeps the fd open
		// (an unreferenced *os.File would be closed by its finalizer).
		r := bufio.NewReader(f)
		var partial strings.Builder
		for {
			line, err := r.ReadString('\n')
			partial.WriteString(line)
			if err == io.EOF {
				time.Sleep(20 * time.Millisecond)
				continue
			}
			if err != nil {
				return
			}
			ll.add(strings.TrimRight(partial.String(), "\r\n"))
			partial.Reset()
		}
	}()
	return ll, nil
}

// waitSeq waits until want appears as a subsequence of lines[from:]. It returns
// the index just after the last match.
func waitSeq(ll *lineLog, from int, want []string, timeout time.Duration) (int, bool) {
	deadline := time.Now().Add(timeout)
	for {
		lines := ll.snapshot()
		if end, ok := subseq(lines, from, want); ok {
			return end, true
		}
		if time.Now().After(deadline) {
			return from, false
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func subseq(lines []string, from int, want []string) (int, bool) {
	i := from
	for _, w := range want {
		found := false
		for ; i < len(lines); i++ {
			if lines[i] == w {
				found = true
				i++
				break
			}
		}
		if !found {
			return 0, false
		}
	}
	return i, true
}

// ---------- Pion client ----------

type client struct {
	name    string
	id      string
	api     *webrtc.API
	pc      *webrtc.PeerConnection
	dc      *webrtc.DataChannel
	pdc     *webrtc.DataChannel // "ping" echo channel
	pingCh  chan []byte         // echoed ping payloads
	rtp     atomic.Int64
	slot    int
	connCh  chan struct{}
	closeMu sync.Once
	lastErr string // error text from the last refused join
}

func newAPI(mode ice.MulticastDNSMode) (*webrtc.API, error) {
	m := &webrtc.MediaEngine{}
	if err := m.RegisterDefaultCodecs(); err != nil {
		return nil, err
	}
	ir := &interceptor.Registry{}
	if err := webrtc.RegisterDefaultInterceptors(m, ir); err != nil {
		return nil, err
	}
	se := webrtc.SettingEngine{}
	se.SetICEMulticastDNSMode(mode)
	se.SetIncludeLoopbackCandidate(true)
	return webrtc.NewAPI(webrtc.WithMediaEngine(m), webrtc.WithInterceptorRegistry(ir), webrtc.WithSettingEngine(se)), nil
}

func newClient(api *webrtc.API, name, id string) *client {
	return &client{name: name, id: id, api: api, connCh: make(chan struct{})}
}

// offer builds a recvonly video offer. withInput adds the "input" data channel.
func (c *client) offer(withInput bool) (string, error) {
	pc, err := c.api.NewPeerConnection(webrtc.Configuration{})
	if err != nil {
		return "", err
	}
	c.pc = pc
	c.connCh = make(chan struct{})
	c.closeMu = sync.Once{}
	if _, err := pc.AddTransceiverFromKind(webrtc.RTPCodecTypeVideo, webrtc.RTPTransceiverInit{Direction: webrtc.RTPTransceiverDirectionRecvonly}); err != nil {
		return "", err
	}
	pc.OnTrack(func(t *webrtc.TrackRemote, _ *webrtc.RTPReceiver) {
		buf := make([]byte, 1600)
		for {
			if _, _, err := t.Read(buf); err != nil {
				return
			}
			c.rtp.Add(1)
		}
	})
	pc.OnConnectionStateChange(func(s webrtc.PeerConnectionState) {
		if s == webrtc.PeerConnectionStateConnected {
			c.closeMu.Do(func() { close(c.connCh) })
		}
	})
	if withInput {
		f := false
		var zero uint16
		dc, err := pc.CreateDataChannel("input", &webrtc.DataChannelInit{Ordered: &f, MaxRetransmits: &zero})
		if err != nil {
			return "", err
		}
		c.dc = dc
		// Reliable, ordered: the same as the phone's ping channel.
		pdc, err := pc.CreateDataChannel("ping", nil)
		if err != nil {
			return "", err
		}
		c.pdc = pdc
		c.pingCh = make(chan []byte, 16)
		pdc.OnMessage(func(m webrtc.DataChannelMessage) {
			fmt.Printf("DEBUG ping rx client=%s len=%d\n", c.id[:10], len(m.Data))
			select {
			case c.pingCh <- m.Data:
			default:
			}
		})
		pdc.OnOpen(func() { fmt.Printf("DEBUG ping open client=%s\n", c.id[:10]) })
	}
	off, err := pc.CreateOffer(nil)
	if err != nil {
		return "", err
	}
	gathered := webrtc.GatheringCompletePromise(pc)
	if err := pc.SetLocalDescription(off); err != nil {
		return "", err
	}
	select {
	case <-gathered:
	case <-time.After(5 * time.Second):
	}
	return pc.LocalDescription().SDP, nil
}

// closeClient closes the PeerConnection, which is what a phone does when its
// page goes away.
func (c *client) closeClient() {
	if c.pc != nil {
		_ = c.pc.Close()
	}
}

type joinResult struct {
	Slot int    `json:"slot"`
	SDP  string `json:"sdp"`
	Err  string `json:"error"`
}

// postJSON posts v and decodes into out. It returns the HTTP status code.
func postJSON(path string, v, out any) (int, error) {
	b, _ := json.Marshal(v)
	resp, err := http.Post(*base+path, "application/json", bytes.NewReader(b))
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

// join performs the full phone handshake. It returns the HTTP status and slot.
func (c *client) join(wantSlot int) (int, int, error) {
	sdp, err := c.offer(true)
	if err != nil {
		return 0, 0, err
	}
	var res joinResult
	code, err := postJSON("/api/join", map[string]any{"clientId": c.id, "wantSlot": wantSlot, "sdp": sdp}, &res)
	if err != nil {
		return 0, 0, err
	}
	if code != http.StatusOK {
		c.lastErr = res.Err
		return code, 0, nil
	}
	c.slot = res.Slot
	if err := c.pc.SetRemoteDescription(webrtc.SessionDescription{Type: webrtc.SDPTypeAnswer, SDP: res.SDP}); err != nil {
		return code, res.Slot, err
	}
	return code, res.Slot, nil
}

func (c *client) waitConnected(timeout time.Duration) bool {
	select {
	case <-c.connCh:
		return true
	case <-time.After(timeout):
		return false
	}
}

// send writes one input message on the data channel.
func (c *client) send(s int64, x, y float64, b int) {
	msg := fmt.Sprintf(`{"s":%d,"x":%g,"y":%g,"b":%d}`, s, x, y, b)
	if err := c.dc.SendText(msg); err != nil {
		log.Printf("send: %v", err)
	}
}

func slotsState() (map[int]bool, error) {
	resp, err := http.Get(*base + "/api/state")
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	var st struct {
		Slots []struct {
			Slot     int  `json:"slot"`
			Occupied bool `json:"occupied"`
		} `json:"slots"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&st); err != nil {
		return nil, err
	}
	out := map[int]bool{}
	for _, s := range st.Slots {
		out[s.Slot] = s.Occupied
	}
	return out, nil
}

func waitRTP(c *client, n int64, timeout time.Duration) (int64, time.Duration) {
	start := time.Now()
	for time.Since(start) < timeout {
		if c.rtp.Load() > n {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	return c.rtp.Load(), time.Since(start)
}

func main() {
	flag.Parse()
	log.SetFlags(log.Ltime | log.Lmicroseconds)
	if *ghostPhone {
		runGhostPhone()
		return
	}
	if *pipePath == "" {
		log.Fatal("-pipe is required (the FIFO for slot 1)")
	}
	pipeAbs, _ := filepath.Abs(*pipePath)
	lines, err := openFIFO(pipeAbs)
	must(err, "open FIFO "+pipeAbs)
	fmt.Println("FIFO reader attached:", pipeAbs)
	p2Abs := *pipe2
	if p2Abs == "" {
		p2Abs = filepath.Join(filepath.Dir(pipeAbs), "p2")
	}
	lines2, err := openFIFO(p2Abs)
	must(err, "open FIFO "+p2Abs)
	fmt.Println("FIFO reader attached:", p2Abs)

	mode := ice.MulticastDNSModeQueryAndGather
	if !*mdns {
		mode = ice.MulticastDNSModeDisabled
	}
	api, err := newAPI(mode)
	must(err, "pion api")

	// ============ 1. Slot assignment ============
	fmt.Println("\n== 1. Simultaneous joins, 409 when full, same clientId keeps its slot")
	cA := newClient(api, "A", "client-A-0000000000000001")
	cB := newClient(api, "B", "client-B-0000000000000002")
	cC := newClient(api, "C", "client-C-0000000000000003")
	var wg sync.WaitGroup
	var codeA, codeB int
	var slotA, slotB int
	var errA, errB error
	wg.Add(2)
	go func() { defer wg.Done(); codeA, slotA, errA = cA.join(0) }()
	go func() { defer wg.Done(); codeB, slotB, errB = cB.join(0) }()
	wg.Wait()
	check(errA == nil && errB == nil && codeA == 200 && codeB == 200, "both simultaneous joins accepted (codes %d,%d err=%v,%v)", codeA, codeB, errA, errB)
	check(slotA != slotB && slotA >= 1 && slotB >= 1 && slotA <= 2 && slotB <= 2, "simultaneous clients got different slots (A=%d B=%d)", slotA, slotB)
	cA.waitConnected(10 * time.Second)
	cB.waitConnected(10 * time.Second)

	// The config has 4 slots: slots 3 and 4 are free, so two more phones fit and a fifth gets 409.
	cD := newClient(api, "D", "client-D-0000000000000005")
	codeC, slotC3, errC := cC.join(0)
	check(errC == nil && codeC == 200 && slotC3 == 3, "third phone gets slot 3 (slot %d, code %d, err=%v)", slotC3, codeC, errC)
	codeD, slotD4, errD := cD.join(0)
	check(errD == nil && codeD == 200 && slotD4 == 4, "fourth phone gets slot 4 (slot %d, code %d, err=%v)", slotD4, codeD, errD)
	cE := newClient(api, "E", "client-E-0000000000000008")
	codeE, _, errE := cE.join(0)
	check(errE == nil && codeE == http.StatusConflict, "fifth phone gets 409 when all 4 slots are taken (got %d err=%v)", codeE, errE)
	cE.closeClient()
	cC.closeClient()
	cD.closeClient()

	codeA2, slotA2, errA2 := cA.join(0)
	check(errA2 == nil && codeA2 == 200 && slotA2 == slotA, "same clientId reconnect returns same slot (want %d got %d code %d err=%v)", slotA, slotA2, codeA2, errA2)
	check(cA.waitConnected(10*time.Second), "A reconnected and ICE/DTLS came up on the same slot")
	cA.closeClient()
	cB.closeClient()

	// Wait for both slots to free: 20 s after each PeerConnection fails or closes.
	fmt.Println("  waiting for slots to free (server default: 20 s after disconnect)...")
	start := time.Now()
	freed := false
	for time.Since(start) < *freeWait {
		st, err := slotsState()
		if err == nil && allFree(st) {
			freed = true
			break
		}
		time.Sleep(time.Second)
	}
	check(freed, "all slots free again after disconnect (took %s)", time.Since(start).Round(time.Second))

	// ============ 2. Streaming + input on slot 1 ============
	fmt.Println("\n== 2. RTP video and input mapping on slot 1")
	cP := newClient(api, "P", "client-P-0000000000000004")
	before := len(lines.snapshot())
	code, slotP, err := cP.join(0)
	must(err, "join P")
	check(code == 200 && slotP == 1, "lowest free slot assigned to P (slot %d, code %d)", slotP, code)
	check(cP.waitConnected(10*time.Second), "P connected via ICE/DTLS")
	joinAt := time.Now()
	got, _ := waitRTP(cP, 100, 5*time.Second)
	check(got > 100, "received %d RTP video packets within %s of connecting (need >100)", got, time.Since(joinAt).Round(time.Millisecond))

	// Input stream on the data channel: this is what a phone sends.
	// Each step waits for the FIFO lines it should produce.
	cursor := before
	step := func(desc string, send func(), want []string) {
		send()
		end, ok := waitSeq(lines, cursor, want, 2*time.Second)
		if ok {
			cursor = end
		}
		check(ok, "%s -> %v", desc, want)
	}
	cP.send(1, 0, 0, 0)
	time.Sleep(100 * time.Millisecond)
	step("button A down", func() { cP.send(2, 0, 0, 1) }, []string{"PRESS A"})
	step("stick full left-up", func() { cP.send(3, -1, 1, 1) }, []string{"SET MAIN 0.000 1.000"})
	step("A up, stick centre", func() { cP.send(4, 0, 0, 0) }, []string{"SET MAIN 0.500 0.500", "RELEASE A"})
	step("B down, stick full right", func() { cP.send(5, 1, 0, 2) }, []string{"SET MAIN 1.000 0.500", "PRESS B"})

	// Stale sequence number (4 < 5) must be ignored: no RELEASE B.
	n := len(lines.snapshot())
	cP.send(4, 0, 0, 0)
	time.Sleep(150 * time.Millisecond)
	stale := lines.snapshot()[n:]
	check(len(stale) == 0, "stale seq 4 ignored after seq 5 (new lines: %v)", stale)

	// Steady 60 Hz heartbeats with unchanged state must not repeat commands.
	n = len(lines.snapshot())
	for i := 0; i < 18; i++ {
		cP.send(int64(6+i), 1, 0, 2)
		time.Sleep(16 * time.Millisecond)
	}
	dup := lines.snapshot()[n:]
	check(len(dup) == 0, "60 Hz heartbeat with unchanged state sends no duplicate commands (new lines: %v)", dup)

	// Silence for >500 ms: watchdog releases B and centres the stick.
	silentAt := time.Now()
	end, ok := waitSeq(lines, cursor, []string{"SET MAIN 0.500 0.500", "RELEASE B"}, 2*time.Second)
	if ok {
		cursor = end
	}
	check(ok, "watchdog after 500 ms of silence -> SET MAIN 0.500 0.500, RELEASE B (after %s)", time.Since(silentAt).Round(10*time.Millisecond))

	// Input resumes with the button still held: it must be pressed again.
	step("button B pressed again after watchdog", func() { cP.send(40, 1, 0, 2) }, []string{"SET MAIN 1.000 0.500", "PRESS B"})

	// Disconnect: release everything.
	discAt := time.Now()
	cP.closeClient()
	end, ok = waitSeq(lines, cursor, []string{"SET MAIN 0.500 0.500", "RELEASE B"}, 15*time.Second)
	if ok {
		cursor = end
	}
	check(ok, "client disconnect -> SET MAIN 0.500 0.500, RELEASE B (after %s)", time.Since(discAt).Round(10*time.Millisecond))

	// ============ 3. Rejoin in the reserved window; wantSlot ============
	fmt.Println("\n== 3. Rejoin within the reserved window, wantSlot")
	cP2 := &client{name: "P2", id: cP.id, api: api, connCh: make(chan struct{})}
	code, slotP2, err := cP2.join(2) // clientId holds slot 1: wantSlot 2 must be ignored
	must(err, "rejoin P")
	check(code == 200 && slotP2 == 1, "same clientId inside the 20 s window gets slot 1 back even with wantSlot=2 (slot %d, code %d)", slotP2, code)
	check(cP2.waitConnected(10*time.Second), "rejoined P connected")
	cP2.closeClient()
	cP2.closeClient()

	// Viewer: /api/watch streams slot 1 and takes no slot.
	fmt.Println("\n== 4. Viewer (/api/watch) streams slot 1 without taking a slot")
	v := newClient(api, "V", "viewer-0000000000000005")
	stBefore, _ := slotsState()
	vsdp, err := v.offer(false)
	must(err, "viewer offer")
	var vres joinResult
	vcode, err := postJSON("/api/watch", map[string]any{"slot": 1, "sdp": vsdp}, &vres)
	must(err, "watch")
	check(vcode == 200 && vres.SDP != "", "POST /api/watch slot 1 accepted (code %d)", vcode)
	if vcode == 200 {
		must(v.pc.SetRemoteDescription(webrtc.SessionDescription{Type: webrtc.SDPTypeAnswer, SDP: vres.SDP}), "viewer answer")
		got, _ = waitRTP(v, 100, 5*time.Second)
		check(got > 100, "viewer received %d RTP packets (need >100)", got)
	}
	stAfter, _ := slotsState()
	check(fmt.Sprint(stBefore) == fmt.Sprint(stAfter), "viewer did not change slot occupancy (before %v after %v)", stBefore, stAfter)
	v.closeClient()

	// wantSlot honoured when the slot is free: slot 2 is free, so ask for it.
	fmt.Println("\n== 5. wantSlot honoured when free; 409 when every slot is held")
	cX := newClient(api, "X", "client-X-0000000000000006")
	codeX, slotX, errX := cX.join(2)
	check(errX == nil && codeX == 200 && slotX == 2, "wantSlot=2 honoured when slot 2 is free (slot %d, code %d)", slotX, codeX)
	cX.waitConnected(10 * time.Second)
	cY := newClient(api, "Y", "client-Y-0000000000000007")
	codeY, slotY, errY := cY.join(3)
	cZ := newClient(api, "Z", "client-Z-0000000000000009")
	codeZ, slotZ, errZ := cZ.join(4)
	check(errY == nil && codeY == 200 && slotY == 3 && errZ == nil && codeZ == 200 && slotZ == 4,
		"wantSlot 3 and 4 honoured (Y slot %d code %d, Z slot %d code %d)", slotY, codeY, slotZ, codeZ)
	cW := newClient(api, "W", "client-W-000000000000000a")
	codeW, _, errW := cW.join(0)
	check(errW == nil && codeW == http.StatusConflict, "fifth phone gets 409 while all four slots are held (code %d)", codeW)
	cW.closeClient()
	cY.closeClient()
	cZ.closeClient()

	cP.closeClient()
	cX.closeClient()
	v.closeClient()

	p3a := filepath.Join(filepath.Dir(pipeAbs), "p3a")
	p3b := filepath.Join(filepath.Dir(pipeAbs), "p3b")
	lines3a, err := openFIFO(p3a)
	must(err, "open FIFO "+p3a)
	lines3b, err := openFIFO(p3b)
	must(err, "open FIFO "+p3b)
	runLobbyTests(api, lines2Pair{lines, lines2}, lines3Pair{lines3a, lines3b}, p3a, p3b)

	// ============ 7. Killed phone ============
	if waitFree(*freeWait) {
		testVanishedPhone(api)
	} else {
		check(false, "slots free before the vanished-phone check")
	}

	fmt.Println("\nFIFO lines produced for slot 1 (test run):")
	all := lines.snapshot()
	for i, l := range all {
		if i >= 0 {
			fmt.Printf("  %3d  %s\n", i, l)
		}
	}

	fmt.Println()
	if len(failures) == 0 {
		fmt.Println("RESULT: PASS")
		os.Exit(0)
	}
	fmt.Printf("RESULT: FAIL (%d checks)\n", len(failures))
	for _, f := range failures {
		fmt.Println("  -", f)
	}
	os.Exit(1)
}
