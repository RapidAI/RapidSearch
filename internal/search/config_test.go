package search

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestMaskKeyLast4(t *testing.T) {
	m := maskKey("abcdefghij")
	if !m.Configured || m.Last4 != "ghij" {
		t.Fatalf("%+v", m)
	}
	empty := maskKey("")
	if empty.Configured || empty.Last4 != "" {
		t.Fatalf("%+v", empty)
	}
	short := maskKey("ab")
	if !short.Configured || short.Last4 != "ab" {
		t.Fatalf("%+v", short)
	}
}

func TestEmptyPatchDoesNotWipeKey(t *testing.T) {
	path := filepath.Join(t.TempDir(), "search-config.json")
	st, err := OpenConfig(path)
	if err != nil {
		t.Fatal(err)
	}
	key := "serper-secret-key-9999"
	if err := st.ApplyPatch(SettingsPatch{SerperAPIKey: strPtr(key)}); err != nil {
		t.Fatal(err)
	}
	if err := st.ApplyPatch(SettingsPatch{}); err != nil {
		t.Fatal(err)
	}
	empty := ""
	if err := st.ApplyPatch(SettingsPatch{SerperAPIKey: &empty, BraveAPIKey: &empty}); err != nil {
		t.Fatal(err)
	}
	snap := st.Snapshot()
	if snap.SerperKey != key {
		t.Fatalf("empty PUT wiped key")
	}
	pub := st.Public()
	if !pub.Serper.Configured || pub.Serper.Last4 != "9999" {
		t.Fatalf("mask=%+v", pub.Serper)
	}
	raw, err := json.Marshal(pub)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(raw), key) {
		t.Fatal("public view leaked raw key")
	}
	if !strings.Contains(mustRead(t, path), key) {
		t.Fatal("file should still hold the key")
	}
}

func TestClearKeyWipes(t *testing.T) {
	path := filepath.Join(t.TempDir(), "search-config.json")
	st, err := OpenConfig(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := st.ApplyPatch(SettingsPatch{SerperAPIKey: strPtr("keep-me-1234")}); err != nil {
		t.Fatal(err)
	}
	if err := st.ApplyPatch(SettingsPatch{ClearSerper: true}); err != nil {
		t.Fatal(err)
	}
	if st.Snapshot().HasKey("serper") {
		t.Fatal("clear should wipe")
	}
}

func TestConfigFileMode0600(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "search-config.json")
	st, err := OpenConfig(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := st.ApplyPatch(SettingsPatch{SerperAPIKey: strPtr("abcd1234")}); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Fatalf("mode=%o want 0600", info.Mode().Perm())
	}
}

func TestSearchConfigGitignored(t *testing.T) {
	b, err := os.ReadFile(filepath.Join("..", "..", ".gitignore"))
	if err != nil {
		t.Fatal(err)
	}
	text := string(b)
	if !strings.Contains(text, "search-config.json") {
		t.Fatalf(".gitignore missing search-config.json:\n%s", text)
	}
}

func TestCacheSigHasNoRawKey(t *testing.T) {
	s := ConfigSnapshot{SerperKey: "super-secret-key", BraveKey: "brave-secret"}
	sig := s.CacheSig()
	if strings.Contains(sig, "super-secret") || strings.Contains(sig, "brave-secret") {
		t.Fatalf("sig leaked key: %s", sig)
	}
	if !strings.Contains(sig, "serper=1") || !strings.Contains(sig, "brave=1") {
		t.Fatalf("sig=%s", sig)
	}
}

func strPtr(s string) *string { return &s }

func mustRead(t *testing.T, path string) string {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}
