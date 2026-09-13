package api

import (
	"bytes"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
)

const translateGlobalLockName = ".run.lock"

// translateWorkDir is the per-paper BabelDOC output directory.
func translateWorkDir(root, id string) string {
	id = sanitizePaperID(id)
	if root == "" || id == "" {
		return ""
	}
	return filepath.Join(root, "translate-work", id)
}

func translateGlobalLockPath(root string) string {
	if root == "" {
		root = papersRoot()
	}
	return filepath.Join(root, "translate-work", translateGlobalLockName)
}

// acquireTranslateLock takes an exclusive flock so at most one BabelDOC run
// proceeds for this papers root (including across search-service restarts).
// The returned unlock function must be called; it is safe if acquire failed.
func acquireTranslateLock(root string) (unlock func(), err error) {
	path := translateGlobalLockPath(root)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return func() {}, err
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return func() {}, err
	}
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX); err != nil {
		_ = f.Close()
		return func() {}, err
	}
	return func() {
		_ = syscall.Flock(int(f.Fd()), syscall.LOCK_UN)
		_ = f.Close()
	}, nil
}

// translateOSBusy reports whether a translate_worker.py or babeldoc process is
// already targeting this paper id (same workdir / --id), so we must not spawn
// another copy that would contend on the same output directory.
func translateOSBusy(root, id string) bool {
	id = sanitizePaperID(id)
	if id == "" {
		return false
	}
	markers := []string{
		filepath.ToSlash(filepath.Join("translate-work", id)),
		"--id " + id,
		"--id\x00" + id,
	}
	return anyProcCmdline(func(cmd string) bool {
		if !isTranslateCmdline(cmd) {
			return false
		}
		norm := strings.ReplaceAll(cmd, "\x00", " ")
		for _, m := range markers {
			if strings.Contains(norm, m) || strings.Contains(cmd, m) {
				return true
			}
		}
		// argv form: ... --id <id> ...
		if strings.Contains(cmd, "\x00--id\x00"+id+"\x00") || strings.HasSuffix(cmd, "\x00--id\x00"+id) {
			return true
		}
		return false
	})
}

// anyTranslateOSBusy reports any in-flight BabelDOC / translate_worker for this
// papers root (hard global concurrency = 1).
func anyTranslateOSBusy(root string) (busy bool, id string) {
	root = filepath.Clean(root)
	rootSlash := filepath.ToSlash(root)
	if rootSlash == "" || rootSlash == "." {
		return false, ""
	}
	anyProcCmdline(func(cmd string) bool {
		if !isTranslateCmdline(cmd) {
			return false
		}
		norm := filepath.ToSlash(strings.ReplaceAll(cmd, "\x00", " "))
		// Only count workers whose argv references THIS papers root.
		if !strings.Contains(norm, rootSlash) && !strings.Contains(cmd, root) {
			return false
		}
		busy = true
		id = extractTranslateID(cmd)
		return true // stop scan
	})
	return busy, id
}

func isTranslateCmdline(cmd string) bool {
	low := strings.ToLower(cmd)
	return strings.Contains(low, "babeldoc") || strings.Contains(low, "translate_worker.py")
}

func extractTranslateID(cmd string) string {
	// Prefer --id <value>
	parts := strings.Split(cmd, "\x00")
	for i := 0; i < len(parts)-1; i++ {
		if parts[i] == "--id" {
			return sanitizePaperID(parts[i+1])
		}
	}
	norm := strings.ReplaceAll(cmd, "\x00", " ")
	if i := strings.Index(norm, "translate-work/"); i >= 0 {
		rest := norm[i+len("translate-work/"):]
		var b strings.Builder
		for _, r := range rest {
			if r == ' ' || r == '/' || r == '\t' {
				break
			}
			b.WriteRune(r)
		}
		return sanitizePaperID(b.String())
	}
	return ""
}

func anyProcCmdline(match func(cmdline string) bool) bool {
	ents, err := os.ReadDir("/proc")
	if err != nil {
		return false
	}
	self := os.Getpid()
	for _, e := range ents {
		if !e.IsDir() {
			continue
		}
		pid, err := strconv.Atoi(e.Name())
		if err != nil || pid <= 1 || pid == self {
			continue
		}
		raw, err := os.ReadFile(filepath.Join("/proc", e.Name(), "cmdline"))
		if err != nil || len(raw) == 0 {
			continue
		}
		// Skip our own search-service binary (does not run babeldoc inline).
		if match(string(raw)) {
			return true
		}
	}
	return false
}

func writeTranslatePID(root, id string, pid int) error {
	dir := translateWorkDir(root, id)
	if dir == "" {
		return fmt.Errorf("empty workdir")
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	return os.WriteFile(filepath.Join(dir, "worker.pid"), []byte(strconv.Itoa(pid)+"\n"), 0o644)
}

func clearTranslatePID(root, id string) {
	dir := translateWorkDir(root, id)
	if dir == "" {
		return
	}
	_ = os.Remove(filepath.Join(dir, "worker.pid"))
}

func readTranslatePIDAlive(root, id string) bool {
	dir := translateWorkDir(root, id)
	if dir == "" {
		return false
	}
	b, err := os.ReadFile(filepath.Join(dir, "worker.pid"))
	if err != nil {
		return false
	}
	pid, err := strconv.Atoi(strings.TrimSpace(string(b)))
	if err != nil || pid <= 1 {
		return false
	}
	p, err := os.FindProcess(pid)
	if err != nil {
		return false
	}
	// Signal 0 checks existence on Unix.
	if err := p.Signal(syscall.Signal(0)); err != nil {
		return false
	}
	return true
}

// jobProcessBusy is true when a pidfile or live OS process still owns this id.
func jobProcessBusy(root, id string) bool {
	if readTranslatePIDAlive(root, id) {
		return true
	}
	return translateOSBusy(root, id)
}

// cmdlineHasAPIKeyFlag is used by tests to ensure we never pass secrets on argv.
func cmdlineHasAPIKeyFlag(args []string) bool {
	for i, a := range args {
		if a == "--openai-api-key" || strings.HasPrefix(a, "--openai-api-key=") {
			return true
		}
		if i > 0 && args[i-1] == "--openai-api-key" {
			return true
		}
	}
	return false
}

// redactCmdlineSecrets replaces likely key tokens for logs (not used on hot path).
func redactCmdlineSecrets(cmd []byte) string {
	parts := bytes.Split(cmd, []byte{0})
	out := make([]string, 0, len(parts))
	skip := false
	for _, p := range parts {
		s := string(p)
		if skip {
			out = append(out, "[redacted]")
			skip = false
			continue
		}
		if s == "--openai-api-key" {
			out = append(out, s)
			skip = true
			continue
		}
		if strings.HasPrefix(s, "--openai-api-key=") {
			out = append(out, "--openai-api-key=[redacted]")
			continue
		}
		out = append(out, s)
	}
	return strings.Join(out, " ")
}
