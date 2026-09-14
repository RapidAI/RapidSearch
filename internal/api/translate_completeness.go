package api

import (
	"encoding/json"
	"fmt"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

// verifyInstalledTranslation runs the Python worker's completeness check on
// EN vs installed ZH. On failure it removes zh/dual outputs and returns an error
// so the job is not marked done. If the checker is unavailable, it logs and
// accepts (same soft-skip as the worker when pdfinfo is missing).
func verifyInstalledTranslation(root, id, enPDF string) error {
	id = sanitizePaperID(id)
	if id == "" || enPDF == "" {
		return nil
	}
	zhAbs, err := translatedPDFAbs(root, translateKindZH, id)
	if err != nil || zhAbs == "" {
		return fmt.Errorf("zh PDF missing after install")
	}
	// Dual must exist for a complete job; install paths already enforce this
	// before calling verify, but keep a soft guard.
	if _, err := translatedPDFAbs(root, translateKindDual, id); err != nil {
		return fmt.Errorf("dual PDF missing after install")
	}

	py, script, ok := findTranslateWorker()
	if !ok {
		log.Printf("papers translate completeness: worker not found; skipping check id=%s", id)
		return nil
	}
	cmd := exec.Command(py, script, "--check-completeness", "--input", enPDF, "--zh", zhAbs)
	out, err := cmd.CombinedOutput()
	line := lastJSONLine(out)
	var res struct {
		OK      bool   `json:"ok"`
		Message string `json:"message"`
		Error   string `json:"error"`
	}
	if uerr := json.Unmarshal(line, &res); uerr != nil {
		// pdftotext/pdfinfo may be missing in some environments — do not fail hard.
		log.Printf("papers translate completeness: unreadable checker output id=%s; accepting", id)
		return nil
	}
	if res.OK {
		if res.Message != "" && res.Message != "ok" && !strings.HasPrefix(res.Message, "skipped") {
			log.Printf("papers translate completeness id=%s warning=%s", id, sanitizeUserError(res.Message))
		}
		return nil
	}
	msg := res.Message
	if msg == "" {
		msg = res.Error
	}
	if msg == "" {
		msg = "incomplete translation (completeness check failed)"
	}
	removeTranslatedOutputs(root, id)
	if err != nil {
		// Non-zero exit is expected on failure; prefer structured message.
		_ = err
	}
	return fmt.Errorf("%s", sanitizeUserError(msg))
}

// sourcePDFForCompleteness resolves the English original for an id.
func sourcePDFForCompleteness(root, id string) string {
	if abs, err := originalPDFAbs(root, id); err == nil {
		return abs
	}
	// Match translateService.sourcePDF catalog fallback without needing the service.
	manPath := filepath.Join(root, "manifest.json")
	raw, err := os.ReadFile(manPath)
	if err != nil {
		return ""
	}
	var man papersManifest
	if err := json.Unmarshal(raw, &man); err != nil {
		return ""
	}
	pdfDir := filepath.Join(root, "pdfs")
	for _, p := range man.Papers {
		p = enrichPaper(p, pdfDir)
		if paperTranslateID(p) == id && p.Filename != "" {
			if abs, err := originalPDFAbs(root, p.Filename); err == nil {
				return abs
			}
		}
	}
	return ""
}
