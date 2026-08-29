package main

import (
	"bufio"
	"bytes"
	"context"
	"encoding/base64"
	"fmt"
	"io"
	"log"
	"net"
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

	log.Printf("search-relay backend=%s tunnel=%s", backend, tun)
	backoff := time.Second
	for ctx.Err() == nil {
		connectedAt := time.Now()
		err := runOnce(ctx, tun, token, backend, client)
		if ctx.Err() != nil {
			break
		}
		if time.Since(connectedAt) > 10*time.Second {
			backoff = time.Second
		}
		log.Printf("tunnel dropped: %v; reconnect in %s", err, backoff)
		t := time.NewTimer(backoff)
		select {
		case <-ctx.Done():
			t.Stop()
			return
		case <-t.C:
		}
		if backoff < 30*time.Second {
			backoff *= 2
			if backoff > 30*time.Second {
				backoff = 30 * time.Second
			}
		}
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
	d := net.Dialer{Timeout: 15 * time.Second, KeepAlive: 30 * time.Second}
	c, err := d.DialContext(ctx, "tcp", addr)
	if err != nil {
		return err
	}
	defer c.Close()

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
		_ = c.SetWriteDeadline(time.Now().Add(30 * time.Second))
		err := tunnel.WriteFrame(c, f)
		_ = c.SetWriteDeadline(time.Time{})
		return err
	}

	stopPing := make(chan struct{})
	go func() {
		t := time.NewTicker(25 * time.Second)
		defer t.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-stopPing:
				return
			case <-t.C:
				if err := write(tunnel.Frame{Type: "ping"}); err != nil {
					_ = c.Close()
					return
				}
			}
		}
	}()
	defer func() { close(stopPing); _ = c.Close() }()

	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		default:
		}
		_ = c.SetReadDeadline(time.Now().Add(90 * time.Second))
		var f tunnel.Frame
		if err := tunnel.ReadFrame(br, &f); err != nil {
			return err
		}
		switch f.Type {
		case "ping":
			_ = write(tunnel.Frame{Type: "pong", ID: f.ID})
		case "pong":
			// keepalive
		case "req":
			go func(f tunnel.Frame) {
				resp := doBackend(ctx, client, backend, f)
				if err := write(resp); err != nil {
					log.Printf("write resp id=%s: %v", f.ID, err)
					_ = c.Close()
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
		if lk == "host" || lk == "authorization" || lk == "content-length" {
			continue
		}
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
