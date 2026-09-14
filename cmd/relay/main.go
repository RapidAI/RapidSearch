package main

import (
	"bufio"
	"bytes"
	"context"
	"encoding/base64"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/url"
	"os"
	"os/signal"
	"strings"
	"sync"
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
	backend := strings.TrimRight(getenv("SEARCH_BACKEND", "http://127.0.0.1:18765"), "/")
	tun, err := parseTunnelAddr(os.Getenv("PROXY_TUNNEL"))
	if err != nil {
		log.Fatal(err)
	}

	ctx, cancel := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer cancel()

	client := &http.Client{
		Timeout: 3 * time.Minute,
		CheckRedirect: func(req *http.Request, via []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}

	log.Printf("search-relay backend=%s tunnel=%s keepalive ping=%s pong_wait=%s read_idle=%s tcp_keepalive=%s",
		backend, tun, tunnel.PingInterval, tunnel.PongTimeout, tunnel.ReadIdleTimeout, tunnel.TCPKeepAlivePeriod)
	backoff := tunnel.ReconnectMin
	for ctx.Err() == nil {
		connectedAt := time.Now()
		err := runOnce(ctx, tun, token, backend, client)
		if ctx.Err() != nil {
			break
		}
		alive := time.Since(connectedAt)
		if alive > tunnel.HealthyResetAfter {
			backoff = tunnel.ReconnectMin
		}
		log.Printf("tunnel reconnect: reason=%v alive=%s backoff=%s", err, alive.Truncate(time.Millisecond), backoff)
		t := time.NewTimer(backoff)
		select {
		case <-ctx.Done():
			t.Stop()
			return
		case <-t.C:
		}
		backoff = tunnel.NextReconnectBackoff(backoff)
	}
}

func getenv(k, def string) string {
	if v := strings.TrimSpace(os.Getenv(k)); v != "" {
		return v
	}
	return def
}

func parseTunnelAddr(s string) (string, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return "", fmt.Errorf("PROXY_TUNNEL is required (host:port, http://host:port, or tcp://host:port)")
	}
	if !strings.Contains(s, "://") {
		if !strings.Contains(s, ":") {
			s += ":18781"
		}
		return s, nil
	}
	u, err := url.Parse(s)
	if err != nil {
		return "", fmt.Errorf("PROXY_TUNNEL: %w", err)
	}
	host := u.Host
	if host == "" {
		return "", fmt.Errorf("invalid PROXY_TUNNEL %q", s)
	}
	if u.Port() == "" {
		host += ":18781"
	}
	return host, nil
}

func runOnce(ctx context.Context, addr, token, backend string, client *http.Client) error {
	d := tunnel.Dialer(15 * time.Second)
	c, err := d.DialContext(ctx, "tcp", addr)
	if err != nil {
		return err
	}
	defer c.Close()
	tunnel.EnableTCPKeepAlive(c)

	_ = c.SetDeadline(time.Now().Add(15 * time.Second))
	if _, err := c.Write([]byte("AUTH " + token + "\n")); err != nil {
		return err
	}
	br := bufio.NewReader(c)
	line, err := br.ReadString('\n')
	if err != nil {
		return err
	}
	if strings.TrimSpace(line) != "OK" {
		return fmt.Errorf("auth rejected")
	}
	_ = c.SetDeadline(time.Time{})
	log.Printf("connected to proxy tunnel %s", addr)

	var wmu sync.Mutex
	write := func(f tunnel.Frame) error {
		wmu.Lock()
		defer wmu.Unlock()
		_ = tunnel.SetWriteIdle(c)
		err := tunnel.WriteFrame(c, f)
		tunnel.ClearWriteDeadline(c)
		return err
	}

	var closeOnce sync.Once
	closeConn := func() { closeOnce.Do(func() { _ = c.Close() }) }
	defer closeConn()

	errCh := make(chan error, 1)
	die := func(err error) {
		select {
		case errCh <- err:
		default:
		}
		closeConn()
	}

	onPong, stopKA := tunnel.StartKeepalive(ctx, tunnel.DefaultKeepalive(), write, die)
	defer stopKA()

	for {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		var f tunnel.Frame
		if err := tunnel.ReadFrameRefreshing(c, br, &f); err != nil {
			select {
			case kerr := <-errCh:
				return tunnel.ClassifyReadError(kerr)
			default:
			}
			if ctx.Err() != nil {
				return ctx.Err()
			}
			return tunnel.ClassifyReadError(err)
		}
		switch f.Type {
		case tunnel.TypePing:
			_ = write(tunnel.Frame{Type: tunnel.TypePong, ID: f.ID})
		case tunnel.TypePong:
			onPong()
		case tunnel.TypeReq:
			go func(f tunnel.Frame) {
				if tunnel.PathNeedsStream(f.Path) {
					if err := streamBackend(ctx, client, backend, f, write); err != nil {
						log.Printf("stream download id=%s: %v", f.ID, err)
						die(fmt.Errorf("stream write: %w", err))
					}
					return
				}
				resp := doBackend(ctx, client, backend, f)
				if err := write(resp); err != nil {
					log.Printf("write resp id=%s: %v", f.ID, err)
					die(fmt.Errorf("resp write: %w", err))
				}
			}(f)
		}
	}
}

func doBackend(ctx context.Context, client *http.Client, backend string, f tunnel.Frame) tunnel.Frame {
	path := f.Path
	if path == "" {
		path = "/"
	}
	rawURL := backend + path
	var body io.Reader
	if f.Body != "" {
		b, err := base64.StdEncoding.DecodeString(f.Body)
		if err != nil {
			b = []byte(f.Body)
		}
		body = bytes.NewReader(b)
	}
	method := f.Method
	if method == "" {
		method = http.MethodGet
	}
	req, err := http.NewRequestWithContext(ctx, method, rawURL, body)
	if err != nil {
		return tunnel.Frame{Type: "resp", ID: f.ID, Status: 502, Error: err.Error()}
	}
	for k, v := range f.Headers {
		lk := strings.ToLower(k)
		if lk == "host" || lk == "content-length" {
			continue
		}
		if lk == "authorization" && !tunnel.PathPassthroughAuth(path) {
			continue
		}
		// Cookie / Set-Cookie travel with other headers so POST /settings/login
		// and subsequent /settings + /settings/config keep the HttpOnly session.
		req.Header.Set(k, v)
	}
	log.Printf("backend %s %s", method, path)
	res, err := client.Do(req)
	if err != nil {
		return tunnel.Frame{Type: "resp", ID: f.ID, Status: 502, Error: err.Error()}
	}
	defer res.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(res.Body, tunnel.MaxFrame/2))
	hdrs := map[string]string{}
	for k, vs := range res.Header {
		if len(vs) > 0 && strings.ToLower(k) != "transfer-encoding" && strings.ToLower(k) != "content-length" {
			hdrs[k] = vs[0]
		}
	}
	return tunnel.Frame{
		Type:    "resp",
		ID:      f.ID,
		Status:  res.StatusCode,
		Headers: hdrs,
		Body:    base64.StdEncoding.EncodeToString(raw),
	}
}

func streamBackend(ctx context.Context, client *http.Client, backend string, f tunnel.Frame, write func(tunnel.Frame) error) error {
	path := f.Path
	if path == "" {
		path = "/"
	}
	rawURL := backend + path
	var body io.Reader
	if f.Body != "" {
		b, err := base64.StdEncoding.DecodeString(f.Body)
		if err != nil {
			b = []byte(f.Body)
		}
		body = bytes.NewReader(b)
	}
	method := f.Method
	if method == "" {
		method = http.MethodGet
	}
	req, err := http.NewRequestWithContext(ctx, method, rawURL, body)
	if err != nil {
		return write(tunnel.Frame{Type: tunnel.TypeRespEnd, ID: f.ID, Status: 502, Error: err.Error()})
	}
	for k, v := range f.Headers {
		lk := strings.ToLower(k)
		if lk == "host" || lk == "content-length" {
			continue
		}
		if lk == "authorization" && !tunnel.PathPassthroughAuth(path) {
			continue
		}
		req.Header.Set(k, v)
	}
	log.Printf("backend stream %s %s", method, path)
	res, err := client.Do(req)
	if err != nil {
		_ = write(tunnel.Frame{Type: tunnel.TypeRespHead, ID: f.ID, Status: 502, Headers: map[string]string{"Content-Type": "application/json; charset=utf-8"}})
		_ = write(tunnel.Frame{Type: tunnel.TypeRespChunk, ID: f.ID, Body: base64.StdEncoding.EncodeToString([]byte(`{"error":"download failed","code":"fetch"}`))})
		return write(tunnel.Frame{Type: tunnel.TypeRespEnd, ID: f.ID, Error: err.Error()})
	}
	defer res.Body.Close()

	hdrs := map[string]string{}
	for k, vs := range res.Header {
		lk := strings.ToLower(k)
		if lk == "transfer-encoding" || lk == "content-length" {
			continue
		}
		if len(vs) > 0 {
			hdrs[k] = vs[0]
		}
	}
	// Keep content-length when known so clients can show progress.
	if res.ContentLength > 0 {
		hdrs["Content-Length"] = fmt.Sprintf("%d", res.ContentLength)
	}
	if err := write(tunnel.Frame{Type: tunnel.TypeRespHead, ID: f.ID, Status: res.StatusCode, Headers: hdrs}); err != nil {
		return err
	}
	buf := make([]byte, tunnel.StreamChunk)
	for {
		n, err := res.Body.Read(buf)
		if n > 0 {
			if werr := write(tunnel.Frame{
				Type: tunnel.TypeRespChunk,
				ID:   f.ID,
				Body: base64.StdEncoding.EncodeToString(buf[:n]),
			}); werr != nil {
				return werr
			}
		}
		if err == io.EOF {
			break
		}
		if err != nil {
			return write(tunnel.Frame{Type: tunnel.TypeRespEnd, ID: f.ID, Error: err.Error()})
		}
	}
	return write(tunnel.Frame{Type: tunnel.TypeRespEnd, ID: f.ID})
}
