package acme

import (
	"context"
	"fmt"
	"log"
	"net"
	"net/http"
	"strings"
	"sync"
)

const challengePath = "/.well-known/acme-challenge/"

type HTTP01Solver struct {
	addr   string
	tokens map[string]string
	mu     sync.RWMutex
	server *http.Server
}

func NewHTTP01Solver(addr string) *HTTP01Solver {
	if addr == "" {
		addr = ":80"
	}
	return &HTTP01Solver{
		addr:   addr,
		tokens: make(map[string]string),
	}
}

func (s *HTTP01Solver) Start(ctx context.Context) error {
	mux := http.NewServeMux()
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		if !strings.HasPrefix(r.URL.Path, challengePath) {
			http.NotFound(w, r)
			return
		}
		token := strings.TrimPrefix(r.URL.Path, challengePath)
		s.mu.RLock()
		response, ok := s.tokens[token]
		s.mu.RUnlock()
		if !ok {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "application/octet-stream")
		_, _ = w.Write([]byte(response))
	})

	s.server = &http.Server{Handler: mux}

	ln, err := net.Listen("tcp", s.addr)
	if err != nil {
		return fmt.Errorf("acme solver listen %s: %w", s.addr, err)
	}

	go func() {
		log.Printf("[acme] challenge server listening on %s", s.addr)
		if err := s.server.Serve(ln); err != nil && err != http.ErrServerClosed {
			log.Printf("[acme] challenge server error: %v", err)
		}
	}()

	go func() {
		<-ctx.Done()
		_ = s.server.Close()
	}()

	return nil
}

func (s *HTTP01Solver) Present(ctx context.Context, token, response string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.tokens[token] = response
	return nil
}

func (s *HTTP01Solver) CleanUp(ctx context.Context, token string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.tokens, token)
	return nil
}
