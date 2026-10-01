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

// translateWorkDir is the per-paper BabelDOC output directory.
func translateWorkDir(root, id string) string {
	id = sanitizePaperID(id)
	if root == "" || id == "" {
		return ""
	}
	return filepath.Join(root, "translate-work", id)
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

// scanTranslateOSBusy lists in-flight BabelDOC / translate_worker processes for
// this papers root. unidentified workers (no extractable paper id) are counted
// separately so they still occupy a concurrency slot after a restart.
func scanTranslateOSBusy(root string) (ids []string, unknown int) {
	root = filepath.Clean(root)
	rootSlash := filepath.ToSlash(root)
	if rootSlash == "" || rootSlash == "." {
		return nil, 0
	}
	seen := map[string]bool{}
	anyProcCmdline(func(cmd string) bool {
		if !isTranslateCmdline(cmd) {
			return false
		}
		norm := filepath.ToSlash(strings.ReplaceAll(cmd, "\x00", " "))
		// Only count workers whose argv references THIS papers root.
		if !strings.Contains(norm, rootSlash) && !strings.Contains(cmd, root) {
			return false
		}
		id := extractTranslateID(cmd)
		if id == "" {
			unknown++
			return false
		}
		if !seen[id] {
			seen[id] = true
			ids = append(ids, id)
		}
		return false // continue scan — multiple papers may be translating
	})
	return ids, unknown
}

func listTranslateOSBusy(root string) []string {
	ids, _ := scanTranslateOSBusy(root)
	return ids
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

func forEachProcCmdline(fn func(pid int, cmdline string) bool) bool {
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
		if fn(pid, string(raw)) {
			return true
		}
	}
	return false
}

func anyProcCmdline(match func(cmdline string) bool) bool {
	return forEachProcCmdline(func(_ int, cmd string) bool {
		return match(cmd)
	})
}

func listTranslatePIDsForID(id string) []int {
	id = sanitizePaperID(id)
	if id == "" {
		return nil
	}
	var pids []int
	forEachProcCmdline(func(pid int, cmd string) bool {
		if !isTranslateCmdline(cmd) {
			return false
		}
		if extractTranslateID(cmd) == id {
			pids = append(pids, pid)
		}
		return false
	})
	return pids
}

func readTranslatePID(root, id string) int {
	dir := translateWorkDir(root, id)
	if dir == "" {
		return 0
	}
	b, err := os.ReadFile(filepath.Join(dir, "worker.pid"))
	if err != nil {
		return 0
	}
	pid, err := strconv.Atoi(strings.TrimSpace(string(b)))
	if err != nil || pid <= 1 {
		return 0
	}
	return pid
}

func procPPID(pid int) int {
	raw, err := os.ReadFile(filepath.Join("/proc", strconv.Itoa(pid), "stat"))
	if err != nil {
		return 0
	}
	i := bytes.LastIndexByte(raw, ')')
	if i < 0 || i+1 >= len(raw) {
		return 0
	}
	fields := strings.Fields(string(raw[i+1:]))
	if len(fields) < 2 {
		return 0
	}
	ppid, err := strconv.Atoi(fields[1])
	if err != nil {
		return 0
	}
	return ppid
}

func listDescendantPIDs(rootPID int) []int {
	if rootPID <= 1 {
		return nil
	}
	children := map[int][]int{}
	ents, err := os.ReadDir("/proc")
	if err != nil {
		return nil
	}
	for _, e := range ents {
		if !e.IsDir() {
			continue
		}
		pid, err := strconv.Atoi(e.Name())
		if err != nil || pid <= 1 {
			continue
		}
		ppid := procPPID(pid)
		if ppid > 1 {
			children[ppid] = append(children[ppid], pid)
		}
	}
	var out []int
	q := []int{rootPID}
	seen := map[int]bool{rootPID: true}
	for len(q) > 0 {
		p := q[0]
		q = q[1:]
		for _, c := range children[p] {
			if seen[c] {
				continue
			}
			seen[c] = true
			out = append(out, c)
			q = append(q, c)
		}
	}
	return out
}

func killPID(pid int) {
	if pid <= 1 {
		return
	}
	// Only signal a process group when this pid is a distinct group leader.
	// Workers started without Setpgid share search-service's PGID — never
	// SIGKILL that group.
	if pgid, err := syscall.Getpgid(pid); err == nil && pgid == pid {
		if self, err2 := syscall.Getpgid(os.Getpid()); err2 != nil || pgid != self {
			_ = syscall.Kill(-pgid, syscall.SIGKILL)
		}
	}
	if p, err := os.FindProcess(pid); err == nil {
		_ = p.Kill()
	}
}

// killTranslateJobTree sends SIGKILL to the translate_worker / babeldoc tree
// for this paper id (pidfile, cmdline match, and descendants) so a hung job
// cannot keep occupying a concurrency slot.
func killTranslateJobTree(root, id string) int {
	id = sanitizePaperID(id)
	if id == "" {
		return 0
	}
	pids := map[int]struct{}{}
	if pid := readTranslatePID(root, id); pid > 1 {
		pids[pid] = struct{}{}
		for _, c := range listDescendantPIDs(pid) {
			pids[c] = struct{}{}
		}
	}
	for _, pid := range listTranslatePIDsForID(id) {
		if pid <= 1 {
			continue
		}
		pids[pid] = struct{}{}
		for _, c := range listDescendantPIDs(pid) {
			pids[c] = struct{}{}
		}
	}
	self := os.Getpid()
	n := 0
	for pid := range pids {
		if pid <= 1 || pid == self {
			continue
		}
		killPID(pid)
		n++
	}
	clearTranslatePID(root, id)
	return n
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
	pid := readTranslatePID(root, id)
	if pid <= 1 {
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
