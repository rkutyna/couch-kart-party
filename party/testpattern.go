package main

import (
	"context"
	"fmt"
	"log"
	"os"
	"os/exec"
	"sync"
	"time"
)

// startTestPattern runs one ffmpeg test-pattern sender per slot. The senders are
// restarted if they exit on their own, and they stop when ctx is cancelled.
// The returned WaitGroup completes once every child has exited.
func startTestPattern(ctx context.Context, ffmpeg string, slots []SlotConfig) *sync.WaitGroup {
	var wg sync.WaitGroup
	for _, s := range slots {
		wg.Add(1)
		go func(s SlotConfig) {
			defer wg.Done()
			runTestPattern(ctx, ffmpeg, s)
		}(s)
	}
	return &wg
}

func runTestPattern(ctx context.Context, ffmpeg string, s SlotConfig) {
	args := []string{
		"-hide_banner", "-loglevel", "error",
		"-re", "-f", "lavfi", "-i", "testsrc2=size=640x360:rate=60",
		"-c:v", "h264_videotoolbox", "-realtime", "1",
		"-profile:v", "baseline", "-bf", "0", "-g", "30", "-b:v", "2M",
		"-f", "rtp", "-payload_type", "96",
		fmt.Sprintf("rtp://127.0.0.1:%d?pkt_size=1200", s.RTPPort),
	}
	for {
		cmd := exec.CommandContext(ctx, ffmpeg, args...)
		cmd.Stdout = nil
		cmd.Stderr = os.Stderr
		log.Printf("[%s] starting test pattern: %s %v", s.Label, ffmpeg, args)
		err := cmd.Run()
		if ctx.Err() != nil {
			return
		}
		log.Printf("[%s] ffmpeg exited (%v); restarting in 2s", s.Label, err)
		select {
		case <-ctx.Done():
			return
		case <-time.After(2 * time.Second):
		}
	}
}
