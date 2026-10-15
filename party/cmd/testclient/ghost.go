package main

// Vanished-phone check. A phone is a child process that joins, reports
// connected, and then sleeps. The parent sends it SIGKILL, so no WebRTC close
// and no signalling reaches partyd. Within 6 s the slot must show
// connected=false while it stays occupied, and a rejoin with the same clientId
// must get the same slot back, connected again.

import (
	"bufio"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"os"
	"os/exec"
	"strings"
	"syscall"
	"time"

	"github.com/pion/ice/v4"
	"github.com/pion/webrtc/v4"
)

const ghostID = "client-GH-0000000000000021"

// iceMode is the mDNS mode for -mdns.
func iceMode() ice.MulticastDNSMode {
	if !*mdns {
		return ice.MulticastDNSModeDisabled
	}
	return ice.MulticastDNSModeQueryAndGather
}

// runGhostPhone is the child process (-ghost-phone): join as ghostID, print
// "GHOST CONNECTED slot=N" once ICE is up, then wait to be killed.
func runGhostPhone() {
	api, err := newAPI(iceMode())
	must(err, "pion api")
	c := newClient(api, "G", ghostID)
	code, slot, err := c.join(0)
	must(err, "ghost join")
	if code != http.StatusOK {
		log.Fatalf("ghost join refused: code %d (%s)", code, c.lastErr)
	}
	if !c.waitConnected(10 * time.Second) {
		log.Fatal("ghost phone did not connect within 10 s")
	}
	fmt.Printf("GHOST CONNECTED slot=%d\n", slot)
	time.Sleep(time.Hour)
}

type slotConn struct {
	Occupied  bool
	Connected bool
}

// slotConns reads occupied and connected for every slot from GET /api/state.
func slotConns() (map[int]slotConn, error) {
	resp, err := http.Get(*base + "/api/state")
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	var st struct {
		Slots []struct {
			Slot      int  `json:"slot"`
			Occupied  bool `json:"occupied"`
			Connected bool `json:"connected"`
		} `json:"slots"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&st); err != nil {
		return nil, err
	}
	out := map[int]slotConn{}
	for _, s := range st.Slots {
		out[s.Slot] = slotConn{Occupied: s.Occupied, Connected: s.Connected}
	}
	return out, nil
}

// testVanishedPhone runs the SIGKILL check. It needs the free slots of the
// lobby tests to have been released first (the caller waits for that).
func testVanishedPhone(api *webrtc.API) {
	fmt.Println("\n== 7. Killed phone (SIGKILL, no close) shows connected=false within 6 s, keeps its slot; rejoin restores it")
	exe, err := os.Executable()
	must(err, "own executable")
	cmd := exec.Command(exe, "-ghost-phone", "-base", *base, fmt.Sprintf("-mdns=%t", *mdns))
	cmd.Stderr = os.Stderr
	stdout, err := cmd.StdoutPipe()
	must(err, "ghost stdout")
	must(cmd.Start(), "start ghost phone")

	ready := make(chan int, 1)
	go func() {
		sc := bufio.NewScanner(stdout)
		for sc.Scan() {
			line := sc.Text()
			if strings.HasPrefix(line, "GHOST CONNECTED") {
				var s int
				if _, err := fmt.Sscanf(line, "GHOST CONNECTED slot=%d", &s); err == nil {
					select {
					case ready <- s:
					default:
					}
				}
			}
		}
	}()

	var slot int
	select {
	case slot = <-ready:
	case <-time.After(25 * time.Second):
		_ = cmd.Process.Kill()
		_ = cmd.Wait()
		check(false, "ghost phone connected within 25 s")
		return
	}
	check(true, "ghost phone joined slot %d and connected", slot)

	killedAt := time.Now()
	_ = cmd.Process.Signal(syscall.SIGKILL)
	_ = cmd.Wait()

	dropped, held := false, false
	var dropAfter time.Duration
	for time.Since(killedAt) < 6*time.Second {
		st, err := slotConns()
		if err == nil {
			if sc, ok := st[slot]; ok && !sc.Connected {
				dropped, held = true, sc.Occupied
				dropAfter = time.Since(killedAt)
				break
			}
		}
		time.Sleep(100 * time.Millisecond)
	}
	check(dropped, "killed phone shows connected=false within 6 s (after %s)", dropAfter.Round(100*time.Millisecond))
	check(held, "slot %d is still occupied right after the phone vanished", slot)

	time.Sleep(2 * time.Second)
	st, err := slotConns()
	check(err == nil && st[slot].Occupied && !st[slot].Connected, "slot %d still held and connected=false 2 s later", slot)

	// Rejoin with the same clientId: same slot back, connected again.
	c2 := newClient(api, "G2", ghostID)
	code, slot2, err := c2.join(0)
	check(err == nil && code == http.StatusOK && slot2 == slot, "same clientId rejoins its held slot %d (got %d, code %d, err=%v)", slot, slot2, code, err)
	check(c2.waitConnected(10*time.Second), "rejoined ghost connected")
	// The phone side can report connected a few ms before partyd's callback runs.
	for t0 := time.Now(); time.Since(t0) < 2*time.Second; time.Sleep(50 * time.Millisecond) {
		if st, err = slotConns(); err == nil && st[slot].Connected {
			break
		}
	}
	check(err == nil && st[slot].Connected && st[slot].Occupied, "slot %d shows connected=true again after rejoin", slot)
	c2.closeClient()
}
