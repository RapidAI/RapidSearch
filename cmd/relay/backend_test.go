package main

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"search-service/internal/tunnel"
)

func TestDoBackendDoesNotSilentlyTruncate(t *testing.T) {
	// 2 MiB + extra — the old LimitReader(MaxFrame/2) produced a 200 whose
	// body ended mid-JSON. That must never happen again.
	payload := append(bytes.Repeat([]byte("x"), int(tunnel.BufferedBodyMax)), []byte(`{"more":true}`)...)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write(payload)
	}))
	t.Cleanup(srv.Close)

	f := doBackend(context.Background(), srv.Client(), srv.URL, tunnel.Frame{
		Type: tunnel.TypeReq, ID: "big", Method: http.MethodGet, Path: "/health",
	})
	raw, err := base64.StdEncoding.DecodeString(f.Body)
	if err != nil {
		t.Fatal(err)
	}
	if f.Status == http.StatusOK {
		if bytes.Equal(raw, payload) {
			return // complete body is acceptable
		}
		t.Fatalf("200 must not be a truncated body: got %d want %d", len(raw), len(payload))
	}
	if f.Status != http.StatusRequestEntityTooLarge && f.Status != http.StatusBadGateway {
		t.Fatalf("status=%d body=%s", f.Status, raw)
	}
	var obj map[string]any
	if err := json.Unmarshal(raw, &obj); err != nil {
		t.Fatalf("error body must be complete JSON, not a half payload: %s", raw)
	}
	if obj["code"] != "tunnel" {
		t.Fatalf("want loud tunnel error, got %+v", obj)
	}
}

func TestServeBackendStreamsOversizedGET(t *testing.T) {
	payload := append([]byte(`{"papers":"`), bytes.Repeat([]byte("a"), int(tunnel.BufferedBodyMax)+4096)...)
	payload = append(payload, []byte(`"}`)...)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write(payload)
	}))
	t.Cleanup(srv.Close)

	var got []byte
	var types []string
	write := func(fr tunnel.Frame) error {
		types = append(types, fr.Type)
		if fr.Type == tunnel.TypeRespChunk {
			b, err := base64.StdEncoding.DecodeString(fr.Body)
			if err != nil {
				return err
			}
			got = append(got, b...)
		}
		if fr.Type == tunnel.TypeResp {
			t.Fatal("oversized GET must not use a single buffered resp frame")
		}
		return nil
	}
	err := serveBackend(context.Background(), srv.Client(), srv.URL, tunnel.Frame{
		Type: tunnel.TypeReq, ID: "cat", Method: http.MethodGet, Path: "/papers/api/catalog",
	}, write)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, payload) {
		t.Fatalf("streamed body %d bytes, want %d (truncated or corrupted)", len(got), len(payload))
	}
	if len(types) < 3 || types[0] != tunnel.TypeRespHead || types[len(types)-1] != tunnel.TypeRespEnd {
		t.Fatalf("stream frames=%v", types)
	}
	if !json.Valid(got) {
		t.Fatal("streamed catalog must be complete JSON")
	}
}

func TestReadBufferedBodyOverflow(t *testing.T) {
	src := strings.NewReader(strings.Repeat("z", 100))
	raw, overflow, err := readBufferedBody(src, 50)
	if err != nil {
		t.Fatal(err)
	}
	if !overflow {
		t.Fatal("expected overflow")
	}
	if len(raw) != 51 { // limit + peek byte
		t.Fatalf("prefetch=%d", len(raw))
	}
	rest, err := io.ReadAll(src)
	if err != nil {
		t.Fatal(err)
	}
	if len(raw)+len(rest) != 100 {
		t.Fatalf("lost bytes: prefetch=%d rest=%d", len(raw), len(rest))
	}
}
