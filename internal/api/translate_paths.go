package api

import (
	"os"
	"path/filepath"
	"strings"
	"unicode"
)

const (
	translateKindZH   = "zh"
	translateKindDual = "dual"
)

// paperTranslateID is the stable key for translation files and status.
// Prefer a sanitized arXiv id (slash → underscore); otherwise the PDF stem.
func paperTranslateID(p paperEntry) string {
	if id := sanitizeArxivID(p.ArxivID); id != "" {
		return strings.ReplaceAll(id, "/", "_")
	}
	name := filepath.Base(strings.TrimSpace(p.Filename))
	if name == "" || name == "." {
		name = filepath.Base(strings.TrimSpace(p.PDFPath))
	}
	name = strings.TrimSuffix(name, filepath.Ext(name))
	return sanitizePaperID(name)
}

func sanitizePaperID(id string) string {
	id = strings.TrimSpace(id)
	id = strings.TrimPrefix(id, "arXiv:")
	id = strings.TrimPrefix(id, "arxiv:")
	id = strings.ReplaceAll(id, `\`, "/")
	if i := strings.LastIndex(id, "/"); i >= 0 {
		id = id[i+1:]
	}
	var b strings.Builder
	for _, r := range id {
		switch {
		case unicode.IsLetter(r) || unicode.IsDigit(r) || r == '.' || r == '-' || r == '_':
			b.WriteRune(r)
		case r == '/' || r == ' ':
			b.WriteByte('_')
		}
	}
	out := strings.Trim(b.String(), "._")
	if out == "." || out == ".." {
		return ""
	}
	return out
}

func isTranslateKind(kind string) bool {
	return kind == translateKindZH || kind == translateKindDual
}

func translatedPDFAbs(root, kind, id string) (string, error) {
	if root == "" {
		root = papersRoot()
	}
	kind = strings.ToLower(strings.TrimSpace(kind))
	id = sanitizePaperID(id)
	if !isTranslateKind(kind) || id == "" {
		return "", os.ErrNotExist
	}
	dir := filepath.Join(root, "pdfs", kind)
	candidates := []string{
		id + "." + kind + ".pdf",
		id + ".pdf",
	}
	for _, name := range candidates {
		full := filepath.Join(dir, name)
		st, err := os.Stat(full)
		if err != nil || st.IsDir() {
			continue
		}
		abs, err := filepath.Abs(full)
		if err != nil {
			continue
		}
		dirAbs, err := filepath.Abs(dir)
		if err != nil {
			continue
		}
		rel, err := filepath.Rel(dirAbs, abs)
		if err != nil || strings.HasPrefix(rel, "..") {
			continue
		}
		return abs, nil
	}
	return "", os.ErrNotExist
}

func translatedPDFURL(kind, id string) string {
	return "/papers/pdf/" + kind + "/" + id
}

// resolveTranslatedPDFName maps /papers/pdf/{zh|dual}/{id} to an absolute file.
func resolveTranslatedPDFName(root, kind, idOrName string) (absPath, filename string, err error) {
	if strings.Contains(idOrName, "..") {
		return "", "", os.ErrNotExist
	}
	id := sanitizePaperID(idOrName)
	if id == "" {
		return "", "", os.ErrNotExist
	}
	// Allow "{id}.zh.pdf" / "{id}.dual.pdf" in the URL.
	lower := strings.ToLower(id)
	for _, suf := range []string{".zh.pdf", ".dual.pdf", ".pdf"} {
		if strings.HasSuffix(lower, suf) {
			id = sanitizePaperID(id[:len(id)-len(suf)])
			break
		}
	}
	if id == "" {
		return "", "", os.ErrNotExist
	}
	abs, err := translatedPDFAbs(root, kind, id)
	if err != nil {
		return "", "", err
	}
	return abs, filepath.Base(abs), nil
}

func originalPDFAbs(root, filename string) (string, error) {
	if root == "" {
		root = papersRoot()
	}
	name, err := resolveLocalPDFName(root, filename)
	if err != nil || name == "" {
		return "", os.ErrNotExist
	}
	full, err := filepath.Abs(filepath.Join(root, "pdfs", name))
	if err != nil {
		return "", err
	}
	pdfRoot, err := filepath.Abs(filepath.Join(root, "pdfs"))
	if err != nil {
		return "", err
	}
	rel, err := filepath.Rel(pdfRoot, full)
	if err != nil || strings.HasPrefix(rel, "..") || strings.Contains(rel, string(filepath.Separator)) {
		// Originals live directly under pdfs/; reject zh/dual subdirs here.
		return "", os.ErrNotExist
	}
	st, err := os.Stat(full)
	if err != nil || st.IsDir() {
		return "", os.ErrNotExist
	}
	return full, nil
}

// collectBabelOutputs finds mono (Chinese-only) and dual PDFs in a BabelDOC work dir.
// Prefers no_watermark variants when both exist.
func collectBabelOutputs(workDir string) (mono, dual string) {
	ents, err := os.ReadDir(workDir)
	if err != nil {
		return "", ""
	}
	var monos, duals []string
	for _, e := range ents {
		if e.IsDir() {
			// BabelDOC sometimes writes into a nested folder.
			subMono, subDual := collectBabelOutputs(filepath.Join(workDir, e.Name()))
			if subMono != "" && mono == "" {
				mono = subMono
			}
			if subDual != "" && dual == "" {
				dual = subDual
			}
			continue
		}
		name := e.Name()
		if !strings.HasSuffix(strings.ToLower(name), ".pdf") {
			continue
		}
		full := filepath.Join(workDir, name)
		low := strings.ToLower(name)
		switch {
		case strings.Contains(low, ".dual.") || strings.HasSuffix(low, ".dual.pdf") || strings.Contains(low, "dual"):
			duals = append(duals, full)
		case strings.Contains(low, ".mono.") || strings.HasSuffix(low, ".mono.pdf") || strings.Contains(low, "mono"):
			monos = append(monos, full)
		}
	}
	if mono == "" {
		mono = preferNoWatermark(monos)
	}
	if dual == "" {
		dual = preferNoWatermark(duals)
	}
	return mono, dual
}

func preferNoWatermark(paths []string) string {
	if len(paths) == 0 {
		return ""
	}
	for _, p := range paths {
		if strings.Contains(strings.ToLower(filepath.Base(p)), "no_watermark") {
			return p
		}
	}
	return paths[0]
}

func installTranslatedPDFs(root, id, srcMono, srcDual string) (zhRel, dualRel string, err error) {
	id = sanitizePaperID(id)
	if id == "" {
		return "", "", os.ErrNotExist
	}
	if srcMono != "" {
		dst := filepath.Join(root, "pdfs", translateKindZH, id+".zh.pdf")
		if err := copyFile(srcMono, dst); err != nil {
			return "", "", err
		}
		zhRel = filepath.ToSlash(filepath.Join("pdfs", translateKindZH, id+".zh.pdf"))
	}
	if srcDual != "" {
		dst := filepath.Join(root, "pdfs", translateKindDual, id+".dual.pdf")
		if err := copyFile(srcDual, dst); err != nil {
			return zhRel, "", err
		}
		dualRel = filepath.ToSlash(filepath.Join("pdfs", translateKindDual, id+".dual.pdf"))
	}
	return zhRel, dualRel, nil
}

func copyFile(src, dst string) error {
	in, err := os.ReadFile(src)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(filepath.Dir(dst), ".pdf-*.tmp")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	ok := false
	defer func() {
		if !ok {
			_ = os.Remove(tmpName)
		}
	}()
	if _, err := tmp.Write(in); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if err := os.Rename(tmpName, dst); err != nil {
		return err
	}
	ok = true
	return nil
}
