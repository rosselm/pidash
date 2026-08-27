// pidash serves a live dashboard for this Raspberry Pi: host metrics read
// straight off /proc, the SoC thermal and throttle state that only the firmware
// knows about, Docker containers, systemd unit health and a journal tail.
//
// It is deliberately self-contained — standard library only, frontend embedded
// in the binary — so it keeps working when the telemetry pipeline it sits next
// to does not.
//
// Usage:
//
//	pidash -addr :8090
package main

import (
	"context"
	"embed"
	"encoding/json"
	"flag"
	"fmt"
	"io/fs"
	"log"
	"net/http"
	"os"
	"os/signal"
	"runtime"
	"strings"
	"syscall"
	"time"
)

//go:embed web
var webFS embed.FS

// version is stamped at build time with -ldflags "-X main.version=...".
// A downloaded binary that cannot say what it is turns every bug report into
// a guessing game.
var version = "dev"

func main() {
	var (
		addr      = flag.String("addr", ":8090", "listen address")
		interval  = flag.Duration("interval", time.Second, "sampling interval")
		topN      = flag.Int("top", 8, "number of processes in the top table")
		unitsFlag = flag.String("units", "otelcol-contrib,pi-temp-exporter,docker,ssh", "comma-separated systemd units to watch")
		logsFlag  = flag.String("log-units", "otelcol-contrib,pi-temp-exporter", "comma-separated units to tail; empty tails the whole journal")
		sock      = flag.String("docker-sock", "/var/run/docker.sock", "docker socket path, empty to disable")
		showVer   = flag.Bool("version", false, "print version and exit")
	)
	flag.Parse()

	if *showVer {
		fmt.Printf("pidash %s (%s %s/%s)\n", version, runtime.Version(), runtime.GOOS, runtime.GOARCH)
		return
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	var docker *dockerClient
	if *sock != "" {
		if _, err := os.Stat(*sock); err == nil {
			docker = newDockerClient(*sock)
		} else {
			log.Printf("docker: %v — container panel disabled", err)
		}
	}

	sampler := NewSampler(*interval, docker, splitList(*unitsFlag), *topN)
	go sampler.Run(ctx)

	tail := NewLogTail(200)
	go tail.Run(ctx, splitList(*logsFlag))

	srv := &http.Server{
		Addr:              *addr,
		Handler:           routes(sampler, tail),
		ReadHeaderTimeout: 10 * time.Second,
		// No WriteTimeout: the SSE endpoints hold their responses open.
	}

	go func() {
		<-ctx.Done()
		sctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()
		_ = srv.Shutdown(sctx)
	}()

	log.Printf("pidash %s listening on %s (sampling every %s)", version, *addr, *interval)
	if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
		log.Fatalf("serve: %v", err)
	}
	log.Print("pidash stopped")
}

func routes(s *Sampler, t *LogTail) http.Handler {
	mux := http.NewServeMux()

	static, err := fs.Sub(webFS, "web")
	if err != nil {
		log.Fatalf("embed: %v", err)
	}
	mux.Handle("GET /", noCache(http.FileServer(http.FS(static))))

	mux.HandleFunc("GET /api/snapshot", func(w http.ResponseWriter, r *http.Request) {
		snap := s.Latest()
		if snap == nil {
			http.Error(w, "warming up", http.StatusServiceUnavailable)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		enc := json.NewEncoder(w)
		enc.SetIndent("", "  ")
		_ = enc.Encode(snap)
	})

	mux.HandleFunc("GET /api/stream", func(w http.ResponseWriter, r *http.Request) {
		flusher, ok := beginSSE(w)
		if !ok {
			return
		}
		ch, last, cancel := s.Subscribe()
		defer cancel()
		if last != nil {
			writeEvent(w, flusher, "snapshot", last)
		}
		keepalive := time.NewTicker(20 * time.Second)
		defer keepalive.Stop()
		for {
			select {
			case <-r.Context().Done():
				return
			case <-keepalive.C:
				fmt.Fprint(w, ": ping\n\n")
				flusher.Flush()
			case snap, ok := <-ch:
				if !ok {
					return
				}
				if !writeEvent(w, flusher, "snapshot", snap) {
					return
				}
			}
		}
	})

	mux.HandleFunc("GET /api/logs", func(w http.ResponseWriter, r *http.Request) {
		flusher, ok := beginSSE(w)
		if !ok {
			return
		}
		ch, history, cancel := t.Subscribe()
		defer cancel()
		for _, l := range history {
			if !writeEvent(w, flusher, "log", l) {
				return
			}
		}
		keepalive := time.NewTicker(20 * time.Second)
		defer keepalive.Stop()
		for {
			select {
			case <-r.Context().Done():
				return
			case <-keepalive.C:
				fmt.Fprint(w, ": ping\n\n")
				flusher.Flush()
			case l, ok := <-ch:
				if !ok {
					return
				}
				if !writeEvent(w, flusher, "log", l) {
					return
				}
			}
		}
	})

	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, r *http.Request) {
		if s.Latest() == nil {
			http.Error(w, "warming up", http.StatusServiceUnavailable)
			return
		}
		fmt.Fprintln(w, "ok")
	})

	return mux
}

func beginSSE(w http.ResponseWriter) (http.Flusher, bool) {
	flusher, ok := w.(http.Flusher)
	if !ok {
		http.Error(w, "streaming unsupported", http.StatusInternalServerError)
		return nil, false
	}
	h := w.Header()
	h.Set("Content-Type", "text/event-stream")
	h.Set("Cache-Control", "no-cache")
	h.Set("Connection", "keep-alive")
	h.Set("X-Accel-Buffering", "no") // in case something proxies this later
	flusher.Flush()
	return flusher, true
}

// writeEvent reports false once the client has gone away, so the handler can
// return instead of spinning on a dead connection.
func writeEvent(w http.ResponseWriter, f http.Flusher, event string, v any) bool {
	b, err := json.Marshal(v)
	if err != nil {
		return true
	}
	if _, err := fmt.Fprintf(w, "event: %s\ndata: %s\n\n", event, b); err != nil {
		return false
	}
	f.Flush()
	return true
}

// noCache keeps a browser from pinning an old build of the UI after the binary
// is replaced — the assets are embedded, so they change only on redeploy.
func noCache(h http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "no-store")
		h.ServeHTTP(w, r)
	})
}

func splitList(s string) []string {
	var out []string
	for _, p := range strings.Split(s, ",") {
		if p = strings.TrimSpace(p); p != "" {
			out = append(out, p)
		}
	}
	return out
}
