package main

import (
	"log"
	"net/http"
	"net/http/pprof"
	"strings"
)

// startPprof serves Go's runtime profiles (/debug/pprof/) on a loopback address, for
// memory and goroutine checks: go tool pprof http://127.0.0.1:6060/debug/pprof/heap.
// Off unless partyd is started with -pprof.
func startPprof(addr string) {
	if !strings.HasPrefix(addr, "127.0.0.1:") && !strings.HasPrefix(addr, "localhost:") {
		log.Printf("pprof: %q is not a loopback address; not started", addr)
		return
	}
	mux := http.NewServeMux()
	mux.HandleFunc("/debug/pprof/", pprof.Index)
	mux.HandleFunc("/debug/pprof/cmdline", pprof.Cmdline)
	mux.HandleFunc("/debug/pprof/profile", pprof.Profile)
	mux.HandleFunc("/debug/pprof/symbol", pprof.Symbol)
	mux.HandleFunc("/debug/pprof/trace", pprof.Trace)
	go func() {
		log.Printf("pprof on http://%s/debug/pprof/", addr)
		if err := http.ListenAndServe(addr, mux); err != nil {
			log.Printf("pprof: %v", err)
		}
	}()
}
