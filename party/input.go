package main

import (
	"encoding/json"
	"fmt"
	"math"
	"strconv"
	"sync"
	"time"
)

// watchdogTimeout: if no input message arrives for this long, every button is
// released and the stick is centred.
const watchdogTimeout = 500 * time.Millisecond

// stickEpsilon: a stick change smaller than this (in raw -1..1 units) is not sent.
const stickEpsilon = 0.01

type buttonBit struct {
	bit  int
	name string
}

// buttonBits in order; the order also defines the order of RELEASE/PRESS lines.
var buttonBits = []buttonBit{
	{1, "A"}, {2, "B"}, {4, "X"}, {8, "Y"}, {16, "Z"}, {32, "START"},
	{64, "L"}, {128, "R"}, {256, "D_UP"}, {512, "D_DOWN"}, {1024, "D_LEFT"}, {2048, "D_RIGHT"},
}

// inputMsg is one data-channel message from a phone.
type inputMsg struct {
	S int64   `json:"s"`
	X float64 `json:"x"`
	Y float64 `json:"y"`
	B int     `json:"b"`
}

// controller turns phone input messages into Dolphin pipe commands for one slot.
// It keeps the emitted state so it only writes transitions.
type controller struct {
	sink func(line string) // non-blocking enqueue to the pipe

	mu       sync.Mutex
	haveSeq  bool
	lastSeq  int64
	lastRecv time.Time
	idle     bool // watchdog already fired; cleared by the next message
	closed   bool // Close was called; ignore everything from here on
	// Desired state (last accepted message).
	x, y float64
	b    int
	// Emitted state (what Dolphin has been told).
	ex, ey float64
	eb     int

	done chan struct{}
	once sync.Once
}

func newController(sink func(string)) *controller {
	c := &controller{sink: sink, lastRecv: time.Now(), done: make(chan struct{})}
	go c.watchdog()
	return c
}

// stickToPipe maps -1..1 to the 0..1 range Dolphin's SET MAIN expects.
func stickToPipe(v float64) float64 {
	out := 0.5 + 0.5*clamp(v, -1, 1)
	return clamp(out, 0, 1)
}

func clamp(v, lo, hi float64) float64 {
	return math.Max(lo, math.Min(hi, v))
}

func fmtPipe(v float64) string {
	return strconv.FormatFloat(v, 'f', 3, 64)
}

// Handle processes one raw data-channel message. Out-of-order (or duplicate)
// sequence numbers are ignored; any well-formed message counts as liveness.
func (c *controller) Handle(raw []byte) {
	var m inputMsg
	if err := json.Unmarshal(raw, &m); err != nil {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.closed {
		return
	}
	c.lastRecv = time.Now()
	c.idle = false
	if c.haveSeq && m.S <= c.lastSeq {
		return
	}
	c.haveSeq = true
	c.lastSeq = m.S
	c.x = clamp(m.X, -1, 1)
	c.y = clamp(m.Y, -1, 1)
	c.b = m.B
	c.emitLocked()
}

// emitLocked writes the commands needed to move from the emitted state to the
// desired state. Caller holds c.mu.
func (c *controller) emitLocked() {
	if math.Abs(c.x-c.ex) > stickEpsilon || math.Abs(c.y-c.ey) > stickEpsilon {
		c.sink(fmt.Sprintf("SET MAIN %s %s", fmtPipe(stickToPipe(c.x)), fmtPipe(stickToPipe(c.y))))
		c.ex, c.ey = c.x, c.y
	}
	changed := c.b ^ c.eb
	for _, bb := range buttonBits {
		if changed&bb.bit == 0 {
			continue
		}
		if c.b&bb.bit != 0 {
			c.sink("PRESS " + bb.name)
		} else {
			c.sink("RELEASE " + bb.name)
		}
	}
	c.eb = c.b
}

// releaseLocked centres the stick and releases every button that is down.
// Caller holds c.mu. The desired state is reset too, so a later message that
// still holds a button presses it again.
func (c *controller) releaseLocked() {
	if c.ex != 0 || c.ey != 0 {
		c.sink(fmt.Sprintf("SET MAIN %s %s", fmtPipe(0.5), fmtPipe(0.5)))
	}
	for _, bb := range buttonBits {
		if c.eb&bb.bit != 0 {
			c.sink("RELEASE " + bb.name)
		}
	}
	c.x, c.y, c.b = 0, 0, 0
	c.ex, c.ey, c.eb = 0, 0, 0
}

// Release is called when the phone goes away.
func (c *controller) Release() {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.closed {
		return
	}
	c.releaseLocked()
	c.idle = true
}

// Close stops the watchdog and releases everything. Idempotent.
func (c *controller) Close() {
	c.once.Do(func() {
		c.mu.Lock()
		c.releaseLocked()
		c.closed = true
		c.mu.Unlock()
		close(c.done)
	})
}

// snapshot returns the lines that bring Dolphin to the current emitted state.
// Used when the pipe (re)attaches. Empty when the controller is neutral.
func (c *controller) snapshot() []string {
	c.mu.Lock()
	defer c.mu.Unlock()
	var out []string
	if c.ex != 0 || c.ey != 0 {
		out = append(out, fmt.Sprintf("SET MAIN %s %s", fmtPipe(stickToPipe(c.ex)), fmtPipe(stickToPipe(c.ey))))
	}
	for _, bb := range buttonBits {
		if c.eb&bb.bit != 0 {
			out = append(out, "PRESS "+bb.name)
		}
	}
	return out
}

func (c *controller) watchdog() {
	t := time.NewTicker(50 * time.Millisecond)
	defer t.Stop()
	for {
		select {
		case <-c.done:
			return
		case <-t.C:
			c.mu.Lock()
			if !c.idle && time.Since(c.lastRecv) > watchdogTimeout {
				c.idle = true
				c.releaseLocked()
			}
			c.mu.Unlock()
		}
	}
}
