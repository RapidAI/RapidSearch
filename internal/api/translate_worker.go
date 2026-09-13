package api

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
)

const translateJobTimeout = 2 * time.Hour

// enqueueResult carries per-id rejection reasons (e.g. already running).
type enqueueResult struct {
	Queued   []string
	Skipped  []string
	Rejected map[string]string // id -> reason
}

func (s *translateService) enqueue(ids []string, papers []paperEntry, force bool) enqueueResult {
	out := enqueueResult{Rejected: map[string]string{}}
	if s == nil {
		out.Skipped = append([]string(nil), ids...)
		return out
	}
	s.start()
	byID := map[string]paperEntry{}
	for _, p := range papers {
		if p.ID != "" {
			byID[p.ID] = p
		}
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, raw := range ids {
		id := sanitizePaperID(raw)
		if id == "" {
			out.Skipped = append(out.Skipped, raw)
			continue
		}
		p, ok := byID[id]
		if !ok {
			if _, err := originalPDFAbs(s.root, id); err != nil {
				out.Skipped = append(out.Skipped, id)
				continue
			}
		} else if !p.HasLocal {
			out.Skipped = append(out.Skipped, id)
			continue
		}

		running := s.runID == id || jobProcessBusy(s.root, id)
		if jExisting, ok := s.status.Jobs[id]; ok && jExisting.Status == translateRunning {
			running = true
		}
		if running {
			// Never spawn a parallel BabelDOC for the same id.
			out.Skipped = append(out.Skipped, id)
			out.Rejected[id] = "正在翻译中"
			continue
		}

		alreadyQueued := s.inQ[id]
		if jExisting, ok := s.status.Jobs[id]; ok && jExisting.Status == translateQueued {
			alreadyQueued = true
		}
		if alreadyQueued {
			if force {
				// Replace: ensure a single queue entry (no duplicate spawn).
				if !s.inQ[id] {
					s.queue = append(s.queue, id)
					s.inQ[id] = true
				}
				j := s.status.Jobs[id]
				j.ID = id
				j.Status = translateQueued
				j.Error = ""
				j.Filename = p.Filename
				j.UpdatedAt = time.Now().UTC().Format(time.RFC3339)
				if s.status.Jobs == nil {
					s.status.Jobs = map[string]translateJob{}
				}
				s.status.Jobs[id] = j
				out.Queued = append(out.Queued, id)
				continue
			}
			out.Skipped = append(out.Skipped, id)
			continue
		}

		complete := (p.ZhPDF != "" && p.DualPDF != "") || outputsExist(s.root, id)
		hasAny := p.ZhPDF != "" || p.DualPDF != "" || anyTranslatedOutput(s.root, id)
		if complete && !force {
			// Auto / normal translate must never re-queue completed papers.
			out.Skipped = append(out.Skipped, id)
			continue
		}
		if force && hasAny {
			// 「再次翻译」: clear prior outputs so runOne does not short-circuit.
			removeTranslatedOutputs(s.root, id)
		}

		s.queue = append(s.queue, id)
		s.inQ[id] = true
		j := s.status.Jobs[id]
		j.ID = id
		j.Status = translateQueued
		j.Error = ""
		j.ZhRel = ""
		j.DualRel = ""
		j.Filename = p.Filename
		j.UpdatedAt = time.Now().UTC().Format(time.RFC3339)
		if s.status.Jobs == nil {
			s.status.Jobs = map[string]translateJob{}
		}
		s.status.Jobs[id] = j
		out.Queued = append(out.Queued, id)
	}
	if len(out.Queued) > 0 {
		s.status.UpdatedAt = time.Now().UTC().Format(time.RFC3339)
		s.status.Version = 1
		if err := s.persistStatusLocked(); err != nil {
			log.Printf("papers translate-status write: %v", err)
		}
		select {
		case s.kick <- struct{}{}:
		default:
		}
	}
	return out
}

func outputsExist(root, id string) bool {
	_, e1 := translatedPDFAbs(root, translateKindZH, id)
	_, e2 := translatedPDFAbs(root, translateKindDual, id)
	return e1 == nil && e2 == nil
}

func anyTranslatedOutput(root, id string) bool {
	_, e1 := translatedPDFAbs(root, translateKindZH, id)
	_, e2 := translatedPDFAbs(root, translateKindDual, id)
	return e1 == nil || e2 == nil
}

func removeTranslatedOutputs(root, id string) {
	id = sanitizePaperID(id)
	if id == "" {
		return
	}
	for _, kind := range []string{translateKindZH, translateKindDual} {
		if abs, err := translatedPDFAbs(root, kind, id); err == nil {
			_ = os.Remove(abs)
		}
		// Also remove canonical names in case Abs lookup already failed mid-replace.
		_ = os.Remove(filepath.Join(root, "pdfs", kind, id+"."+kind+".pdf"))
	}
}

func (s *translateService) loop() {
	for {
		s.reconcileExternal()
		if s.slotBusy() {
			select {
			case <-s.kick:
			case <-time.After(2 * time.Second):
			}
			continue
		}
		id := s.pop()
		if id == "" {
			select {
			case <-s.kick:
			case <-time.After(3 * time.Second):
			}
			continue
		}
		s.runOne(id)
	}
}

// slotBusy is true while this service (or an orphaned OS worker) already holds
// the single global translation slot.
func (s *translateService) slotBusy() bool {
	if s == nil {
		return false
	}
	s.mu.Lock()
	runID := s.runID
	root := s.root
	s.mu.Unlock()
	if runID != "" {
		return true
	}
	busy, _ := anyTranslateOSBusy(root)
	return busy
}

// reconcileExternal keeps status in sync with orphaned babeldoc processes left
// behind by a previous search-service instance, and claims the global slot so
// we never start a second job while one is still running.
func (s *translateService) reconcileExternal() {
	if s == nil {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	now := time.Now().UTC().Format(time.RFC3339)
	dirty := false

	// Adopt live OS workers into status=running and hold the slot.
	if busy, id := anyTranslateOSBusy(s.root); busy && id != "" {
		j := s.status.Jobs[id]
		j.ID = id
		if j.Status != translateRunning {
			j.Status = translateRunning
			j.Error = ""
			if j.StartedAt == "" {
				j.StartedAt = now
			}
			j.UpdatedAt = now
			if s.status.Jobs == nil {
				s.status.Jobs = map[string]translateJob{}
			}
			s.status.Jobs[id] = j
			dirty = true
		}
		if s.runID == "" {
			s.runID = id
		}
	}

	for id, j := range s.status.Jobs {
		if j.Status != translateRunning {
			continue
		}
		// Our in-process runner owns this id — do not treat it as a dead orphan.
		if s.runID == id {
			continue
		}
		alive := jobProcessBusy(s.root, id)
		if alive {
			if s.runID == "" {
				s.runID = id
			}
			continue
		}
		// Process gone: promote outputs or mark interrupted.
		if s.runID == id {
			s.runID = ""
		}
		if zh, e1 := translatedPDFAbs(s.root, translateKindZH, id); e1 == nil {
			if dual, e2 := translatedPDFAbs(s.root, translateKindDual, id); e2 == nil {
				j.Status = translateDone
				j.Error = ""
				j.ZhRel = filepath.ToSlash(filepath.Join("pdfs", translateKindZH, filepath.Base(zh)))
				j.DualRel = filepath.ToSlash(filepath.Join("pdfs", translateKindDual, filepath.Base(dual)))
				j.Finished = now
				j.UpdatedAt = now
				s.status.Jobs[id] = j
				dirty = true
				continue
			}
		}
		// Still writing? workdir may have partial output from a just-exited worker.
		mono, dual := collectBabelOutputs(translateWorkDir(s.root, id))
		if mono != "" || dual != "" {
			zhRel, dualRel, err := installTranslatedPDFs(s.root, id, mono, dual)
			if err == nil && zhRel != "" && dualRel != "" {
				j.Status = translateDone
				j.Error = ""
				j.ZhRel = zhRel
				j.DualRel = dualRel
				j.Finished = now
				j.UpdatedAt = now
				s.status.Jobs[id] = j
				dirty = true
				continue
			}
		}
		j.Status = translateFailed
		j.Error = "interrupted (process restarted)"
		j.Finished = now
		j.UpdatedAt = now
		s.status.Jobs[id] = j
		dirty = true
	}
	if dirty {
		s.status.UpdatedAt = now
		s.status.Version = 1
		if err := s.persistStatusLocked(); err != nil {
			log.Printf("papers translate-status write: %v", err)
		}
	}
}

func (s *translateService) pop() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.queue) == 0 {
		return ""
	}
	id := s.queue[0]
	s.queue = s.queue[1:]
	delete(s.inQ, id)
	s.runID = id
	return id
}

func (s *translateService) runOne(id string) {
	defer func() {
		s.mu.Lock()
		if s.runID == id {
			s.runID = ""
		}
		s.mu.Unlock()
	}()

	now := time.Now().UTC().Format(time.RFC3339)
	j, _ := s.job(id)
	j.ID = id
	j.Status = translateRunning
	j.StartedAt = now
	j.Error = ""
	s.putJob(j)

	src, err := s.sourcePDF(id)
	if err != nil {
		j.Status = translateFailed
		j.Error = "source PDF not found"
		j.Finished = time.Now().UTC().Format(time.RFC3339)
		s.putJob(j)
		return
	}
	j.Filename = filepath.Base(src)

	// Already produced both outputs (e.g. copied in while queued).
	if zh, e1 := translatedPDFAbs(s.root, translateKindZH, id); e1 == nil {
		if dual, e2 := translatedPDFAbs(s.root, translateKindDual, id); e2 == nil {
			j.Status = translateDone
			j.ZhRel = filepath.ToSlash(filepath.Join("pdfs", translateKindZH, filepath.Base(zh)))
			j.DualRel = filepath.ToSlash(filepath.Join("pdfs", translateKindDual, filepath.Base(dual)))
			j.Finished = time.Now().UTC().Format(time.RFC3339)
			s.putJob(j)
			return
		}
	}

	cfg := s.snapshot()
	if !cfg.ready() {
		j.Status = translateFailed
		j.Error = "LLM settings incomplete (need api_key and model)"
		j.Finished = time.Now().UTC().Format(time.RFC3339)
		s.putJob(j)
		return
	}

	// Another OS worker already owns this id (e.g. orphaned after reload).
	if jobProcessBusy(s.root, id) {
		log.Printf("papers translate id=%s status=adopt existing process", id)
		s.waitForProcess(id)
		if zh, e1 := translatedPDFAbs(s.root, translateKindZH, id); e1 == nil {
			if dual, e2 := translatedPDFAbs(s.root, translateKindDual, id); e2 == nil {
				j.Status = translateDone
				j.Error = ""
				j.ZhRel = filepath.ToSlash(filepath.Join("pdfs", translateKindZH, filepath.Base(zh)))
				j.DualRel = filepath.ToSlash(filepath.Join("pdfs", translateKindDual, filepath.Base(dual)))
				j.Finished = time.Now().UTC().Format(time.RFC3339)
				s.putJob(j)
				return
			}
		}
		mono, dual := collectBabelOutputs(translateWorkDir(s.root, id))
		zhRel, dualRel, err := installTranslatedPDFs(s.root, id, mono, dual)
		if err == nil && zhRel != "" && dualRel != "" {
			j.Status = translateDone
			j.Error = ""
			j.ZhRel = zhRel
			j.DualRel = dualRel
			j.Finished = time.Now().UTC().Format(time.RFC3339)
			s.putJob(j)
			return
		}
		j.Status = translateFailed
		j.Error = "existing translate process exited without outputs"
		j.Finished = time.Now().UTC().Format(time.RFC3339)
		s.putJob(j)
		return
	}

	unlock, lockErr := acquireTranslateLock(s.root)
	defer unlock()
	if lockErr != nil {
		j.Status = translateFailed
		j.Error = "could not acquire translate lock: " + sanitizeUserError(lockErr.Error())
		j.Finished = time.Now().UTC().Format(time.RFC3339)
		s.putJob(j)
		return
	}
	// Re-check after lock: another instance may have started meanwhile.
	if jobProcessBusy(s.root, id) || func() bool { b, other := anyTranslateOSBusy(s.root); return b && other != "" && other != id }() {
		j.Status = translateFailed
		j.Error = "skipped: another translate process is already running"
		j.Finished = time.Now().UTC().Format(time.RFC3339)
		s.putJob(j)
		return
	}

	ctx, cancel := context.WithTimeout(context.Background(), translateJobTimeout)
	defer cancel()
	runner := s.runner
	if runner == nil {
		runner = defaultTranslateRunner
	}
	log.Printf("papers translate id=%s status=start model=%s", id, cfg.Model)
	zhRel, dualRel, err := runner(ctx, translateRun{
		ID:       id,
		InputPDF: src,
		Root:     s.root,
		Cfg:      cfg,
	})
	if err != nil {
		log.Printf("papers translate id=%s status=fail err=%s", id, sanitizeUserError(err.Error()))
		j.Status = translateFailed
		j.Error = sanitizeUserError(err.Error())
		j.Finished = time.Now().UTC().Format(time.RFC3339)
		s.putJob(j)
		return
	}
	if zhRel == "" && dualRel == "" {
		j.Status = translateFailed
		j.Error = "babeldoc produced no output PDFs"
		j.Finished = time.Now().UTC().Format(time.RFC3339)
		s.putJob(j)
		return
	}
	if zhRel == "" || dualRel == "" {
		j.Status = translateFailed
		j.Error = "babeldoc produced incomplete output (need both zh and dual)"
		j.ZhRel = zhRel
		j.DualRel = dualRel
		j.Finished = time.Now().UTC().Format(time.RFC3339)
		s.putJob(j)
		return
	}
	log.Printf("papers translate id=%s status=done", id)
	j.Status = translateDone
	j.Error = ""
	j.ZhRel = zhRel
	j.DualRel = dualRel
	j.Finished = time.Now().UTC().Format(time.RFC3339)
	s.putJob(j)
}


func (s *translateService) waitForProcess(id string) {
	deadline := time.Now().Add(translateJobTimeout)
	for time.Now().Before(deadline) {
		if !jobProcessBusy(s.root, id) {
			return
		}
		time.Sleep(2 * time.Second)
	}
}
func (s *translateService) sourcePDF(id string) (string, error) {
	if abs, err := originalPDFAbs(s.root, id); err == nil {
		return abs, nil
	}
	// Catalog lookup: id may be arxiv while the file is named "{id}_title.pdf".
	cat, err := (&papersStore{root: s.root}).catalogBase()
	if err == nil {
		for _, p := range cat.Papers {
			if paperTranslateID(p) == id && p.Filename != "" {
				if abs, err := originalPDFAbs(s.root, p.Filename); err == nil {
					return abs, nil
				}
			}
		}
	}
	return "", os.ErrNotExist
}

func defaultTranslateRunner(ctx context.Context, job translateRun) (string, string, error) {
	if py, script, ok := findTranslateWorker(); ok {
		return runPythonWorker(ctx, py, script, job)
	}
	return runBabelDOC(ctx, job)
}

func findTranslateWorker() (py, script string, ok bool) {
	if v := strings.TrimSpace(os.Getenv("PAPERS_TRANSLATE_WORKER")); v != "" {
		if st, err := os.Stat(v); err == nil && !st.IsDir() {
			script = v
		}
	}
	if script == "" {
		cands := []string{
			filepath.Join(papersRoot(), "translate_worker.py"),
			filepath.Join("agent-papers", "translate_worker.py"),
		}
		if exe, err := os.Executable(); err == nil {
			cands = append(cands, filepath.Join(filepath.Dir(exe), "agent-papers", "translate_worker.py"))
		}
		if wd, err := os.Getwd(); err == nil {
			cands = append(cands, filepath.Join(wd, "agent-papers", "translate_worker.py"))
		}
		for _, c := range cands {
			if st, err := os.Stat(c); err == nil && !st.IsDir() {
				script = c
				break
			}
		}
	}
	if script == "" {
		return "", "", false
	}
	if v := strings.TrimSpace(os.Getenv("PAPERS_PYTHON")); v != "" {
		py = v
	} else if p, err := exec.LookPath("python3"); err == nil {
		py = p
	} else if p, err := exec.LookPath("python"); err == nil {
		py = p
	} else {
		return "", script, false
	}
	return py, script, true
}

func runPythonWorker(ctx context.Context, py, script string, job translateRun) (string, string, error) {
	args := []string{
		script,
		"--input", job.InputPDF,
		"--id", job.ID,
		"--out-root", job.Root,
		"--model", job.Cfg.Model,
		"--lang-in", "en",
		"--lang-out", "zh-CN",
	}
	if job.Cfg.BaseURL != "" {
		args = append(args, "--base-url", job.Cfg.BaseURL)
	}
	if job.Cfg.QPS > 0 {
		args = append(args, "--qps", fmt.Sprintf("%d", job.Cfg.QPS))
	}
	cmd := exec.CommandContext(ctx, py, args...)
	env := os.Environ()
	if job.Cfg.APIKey != "" {
		// Prefer env; never put the key on argv (process list).
		env = append(env, "OPENAI_API_KEY="+job.Cfg.APIKey)
	}
	cmd.Env = env
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Start(); err != nil {
		return "", "", fmt.Errorf("translate worker: %s", sanitizeUserError(err.Error()))
	}
	if cmd.Process != nil {
		_ = writeTranslatePID(job.Root, job.ID, cmd.Process.Pid)
	}
	defer clearTranslatePID(job.Root, job.ID)
	if err := cmd.Wait(); err != nil {
		msg := strings.TrimSpace(stderr.String())
		if msg == "" {
			msg = err.Error()
		}
		return "", "", fmt.Errorf("translate worker: %s", sanitizeUserError(msg))
	}
	var out struct {
		OK   bool   `json:"ok"`
		Zh   string `json:"zh"`
		Dual string `json:"dual"`
		Error string `json:"error"`
	}
	line := lastJSONLine(stdout.Bytes())
	if err := json.Unmarshal(line, &out); err != nil {
		return "", "", fmt.Errorf("translate worker: invalid json (%s)", sanitizeUserError(err.Error()))
	}
	if !out.OK {
		msg := out.Error
		if msg == "" {
			msg = "worker reported failure"
		}
		return "", "", fmt.Errorf("%s", sanitizeUserError(msg))
	}
	return out.Zh, out.Dual, nil
}

func lastJSONLine(b []byte) []byte {
	b = bytes.TrimSpace(b)
	if len(b) == 0 {
		return b
	}
	if i := bytes.LastIndexByte(b, '\n'); i >= 0 {
		return bytes.TrimSpace(b[i+1:])
	}
	return b
}

func runBabelDOC(ctx context.Context, job translateRun) (string, string, error) {
	bin, err := exec.LookPath("babeldoc")
	if err != nil {
		return "", "", fmt.Errorf("babeldoc not on PATH (uv tool install --python 3.12 BabelDOC)")
	}
	work := filepath.Join(job.Root, "translate-work", job.ID)
	if err := os.MkdirAll(work, 0o755); err != nil {
		return "", "", err
	}
	args := []string{
		"--openai",
		"--openai-model", job.Cfg.Model,
		"--files", job.InputPDF,
		"--lang-in", "en",
		"--lang-out", "zh-CN",
		"--output", work,
		"--watermark-output-mode=no_watermark",
	}
	if job.Cfg.BaseURL != "" {
		args = append(args, "--openai-base-url", job.Cfg.BaseURL)
	}
	if job.Cfg.QPS > 0 {
		args = append(args, "--qps", fmt.Sprintf("%d", job.Cfg.QPS))
	}
	run := func(a []string) error {
		cmd := exec.CommandContext(ctx, bin, a...)
		env := os.Environ()
		if job.Cfg.APIKey != "" {
			// Prefer env over CLI so `ps` does not leak the key.
			env = append(env, "OPENAI_API_KEY="+job.Cfg.APIKey)
		}
		cmd.Env = env
		var stderr bytes.Buffer
		cmd.Stderr = &stderr
		cmd.Stdout = &stderr
		if err := cmd.Run(); err != nil {
			return fmt.Errorf("babeldoc: %s", sanitizeUserError(strings.TrimSpace(stderr.String())))
		}
		return nil
	}
	if err := run(args); err != nil {
		if strings.Contains(err.Error(), "watermark-output-mode") || strings.Contains(err.Error(), "unrecognized") {
			stripped := stripFlagPrefix(args, "--watermark-output-mode")
			if err2 := run(stripped); err2 != nil {
				return "", "", err2
			}
		} else {
			return "", "", err
		}
	}
	mono, dual := collectBabelOutputs(work)
	return installTranslatedPDFs(job.Root, job.ID, mono, dual)
}

func stripFlagPrefix(args []string, prefix string) []string {
	out := make([]string, 0, len(args))
	for _, a := range args {
		if strings.HasPrefix(a, prefix) {
			continue
		}
		out = append(out, a)
	}
	return out
}

func boolWord(ok bool) string {
	if ok {
		return "set"
	}
	return "missing"
}
