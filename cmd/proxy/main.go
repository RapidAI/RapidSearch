package main

import (
	"bufio"
	"context"
	"crypto/subtle"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"search-service/internal/tunnel"
)

func main() {
	log.SetFlags(log.LstdFlags | log.Lmicroseconds)
	log.SetOutput(os.Stdout)

	token := strings.TrimSpace(os.Getenv("SEARCH_TOKEN"))
	if token == "" {
		log.Fatal("SEARCH_TOKEN is required")
	}
	publicAddr := getenv("PROXY_LISTEN", "0.0.0.0:18780")
	tunnelAddr := getenv("TUNNEL_LISTEN", "0.0.0.0:18781")

	hub := newHub(token)

	tunLn, err := net.Listen("tcp", tunnelAddr)
	if err != nil {
		log.Fatalf("tunnel listen %s: %v", tunnelAddr, err)
	}
	go func() {
		log.Printf("search-proxy tunnel listening on %s", tunnelAddr)
		for {
			c, err := tunLn.Accept()
			if err != nil {
				log.Printf("tunnel accept: %v", err)
				return
			}
			go hub.handleTunnel(c)
		}
	}()

	mux := http.NewServeMux()
	mux.HandleFunc("/health", hub.serveHTTP)
	mux.HandleFunc("/search", hub.serveHTTP)

	hs := &http.Server{
		Addr:              publicAddr,
		Handler:           mux,
		ReadHeaderTimeout: 10 * time.Second,
		ReadTimeout:       30 * time.Second,
		WriteTimeout:      3 * time.Minute,
		IdleTimeout:       60 * time.Second,
	}
	go func() {
		log.Printf("search-proxy public API listening on http://%s", publicAddr)
		if err := hs.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			log.Fatalf("http: %v", err)
		}
	}()

	sig := make(chan os.Signal, 1)
	signal.Notify(sig, syscall.SIGINT, syscall.SIGTERM)
	<-sig
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_ = hs.Shutdown(ctx)
	_ = tunLn.Close()
	hub.close()
}

func getenv(k, def string) string {
	if v := strings.TrimSpace(os.Getenv(k)); v != "" {
		return v
	}
	return def
}

type hub struct {
	token string
	mu    sync.Mutex
	sess  *session
	seq   atomic.Uint64
}

func newHub(token string) *hub { return &hub{token: token} }

func (h *hub) close() {
	h.mu.Lock()
	s := h.sess
	h.sess = nil
	h.mu.Unlock()
	if s != nil {
		s.close(io.EOF)
	}
}

func (h *hub) handleTunnel(c net.Conn) {
	_ = c.SetDeadline(time.Now().Add(15 * time.Second))
	br := bufio.NewReader(c)
	line, err := br.ReadString('\n')
	if err != nil {
		log.Printf("tunnel auth read from %s: %v", c.RemoteAddr(), err)
		_ = c.Close()
		return
	}
	got, ok := tunnel.ParseAuthLine(line)
	if !ok || subtle.ConstantTimeCompare([]byte(got), []byte(h.token)) != 1 {
		log.Printf("tunnel auth failed from %s", c.RemoteAddr())
		_, _ = c.Write([]byte("ERR unauthorized\n"))
		_ = c.Close()
		return
	}
	if _, err := c.Write([]byte("OK\n")); err != nil {
		_ = c.Close()
		return
	}
	_ = c.SetDeadline(time.Time{})
	if tc, ok := c.(*net.TCPConn); ok {
		_ = tc.SetKeepAlive(true)
		_ = tc.SetKeepAlivePeriod(30 * time.Second)
	}

	s := newSession(c, br)
	h.mu.Lock()
	old := h.sess
	h.sess = s
	h.mu.Unlock()
	if old != nil {
		log.Printf("replacing previous tunnel from %s", old.remote)
		old.close(errReplaced)
	}
	log.Printf("tunnel connected from %s", c.RemoteAddr())
	s.readLoop()
	log.Printf("tunnel disconnected from %s", c.RemoteAddr())
	h.mu.Lock()
	if h.sess == s {
		h.sess = nil
	}
	h.mu.Unlock()
}

var errReplaced = fmt.Errorf("replaced by newer tunnel")

func (h *hub) current() *session {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.sess
}

func (h *hub) authorized(r *http.Request) bool {
	var got string
	if a := r.Header.Get("Authorization"); strings.HasPrefix(strings.ToLower(a), "bearer ") {
		got = strings.TrimSpace(a[7:])
	}
	if got == "" {
		got = strings.TrimSpace(r.URL.Query().Get("token"))
	}
	if got == "" {
		return false
	}
	return subtle.ConstantTimeCompare([]byte(got), []byte(h.token)) == 1
}

func (h *hub) serveHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet && r.Method != http.MethodPost && r.Method != http.MethodHead {
		writeJSON(w, http.StatusMethodNotAllowed, map[string]string{"error": "method not allowed", "code": "bad_request"})
		return
	}
	if !h.authorized(r) {
		writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "unauthorized", "code": "unauthorized"})
		return
	}
	s := h.current()
	if s == nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": "search backend offline", "code": "offline"})
		return
	}

	body, _ := io.ReadAll(io.LimitReader(r.Body, tunnel.MaxFrame/2))
	_ = r.Body.Close()

	u := *r.URL
	q := u.Query()
	q.Del("token")
	u.RawQuery = q.Encode()
	path := u.RequestURI()
	if path == "" {
		path = u.Path
	}

	hdrs := map[string]string{}
	for k, vs := range r.Header {
		lk := strings.ToLower(k)
		if hopHeaders[lk] {
			continue
		}
		if len(vs) > 0 {
			hdrs[k] = vs[0]
		}
	}

	id := fmt.Sprintf("%d-%d", time.Now().UnixNano(), h.seq.Add(1))
	fr := tunnel.Frame{
		Type:    "req",
		ID:      id,
		Method:  r.Method,
		Path:    path,
		Headers: hdrs,
		Body:    base64.StdEncoding.EncodeToString(body),
	}

	ctx, cancel := context.WithTimeout(r.Context(), 3*time.Minute)
	defer cancel()
	resp, err := s.roundTrip(ctx, fr)
	if err != nil {
		if err == errReplaced || strings.Contains(err.Error(), "offline") {
			writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": "search backend offline", "code": "offline"})
			return
		}
		log.Printf("proxy forward %s %s: %v", r.Method, path, err)
		writeJSON(w, http.StatusBadGateway, map[string]string{"error": "tunnel error", "code": "tunnel"})
		return
	}
	if resp.Error != "" && resp.Status == 0 {
		writeJSON(w, http.StatusBadGateway, map[string]string{"error": "backend error", "code": "tunnel"})
		return
	}
	raw, err := base64.StdEncoding.DecodeString(resp.Body)
	if err != nil {
		raw = []byte(resp.Body)
	}
	for k, v := range resp.Headers {
		if hopHeaders[strings.ToLower(k)] {
			continue
		}
		w.Header().Set(k, v)
	}
	if w.Header().Get("Content-Type") == "" {
		w.Header().Set("Content-Type", "application/json; charset=utf-8")
	}
	status := resp.Status
	if status == 0 {
		status = http.StatusOK
	}
	w.WriteHeader(status)
	_, _ = w.Write(raw)
}

var hopHeaders = map[string]bool{
	"connection": true, "keep-alive": true, "proxy-authenticate": true,
	"proxy-authorization": true, "te": true, "trailers": true,
	"transfer-encoding": true, "upgrade": true, "authorization": true,
	"host": true, "content-length": true,
}

type session struct {
	conn   net.Conn
	br     *bufio.Reader
	remote string
	wmu    sync.Mutex
	mu     sync.Mutex
	pend   map[string]chan tunnel.Frame
	closed bool
}

func newSession(c net.Conn, br *bufio.Reader) *session {
	return &session{conn: c, br: br, remote: c.RemoteAddr().String(), pend: map[string]chan tunnel.Frame{}}
}

func (s *session) write(f tunnel.Frame) error {
	s.wmu.Lock()
	defer s.wmu.Unlock()
	_ = s.conn.SetWriteDeadline(time.Now().Add(30 * time.Second))
	err := tunnel.WriteFrame(s.conn, f)
	_ = s.conn.SetWriteDeadline(time.Time{})
	return err
}

func (s *session) roundTrip(ctx context.Context, f tunnel.Frame) (tunnel.Frame, error) {
	ch := make(chan tunnel.Frame, 1)
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return tunnel.Frame{}, fmt.Errorf("search backend offline")
	}
	s.pend[f.ID] = ch
	s.mu.Unlock()
	defer func() {
		s.mu.Lock()
		delete(s.pend, f.ID)
		s.mu.Unlock()
	}()
	if err := s.write(f); err != nil {
		return tunnel.Frame{}, err
	}
	select {
	case resp := <-ch:
		return resp, nil
	case <-ctx.Done():
		return tunnel.Frame{}, ctx.Err()
	}
}

func (s *session) readLoop() {
	defer s.close(io.EOF)
	for {
		_ = s.conn.SetReadDeadline(time.Now().Add(90 * time.Second))
		var f tunnel.Frame
		if err := tunnel.ReadFrame(s.br, &f); err != nil {
			return
		}
		switch f.Type {
		case "pong", "ping":
			if f.Type == "ping" {
				_ = s.write(tunnel.Frame{Type: "pong", ID: f.ID})
			}
		case "resp":
			s.mu.Lock()
			ch := s.pend[f.ID]
			s.mu.Unlock()
			if ch != nil {
				select {
				case ch <- f:
				default:
				}
			}
		}
	}
}

func (s *session) close(err error) {
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return
	}
	s.closed = true
	for id, ch := range s.pend {
		select {
		case ch <- tunnel.Frame{Type: "resp", ID: id, Status: 503, Error: "offline"}:
		default:
		}
		delete(s.pend, id)
	}
	s.mu.Unlock()
	_ = s.conn.Close()
}

func writeJSON(w http.ResponseWriter, status int, v interface{}) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	enc := json.NewEncoder(w)
	enc.SetEscapeHTML(false)
	_ = enc.Encode(v)
}
