// Package fakeha is an in-memory Home Assistant REST API for tests. It speaks
// the same wire protocol as a real instance (bearer auth, /api/states,
// /api/services) so tests can exercise the real REST client end to end
// without live credentials.
package fakeha

import (
	"crypto/subtle"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sort"
	"sync"
	"testing"
)

// Call records one service call received by the server.
type Call struct {
	Domain   string
	Service  string
	EntityID string
}

type entity struct {
	state string
	attrs map[string]any
}

// Server is a fake Home Assistant listening on a loopback port.
type Server struct {
	*httptest.Server
	token string

	mu       sync.Mutex
	entities map[string]entity
	calls    []Call
}

// New starts a server that accepts only the given bearer token. It is closed
// automatically when the test ends.
func New(t testing.TB, token string) *Server {
	t.Helper()
	s := &Server{token: token, entities: make(map[string]entity)}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /api/config", s.handleConfig)
	mux.HandleFunc("GET /api/states", s.handleStates)
	mux.HandleFunc("GET /api/states/{id}", s.handleState)
	mux.HandleFunc("POST /api/services/{domain}/{service}", s.handleService)
	s.Server = httptest.NewServer(s.authenticate(mux))
	t.Cleanup(s.Close)
	return s
}

// Set creates or replaces an entity.
func (s *Server) Set(id, state string, attrs map[string]any) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.entities[id] = entity{state: state, attrs: attrs}
}

// Calls returns a copy of every service call received so far.
func (s *Server) Calls() []Call {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]Call(nil), s.calls...)
}

func (s *Server) authenticate(next http.Handler) http.Handler {
	want := []byte("Bearer " + s.token)
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if subtle.ConstantTimeCompare([]byte(r.Header.Get("Authorization")), want) != 1 {
			http.Error(w, `{"message":"401: Unauthorized"}`, http.StatusUnauthorized)
			return
		}
		next.ServeHTTP(w, r)
	})
}

func (s *Server) handleConfig(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, map[string]any{"version": "2026.10.0"})
}

func (s *Server) handleStates(w http.ResponseWriter, _ *http.Request) {
	s.mu.Lock()
	ids := make([]string, 0, len(s.entities))
	for id := range s.entities {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	out := make([]map[string]any, 0, len(ids))
	for _, id := range ids {
		out = append(out, s.render(id))
	}
	s.mu.Unlock()
	writeJSON(w, out)
}

func (s *Server) handleState(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	s.mu.Lock()
	_, ok := s.entities[id]
	var body map[string]any
	if ok {
		body = s.render(id)
	}
	s.mu.Unlock()
	if !ok {
		http.Error(w, `{"message":"Entity not found."}`, http.StatusNotFound)
		return
	}
	writeJSON(w, body)
}

func (s *Server) handleService(w http.ResponseWriter, r *http.Request) {
	var payload struct {
		EntityID string `json:"entity_id"`
	}
	if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}
	call := Call{Domain: r.PathValue("domain"), Service: r.PathValue("service"), EntityID: payload.EntityID}
	s.mu.Lock()
	s.calls = append(s.calls, call)
	e, ok := s.entities[call.EntityID]
	changed := []map[string]any{}
	if ok && (call.Service == "turn_on" || call.Service == "turn_off") {
		e.state = map[string]string{"turn_on": "on", "turn_off": "off"}[call.Service]
		s.entities[call.EntityID] = e
		changed = append(changed, s.render(call.EntityID))
	}
	s.mu.Unlock()
	writeJSON(w, changed)
}

func (s *Server) render(id string) map[string]any {
	e := s.entities[id]
	attrs := e.attrs
	if attrs == nil {
		attrs = map[string]any{}
	}
	return map[string]any{
		"entity_id": id, "state": e.state, "attributes": attrs,
		"last_changed": "2026-10-27T10:00:00+00:00", "last_updated": "2026-10-27T10:00:00+00:00",
	}
}

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(v)
}
