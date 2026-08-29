package tunnel

import (
	"encoding/binary"
	"encoding/json"
	"errors"
	"io"
	"strings"
)

const MaxFrame = 4 << 20 // 4 MiB

var (
	ErrTooLarge = errors.New("tunnel frame too large")
	ErrEmpty    = errors.New("empty tunnel frame")
)

// Frame is one multiplexed request, response, or keepalive message.
type Frame struct {
	Type    string            `json:"type"`
	ID      string            `json:"id,omitempty"`
	Method  string            `json:"method,omitempty"`
	Path    string            `json:"path,omitempty"`
	Headers map[string]string `json:"headers,omitempty"`
	Body    string            `json:"body,omitempty"` // base64
	Status  int               `json:"status,omitempty"`
	Error   string            `json:"error,omitempty"`
}

func WriteFrame(w io.Writer, f Frame) error {
	b, err := json.Marshal(f)
	if err != nil {
		return err
	}
	if len(b) > MaxFrame {
		return ErrTooLarge
	}
	var hdr [4]byte
	binary.BigEndian.PutUint32(hdr[:], uint32(len(b)))
	if _, err := w.Write(hdr[:]); err != nil {
		return err
	}
	_, err = w.Write(b)
	return err
}

func ReadFrame(r io.Reader, f *Frame) error {
	var hdr [4]byte
	if _, err := io.ReadFull(r, hdr[:]); err != nil {
		return err
	}
	n := binary.BigEndian.Uint32(hdr[:])
	if n == 0 {
		return ErrEmpty
	}
	if n > MaxFrame {
		return ErrTooLarge
	}
	buf := make([]byte, n)
	if _, err := io.ReadFull(r, buf); err != nil {
		return err
	}
	return json.Unmarshal(buf, f)
}

func ParseAuthLine(line string) (token string, ok bool) {
	line = strings.TrimSpace(line)
	if !strings.HasPrefix(line, "AUTH ") {
		return "", false
	}
	tok := strings.TrimSpace(strings.TrimPrefix(line, "AUTH "))
	if tok == "" {
		return "", false
	}
	return tok, true
}
