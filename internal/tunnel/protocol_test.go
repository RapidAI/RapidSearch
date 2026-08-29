package tunnel

import (
	"bytes"
	"testing"
)

func TestFrameRoundTrip(t *testing.T) {
	var buf bytes.Buffer
	in := Frame{Type: "req", ID: "1", Method: "GET", Path: "/health", Body: "aGVsbG8="}
	if err := WriteFrame(&buf, in); err != nil {
		t.Fatal(err)
	}
	var out Frame
	if err := ReadFrame(&buf, &out); err != nil {
		t.Fatal(err)
	}
	if out.Type != "req" || out.ID != "1" || out.Path != "/health" || out.Body != "aGVsbG8=" {
		t.Fatalf("%+v", out)
	}
}

func TestParseAuthLine(t *testing.T) {
	tok, ok := ParseAuthLine("AUTH secret-token\n")
	if !ok || tok != "secret-token" {
		t.Fatalf("got %q %v", tok, ok)
	}
	if _, ok := ParseAuthLine("NOPE"); ok {
		t.Fatal("expected fail")
	}
}
