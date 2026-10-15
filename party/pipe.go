package main

import (
	"errors"
	"log"
	"sync"
	"sync/atomic"
	"syscall"
	"time"
)

// pipeWriter owns the writing side of one Dolphin input FIFO. Callers never
// touch the file: they call Send, which is a non-blocking enqueue. A dedicated
// goroutine does all the opening and writing.
//
// The FIFO is opened O_WRONLY|O_NONBLOCK, which fails with ENXIO until a reader
// (Dolphin) has the pipe open. The goroutine retries the open periodically.
// Lines produced while no reader is attached are dropped; when a reader shows
// up, snapshot() supplies the current held state so Dolphin is brought in sync.
//
// A pipe can be unassigned (empty path): then nothing is ever opened and every
// line is dropped. setPath re-targets the writer at runtime: lines queued
// before the call go to the old FIFO, then the old fd is closed and the new
// path is opened lazily.
type pipeWriter struct {
	label    string
	ch       chan string
	snapshot func() []string
	dropped  atomic.Uint64
	retarget chan struct{} // capacity 1: wakes run when nextPath changes

	mu       sync.Mutex // guards nextPath and applied
	nextPath string
	applied  string // path the run goroutine last switched to

	// Fields below are used only by the run goroutine.
	path   string
	fd     int
	warned bool
}

func newPipeWriter(path, label string, snapshot func() []string) *pipeWriter {
	return &pipeWriter{
		label:    label,
		ch:       make(chan string, 512),
		snapshot: snapshot,
		retarget: make(chan struct{}, 1),
		path:     path,
		nextPath: path,
		applied:  path,
		fd:       -1,
	}
}

// Send enqueues one command line (without the trailing newline). It never blocks.
func (p *pipeWriter) Send(line string) {
	select {
	case p.ch <- line:
	default:
		if n := p.dropped.Add(1); n%100 == 1 {
			log.Printf("[%s] pipe queue full, dropped %d commands", p.label, n)
		}
	}
}

// setPath asks the run goroutine to switch to path ("" = unassigned). It never
// blocks, so it is safe to call while holding Server.mu.
func (p *pipeWriter) setPath(path string) {
	p.mu.Lock()
	p.nextPath = path
	p.mu.Unlock()
	select {
	case p.retarget <- struct{}{}:
	default: // a wake-up is already pending; it will read nextPath
	}
}

// run is the writer goroutine. It returns when stop is closed.
func (p *pipeWriter) run(stop <-chan struct{}) {
	tick := time.NewTicker(200 * time.Millisecond)
	defer tick.Stop()
	defer p.close()
	for {
		select {
		case <-stop:
			return
		case line := <-p.ch:
			if p.fd < 0 && !p.open() {
				continue // no reader (or unassigned): drop, the open-time snapshot will resync
			}
			if err := p.writeLine(line + "\n"); err != nil {
				p.close()
			}
		case <-p.retarget:
			p.switchTo()
		case <-tick.C:
			if p.fd < 0 {
				p.open()
			}
		}
	}
}

// switchTo moves the writer to nextPath. Lines queued before the switch are
// written to the old FIFO first; the new one is opened lazily by the run loop.
func (p *pipeWriter) switchTo() {
	p.mu.Lock()
	next := p.nextPath
	p.mu.Unlock()
	if next == p.path {
		return
	}
	p.drainQueued()
	p.close()
	log.Printf("[%s] pipe: %s -> %s", p.label, orUnassigned(p.path), orUnassigned(next))
	p.path = next
	p.warned = false
	p.mu.Lock()
	p.applied = next
	p.mu.Unlock()
}

// waitApplied blocks until the writer has switched to path, or until a newer
// setPath supersedes it, or until timeout. Used by POST /api/nav/assign after
// it has released Server.mu, so the navigator gets a synchronous hand-over.
func (p *pipeWriter) waitApplied(path string, timeout time.Duration) {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		p.mu.Lock()
		done := p.applied == path || p.nextPath != path
		p.mu.Unlock()
		if done {
			return
		}
		time.Sleep(time.Millisecond)
	}
}

// drainQueued writes everything already queued to the current fd (or drops it
// when no reader is attached).
func (p *pipeWriter) drainQueued() {
	for {
		select {
		case line := <-p.ch:
			if p.fd < 0 {
				continue
			}
			if err := p.writeLine(line + "\n"); err != nil {
				p.close()
			}
		default:
			return
		}
	}
}

func orUnassigned(path string) string {
	if path == "" {
		return "(unassigned)"
	}
	return path
}

// open tries to attach to the FIFO. On success it writes the snapshot lines.
func (p *pipeWriter) open() bool {
	if p.path == "" {
		return false
	}
	fd, err := syscall.Open(p.path, syscall.O_WRONLY|syscall.O_NONBLOCK, 0)
	if err != nil {
		if !p.warned && !errors.Is(err, syscall.ENXIO) {
			log.Printf("[%s] cannot open pipe %s: %v (will keep retrying)", p.label, p.path, err)
			p.warned = true
		}
		return false
	}
	p.fd = fd
	p.warned = false
	log.Printf("[%s] pipe %s: reader attached", p.label, p.path)
	if p.snapshot != nil {
		for _, l := range p.snapshot() {
			if err := p.writeLine(l + "\n"); err != nil {
				p.close()
				return false
			}
		}
	}
	return true
}

func (p *pipeWriter) close() {
	if p.fd >= 0 {
		_ = syscall.Close(p.fd)
		p.fd = -1
	}
}

// writeLine writes a whole line. The fd is non-blocking, so EAGAIN (pipe buffer
// full because the reader is stalled) is retried briefly; this runs on the
// pipe goroutine, never on a WebRTC goroutine.
func (p *pipeWriter) writeLine(s string) error {
	b := []byte(s)
	deadline := time.Now().Add(time.Second)
	for len(b) > 0 {
		n, err := syscall.Write(p.fd, b)
		if n > 0 {
			b = b[n:]
		}
		if err == nil {
			continue
		}
		if errors.Is(err, syscall.EAGAIN) && time.Now().Before(deadline) {
			time.Sleep(time.Millisecond)
			continue
		}
		// EPIPE: reader went away. Anything else: give up on this fd.
		return err
	}
	return nil
}
