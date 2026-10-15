// Command statsreport summarises phonestats-*.jsonl files written by partyd.
// For each slot it prints the duration, fps (median, p95, min), freezes, the
// share of seconds below 20 fps, packet loss, jitter buffer, round trip and
// input ping (median, p95), median bitrate, and the worst seconds.
//
// Percentiles are nearest-rank. Counters that go down (a peer connection was
// rebuilt) count from zero again. "Worst" orders by fps ascending, then loss
// descending; seconds without an fps value come last.
//
// Usage: go run ./cmd/statsreport [-worst 10] phonestats-20261013-162550.jsonl ...
package main

import (
	"bufio"
	"encoding/json"
	"flag"
	"fmt"
	"log"
	"math"
	"os"
	"sort"
	"strings"
	"time"
)

type line struct {
	ServerMs int64          `json:"serverMs"`
	Slot     int            `json:"slot"`
	Stats    map[string]any `json:"stats"`
}

// sample is one second from one phone.
type sample struct {
	t          time.Time
	fps        *float64
	lossPct    *float64
	rtt        *float64
	ping       *float64
	jb         *float64
	kbps       *float64
	freezeN    float64 // cumulative
	freezeS    float64 // cumulative
	lost       float64 // cumulative
	recv       float64 // cumulative
	hasCounter bool
}

var worstN = flag.Int("worst", 10, "how many worst seconds to list")

func num(m map[string]any, key string) *float64 {
	v, ok := m[key].(float64)
	if !ok {
		return nil
	}
	return &v
}

func cum(m map[string]any, key string) (float64, bool) {
	v, ok := m[key].(float64)
	return v, ok
}

// pct returns the nearest-rank percentile p (0..1) of vs (already sorted).
func pct(vs []float64, p float64) float64 {
	if len(vs) == 0 {
		return math.NaN()
	}
	i := int(math.Ceil(p*float64(len(vs)))) - 1
	if i < 0 {
		i = 0
	}
	if i >= len(vs) {
		i = len(vs) - 1
	}
	return vs[i]
}

func collect(vals []*float64) []float64 {
	var out []float64
	for _, v := range vals {
		if v != nil {
			out = append(out, *v)
		}
	}
	sort.Float64s(out)
	return out
}

func fmtNum(v float64) string {
	if math.IsNaN(v) {
		return "-"
	}
	return fmt.Sprintf("%.1f", v)
}

// deltaTotal sums per-sample increases of a cumulative counter; a decrease
// means the counter restarted from zero.
func deltaTotal(xs []float64) float64 {
	var total float64
	for i, x := range xs {
		if i == 0 {
			total += x
			continue
		}
		if x < xs[i-1] {
			total += x
		} else {
			total += x - xs[i-1]
		}
	}
	return total
}

func report(path string) error {
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	defer f.Close()
	bySlot := map[int][]sample{}
	bad := 0
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 1<<20), 1<<20)
	for sc.Scan() {
		raw := strings.TrimSpace(sc.Text())
		if raw == "" {
			continue
		}
		var l line
		if err := json.Unmarshal([]byte(raw), &l); err != nil || l.Stats == nil {
			bad++
			continue
		}
		m := l.Stats
		s := sample{
			t:       time.UnixMilli(l.ServerMs),
			fps:     num(m, "fps"),
			lossPct: num(m, "lossPct"),
			rtt:     num(m, "rttMs"),
			ping:    num(m, "pingMs"),
			jb:      num(m, "jbMs"),
			kbps:    num(m, "kbps"),
		}
		s.freezeN, _ = cum(m, "freezeCount")
		s.freezeS, _ = cum(m, "freezeSec")
		s.lost, _ = cum(m, "packetsLost")
		s.recv, _ = cum(m, "packetsReceived")
		s.hasCounter = true
		bySlot[l.Slot] = append(bySlot[l.Slot], s)
	}
	if err := sc.Err(); err != nil {
		return err
	}
	fmt.Printf("== %s\n", path)
	if bad > 0 {
		fmt.Printf("   (%d unreadable lines skipped)\n", bad)
	}
	if len(bySlot) == 0 {
		fmt.Println("   no stats lines")
		return nil
	}
	slots := make([]int, 0, len(bySlot))
	for k := range bySlot {
		slots = append(slots, k)
	}
	sort.Ints(slots)
	for _, slotNo := range slots {
		ss := bySlot[slotNo]
		sort.Slice(ss, func(i, j int) bool { return ss[i].t.Before(ss[j].t) })
		printSlot(slotNo, ss)
	}
	return nil
}

func printSlot(slotNo int, ss []sample) {
	first, last := ss[0].t, ss[len(ss)-1].t
	fmt.Printf("\nslot %d   samples %d   duration %.0f s   %s to %s\n",
		slotNo, len(ss), last.Sub(first).Seconds()+1, first.Format("15:04:05"), last.Format("15:04:05"))

	var fpsVals, jbVals, rttVals, pingVals, kbpsVals []*float64
	var freezeN, freezeS, lost, recv []float64
	below20, withFps := 0, 0
	for _, s := range ss {
		fpsVals = append(fpsVals, s.fps)
		jbVals = append(jbVals, s.jb)
		rttVals = append(rttVals, s.rtt)
		pingVals = append(pingVals, s.ping)
		kbpsVals = append(kbpsVals, s.kbps)
		if s.fps != nil {
			withFps++
			if *s.fps < 20 {
				below20++
			}
		}
		freezeN = append(freezeN, s.freezeN)
		freezeS = append(freezeS, s.freezeS)
		lost = append(lost, s.lost)
		recv = append(recv, s.recv)
	}
	fps := collect(fpsVals)
	jb := collect(jbVals)
	rtt := collect(rttVals)
	ping := collect(pingVals)
	kbps := collect(kbpsVals)

	fmt.Printf("  fps            median %s   p95 %s   min %s\n",
		fmtNum(pct(fps, 0.5)), fmtNum(pct(fps, 0.95)), fmtNum(first2(fps)))
	fmt.Printf("  seconds < 20fps  %d of %d (%.1f%%)\n", below20, withFps, pctOf(below20, withFps))
	fmt.Printf("  freezes        %.0f   freeze time %.1f s\n", deltaTotal(freezeN), deltaTotal(freezeS))

	lostTotal, recvTotal := deltaTotal(lost), deltaTotal(recv)
	lossPct := math.NaN()
	if lostTotal+recvTotal > 0 {
		lossPct = 100 * lostTotal / (lostTotal + recvTotal)
	}
	fmt.Printf("  packet loss    %s%%   (%.0f lost of %.0f)\n", fmtNum(lossPct), lostTotal, lostTotal+recvTotal)
	fmt.Printf("  jitter buffer  median %s ms   p95 %s ms\n", fmtNum(pct(jb, 0.5)), fmtNum(pct(jb, 0.95)))
	fmt.Printf("  RTT            median %s ms   p95 %s ms\n", fmtNum(pct(rtt, 0.5)), fmtNum(pct(rtt, 0.95)))
	fmt.Printf("  input ping     median %s ms   p95 %s ms\n", fmtNum(pct(ping, 0.5)), fmtNum(pct(ping, 0.95)))
	fmt.Printf("  bitrate        median %s kbps\n", fmtNum(pct(kbps, 0.5)))

	worst := append([]sample(nil), ss...)
	sort.SliceStable(worst, func(i, j int) bool {
		a, b := worst[i], worst[j]
		if (a.fps == nil) != (b.fps == nil) {
			return a.fps != nil // unknown fps last
		}
		if a.fps != nil && *a.fps != *b.fps {
			return *a.fps < *b.fps
		}
		return deref(a.lossPct) > deref(b.lossPct)
	})
	n := *worstN
	if n > len(worst) {
		n = len(worst)
	}
	fmt.Printf("  worst %d seconds:\n", n)
	fmt.Printf("    %-10s %6s %8s %8s\n", "time", "fps", "loss%", "rtt ms")
	for _, s := range worst[:n] {
		fmt.Printf("    %-10s %6s %8s %8s\n", s.t.Format("15:04:05"), fmtPtr(s.fps), fmtPtr(s.lossPct), fmtPtr(s.rtt))
	}
	fmt.Println()
}

func first2(v []float64) float64 {
	if len(v) == 0 {
		return math.NaN()
	}
	return v[0]
}

func pctOf(a, b int) float64 {
	if b == 0 {
		return 0
	}
	return 100 * float64(a) / float64(b)
}

func deref(p *float64) float64 {
	if p == nil {
		return -1
	}
	return *p
}

func fmtPtr(p *float64) string {
	if p == nil {
		return "-"
	}
	return fmt.Sprintf("%.1f", *p)
}

func main() {
	flag.Usage = func() {
		fmt.Fprintln(os.Stderr, "usage: statsreport [-worst N] phonestats-*.jsonl ...")
		flag.PrintDefaults()
	}
	flag.Parse()
	if flag.NArg() == 0 {
		flag.Usage()
		os.Exit(2)
	}
	for _, p := range flag.Args() {
		if err := report(p); err != nil {
			log.Printf("%s: %v", p, err)
			os.Exit(1)
		}
	}
}
