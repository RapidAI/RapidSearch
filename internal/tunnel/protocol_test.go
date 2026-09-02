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

func TestPathIsDownload(t *testing.T) {
	if !PathIsDownload("/download") || !PathIsDownload("/download?url=https://x") {
		t.Fatal("download path")
	}
	if PathIsDownload("/search") || PathIsDownload("/download/other") {
		t.Fatal("not download")
	}
}

func TestPathIsSettings(t *testing.T) {
	if !PathIsSettings("/settings") || !PathIsSettings("/settings/config") || !PathIsSettings("/settings/config?x=1") {
		t.Fatal("settings path")
	}
	if !PathIsSettings("/settings/login") || !PathIsSettings("/settings/logout") {
		t.Fatal("settings login/logout path")
	}
	if PathIsSettings("/search") || PathIsSettings("/settingsx") {
		t.Fatal("not settings")
	}
}

func TestStreamFrames(t *testing.T) {
	var buf bytes.Buffer
	frames := []Frame{
		{Type: TypeRespHead, ID: "1", Status: 200, Headers: map[string]string{"Content-Type": "text/plain"}},
		{Type: TypeRespChunk, ID: "1", Body: "aGVsbG8="},
		{Type: TypeRespEnd, ID: "1"},
	}
	for _, f := range frames {
		if err := WriteFrame(&buf, f); err != nil {
			t.Fatal(err)
		}
	}
	for i, want := range frames {
		var out Frame
		if err := ReadFrame(&buf, &out); err != nil {
			t.Fatal(err)
		}
		if out.Type != want.Type || out.ID != want.ID {
			t.Fatalf("%d %+v", i, out)
		}
	}
}
