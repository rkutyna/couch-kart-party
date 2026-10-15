// Command partyd serves the party web client, relays one H.264 RTP stream per
// player slot to phones over WebRTC, and turns phone input into Dolphin FIFO
// commands.
package main

import (
	"context"
	"crypto/tls"
	"errors"
	"flag"
	"fmt"
	"log"
	"net"
	"net/http"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"time"
)

func main() {
	cfgPath := flag.String("config", "slots.json", "slot configuration file")
	httpAddr := flag.String("http", ":8080", "plain HTTP listen address (dual-stack)")
	httpsAddr := flag.String("https", "", "optional HTTPS listen address, e.g. :8443 (needs -tls-cert and -tls-key)")
	tlsCert := flag.String("tls-cert", "", "TLS certificate PEM for -https")
	tlsKey := flag.String("tls-key", "", "TLS private key PEM for -https")
	testPattern := flag.Bool("testpattern", false, "spawn an ffmpeg test-pattern sender per slot")
	ffmpegBin := flag.String("ffmpeg", defaultFFmpeg(), "ffmpeg binary for -testpattern")
	webDir := flag.String("webdir", "", "serve client files from this directory instead of the embedded copy")
	freeDelay := flag.Duration("free-delay", 20*time.Second, "how long a slot stays reserved after its phone disconnects")
	publicURL := flag.String("public-url", "", "preferred phone URL, listed first in /api/joininfo and used for the TV QR code, e.g. https://party.local:8443/")
	fakeNav := flag.Bool("fake-nav", false, "simulate the navigator: starting the race walks statuses for ~6 s, then goes live")
	statsDir := flag.String("statsdir", ".", "directory for phonestats-<start>.jsonl (created if missing)")
	pprofAddr := flag.String("pprof", "", "serve Go runtime profiles on this loopback address, e.g. 127.0.0.1:6060 (off when empty)")
	flag.Parse()
	if *pprofAddr != "" {
		startPprof(*pprofAddr)
	}

	if *httpsAddr != "" && (*tlsCert == "" || *tlsKey == "") {
		log.Fatal("-https needs both -tls-cert and -tls-key")
	}
	join, err := newJoinSettings(*publicURL)
	if err != nil {
		log.Fatal(err)
	}

	cfg, err := loadConfig(*cfgPath)
	if err != nil {
		log.Fatalf("config: %v", err)
	}
	web, err := webFS(*webDir)
	if err != nil {
		log.Fatalf("web assets: %v", err)
	}
	catalog, catVer, err := loadCatalog(web)
	if err != nil {
		log.Fatalf("catalog: %v", err)
	}
	log.Printf("catalog %s: %d characters, %d vehicles, %d courses", catVer, len(catalog.Characters), len(catalog.Vehicles), len(catalog.courses))
	api, err := newAPI()
	if err != nil {
		log.Fatalf("webrtc: %v", err)
	}

	stop := make(chan struct{})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	srv := newServer(api, *freeDelay)
	srv.catalog = catalog
	srv.catalogVersion = catVer
	srv.fakeNav = *fakeNav
	srv.joinCfg = join
	started := time.Now()
	sl, err := openStatsLog(*statsDir, started)
	if err != nil {
		log.Fatalf("stats log in %s: %v", *statsDir, err)
	}
	srv.statsLog = sl
	defer sl.close()
	log.Printf("phone stats: %s", sl.path)
	if *fakeNav {
		log.Printf("fake navigator enabled (-fake-nav)")
	}
	go srv.runRollup(stop)

	for _, sc := range cfg.Slots {
		sc := sc
		track, err := newTrack(sc.Slot)
		if err != nil {
			log.Fatalf("slot %d track: %v", sc.Slot, err)
		}
		ctr := &slotCounters{}
		if err := rtpIngest(sc.RTPPort, track, sc.Label, stop, ctr); err != nil {
			log.Fatalf("slot %d: listen UDP 127.0.0.1:%d: %v", sc.Slot, sc.RTPPort, err)
		}
		slotRef := &slot{cfg: sc, track: track, ctr: ctr}
		slotRef.pipe = newPipeWriter(sc.Pipe, sc.Label, func() []string {
			srv.mu.Lock()
			c := slotRef.ctrl
			srv.mu.Unlock()
			if c == nil {
				return nil
			}
			return c.snapshot()
		})
		go slotRef.pipe.run(stop)
		srv.addSlot(slotRef)
		log.Printf("[%s] slot %d: rtp udp/%d -> pipe %s", sc.Label, sc.Slot, sc.RTPPort, orUnassigned(sc.Pipe))
	}

	for _, src := range cfg.Sources {
		if err := srv.sourceIngest(src, stop); err != nil {
			log.Fatalf("source i%d pad %d: listen UDP 127.0.0.1:%d: %v", src.Instance, src.Pad, src.Port, err)
		}
	}
	if len(cfg.Sources) > 0 {
		log.Printf("%d instance video sources (ports %d..%d)", len(cfg.Sources), cfg.Sources[0].Port, cfg.Sources[len(cfg.Sources)-1].Port)
	}

	var ffWG *sync.WaitGroup
	if *testPattern {
		ffWG = startTestPattern(ctx, *ffmpegBin, cfg.Slots)
	}

	handler := srv.handler(web)
	httpSrv := &http.Server{Handler: handler, ReadHeaderTimeout: 10 * time.Second}
	ln, err := net.Listen("tcp", *httpAddr)
	if err != nil {
		log.Fatalf("listen %s: %v", *httpAddr, err)
	}
	join.setListener(false, ln.Addr())
	log.Printf("serving http on %s", ln.Addr())
	go func() {
		if err := httpSrv.Serve(ln); err != nil && !errors.Is(err, http.ErrServerClosed) {
			log.Fatalf("http: %v", err)
		}
	}()

	var httpsSrv *http.Server
	if *httpsAddr != "" {
		cert, err := tls.LoadX509KeyPair(*tlsCert, *tlsKey)
		if err != nil {
			log.Fatalf("tls: %v", err)
		}
		tlsLn, err := net.Listen("tcp", *httpsAddr)
		if err != nil {
			log.Fatalf("listen %s: %v", *httpsAddr, err)
		}
		httpsSrv = &http.Server{Handler: handler, ReadHeaderTimeout: 10 * time.Second,
			TLSConfig: &tls.Config{Certificates: []tls.Certificate{cert}, MinVersion: tls.VersionTLS12}}
		join.setListener(true, tlsLn.Addr())
		log.Printf("serving https on %s", tlsLn.Addr())
		go func() {
			if err := httpsSrv.ServeTLS(tlsLn, "", ""); err != nil && !errors.Is(err, http.ErrServerClosed) {
				log.Fatalf("https: %v", err)
			}
		}()
	}

	sigCtx, sigStop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer sigStop()
	<-sigCtx.Done()
	log.Printf("shutting down")

	close(stop)
	srv.closeAll()
	shutdownCtx, c2 := context.WithTimeout(context.Background(), 3*time.Second)
	defer c2()
	_ = httpSrv.Shutdown(shutdownCtx)
	if httpsSrv != nil {
		_ = httpsSrv.Shutdown(shutdownCtx)
	}
	cancel() // kills ffmpeg children
	if ffWG != nil {
		done := make(chan struct{})
		go func() { ffWG.Wait(); close(done) }()
		select {
		case <-done:
		case <-time.After(3 * time.Second):
			log.Printf("ffmpeg children still exiting")
		}
	}
	fmt.Fprintln(os.Stderr, "bye")
}

func defaultFFmpeg() string {
	if p, err := exec.LookPath("ffmpeg"); err == nil {
		return p
	}
	// Not on PATH (e.g. started from launchd): try the Homebrew prefix.
	prefix := os.Getenv("HOMEBREW_PREFIX")
	if prefix == "" {
		if out, err := exec.Command("brew", "--prefix").Output(); err == nil {
			prefix = strings.TrimSpace(string(out))
		}
	}
	if prefix == "" {
		prefix = "/opt/homebrew"
	}
	return filepath.Join(prefix, "bin", "ffmpeg")
}
