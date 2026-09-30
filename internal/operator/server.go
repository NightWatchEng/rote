// Package operator is the human side of a handoff: a deliberately bare
// console (one HTML page and a small JSON API) through which an operator sees
// the intervention request, takes control of the live session, drives it, and
// hands control back. The console is a mock; the API and the control
// transfer behind it are the real mechanism, and a richer console would call
// exactly these endpoints.
package operator

import (
	"context"
	_ "embed"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"time"

	"github.com/NightWatchEng/rote/internal/control"
)

//go:embed console.html
var consoleHTML []byte

// Server exposes one Hub over HTTP.
type Server struct {
	hub *control.Hub
	srv *http.Server
	ln  net.Listener
}

// Listen starts the console on addr (e.g. "127.0.0.1:8090"). Only loopback
// addresses are accepted: the console has no authentication, so it must not
// be reachable from anywhere but the machine running the run.
func Listen(addr string, hub *control.Hub) (*Server, error) {
	host, _, err := net.SplitHostPort(addr)
	if err != nil {
		return nil, fmt.Errorf("operator address %q: %w", addr, err)
	}
	if ip := net.ParseIP(host); host != "localhost" && (ip == nil || !ip.IsLoopback()) {
		return nil, fmt.Errorf("operator console must listen on a loopback address, not %q (it has no authentication)", host)
	}
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		return nil, err
	}
	s := &Server{hub: hub, ln: ln}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /{$}", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.Write(consoleHTML)
	})
	mux.HandleFunc("GET /api/state", s.state)
	mux.HandleFunc("GET /api/screen", s.screen)
	mux.HandleFunc("POST /api/claim", s.claim)
	mux.HandleFunc("POST /api/input", s.input)
	mux.HandleFunc("POST /api/resolve", s.resolve)
	s.srv = &http.Server{Handler: mux}
	go s.srv.Serve(ln)
	return s, nil
}

func (s *Server) URL() string { return "http://" + s.ln.Addr().String() + "/" }

func (s *Server) Close() error { return s.srv.Close() }

func (s *Server) state(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]any{
		"control":      s.hub.Session().Control(),
		"intervention": s.hub.Current(),
	})
}

// screen serves the live, unredacted view of the session, and only while an
// intervention is open: an operator sees the application exactly as they
// would at their own workstation, for as long as they have been asked to.
func (s *Server) screen(w http.ResponseWriter, r *http.Request) {
	if s.hub.Current() == nil {
		http.Error(w, "no open intervention", http.StatusConflict)
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 3*time.Second)
	defer cancel()
	shot, err := s.hub.Session().Screenshot(ctx)
	if err != nil {
		http.Error(w, err.Error(), http.StatusServiceUnavailable)
		return
	}
	w.Header().Set("Content-Type", "image/png")
	w.Header().Set("Cache-Control", "no-store")
	w.Write(shot)
}

type body struct {
	ID       string `json:"id"`
	Operator string `json:"operator"`
	Action   string `json:"action"`
	Note     string `json:"note"`
	control.Input
}

func decode(w http.ResponseWriter, r *http.Request) (body, bool) {
	var b body
	if err := json.NewDecoder(r.Body).Decode(&b); err != nil {
		http.Error(w, "bad JSON: "+err.Error(), http.StatusBadRequest)
		return b, false
	}
	return b, true
}

func reply(w http.ResponseWriter, v any, err error) {
	if err != nil {
		http.Error(w, err.Error(), http.StatusConflict)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(v)
}

func (s *Server) claim(w http.ResponseWriter, r *http.Request) {
	if b, ok := decode(w, r); ok {
		reply(w, map[string]string{"status": "claimed"}, s.hub.Claim(b.ID, b.Operator))
	}
}

func (s *Server) input(w http.ResponseWriter, r *http.Request) {
	if b, ok := decode(w, r); ok {
		act, err := s.hub.Input(r.Context(), b.ID, b.Operator, b.Input)
		reply(w, act, err)
	}
}

func (s *Server) resolve(w http.ResponseWriter, r *http.Request) {
	if b, ok := decode(w, r); ok {
		reply(w, map[string]string{"status": "resolved"}, s.hub.Resolve(b.ID, b.Operator, b.Action, b.Note))
	}
}
