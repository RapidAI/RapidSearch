package api

import (
	"bytes"
	"compress/zlib"
	"errors"
	"io"
	"os"
	"os/exec"
	"regexp"
	"strings"
	"unicode"
	"unicode/utf8"
)

const (
	importUploadMaxBytes = 80 << 20
	paperCheckMinRunes   = 400
	paperCheckMaxPages   = 8
)

// Machine-readable import rejection codes (ZH+EN copy lives in papers.html).
const (
	importErrNotPDF    = "not_pdf"
	importErrTooLarge  = "too_large"
	importErrTooSmall  = "too_small"
	importErrNotPaper  = "not_a_paper"
	importErrScanned   = "scanned"
	importErrNeedTag   = "tag_required"
	importErrNeedInput = "need_input"
)

var (
	errImportTag     = errors.New("tag is required and must be a known category key")
	errPDFTooLarge   = errors.New("pdf exceeds size limit")
	errPDFTooSmall   = errors.New("pdf is too small")
	errNotAPaper     = errors.New("not a paper")
	errScannedPDF    = errors.New("scanned pdf")
	errImportNeedSrc = errors.New("url_or_id or pdf file is required")

	rePDFPage     = regexp.MustCompile(`/Type\s*/Page`)
	rePDFImage    = regexp.MustCompile(`/Subtype\s*/Image`)
	rePDFTitle    = regexp.MustCompile(`/Title\s*\((?:\\.|[^\\)])*\)`)
	rePDFAuthor   = regexp.MustCompile(`/Author\s*\((?:\\.|[^\\)])*\)`)
	reFlateFilter = regexp.MustCompile(`/Filter\s*/FlateDecode|/FlateDecode`)
)

type importCheckError struct {
	Code string
	Msg  string
}

func (e *importCheckError) Error() string {
	if e == nil {
		return ""
	}
	if e.Msg != "" {
		return e.Msg
	}
	return e.Code
}

func (e *importCheckError) Unwrap() error {
	if e == nil {
		return nil
	}
	switch e.Code {
	case importErrNotPDF:
		return errNotPDF
	case importErrTooLarge:
		return errPDFTooLarge
	case importErrTooSmall:
		return errPDFTooSmall
	case importErrNotPaper:
		return errNotAPaper
	case importErrScanned:
		return errScannedPDF
	case importErrNeedTag:
		return errImportTag
	case importErrNeedInput:
		return errImportNeedSrc
	default:
		return nil
	}
}

func importCheckErr(code, msg string) error {
	return &importCheckError{Code: code, Msg: msg}
}

func requireImportTag(tag string) (string, error) {
	t, ok := validateTopicTag(tag)
	if !ok {
		return "", importCheckErr(importErrNeedTag, errImportTag.Error())
	}
	return t, nil
}

type extractedPaperMeta struct {
	Title    string
	Authors  []string
	Abstract string
	Year     int
	Text     string
	Pages    int
	Images   int
}

// checkAcademicPaperPDF rejects slides, posters, random PDFs, and image-only scans.
// A traditional paper needs extractable text plus several of Abstract / Introduction /
// References (or ZH equivalents) and a multi-page or sectioned body.
func checkAcademicPaperPDF(raw []byte) error {
	if _, err := inspectAcademicPaperPDF(raw); err != nil {
		return err
	}
	return nil
}

func inspectAcademicPaperPDF(raw []byte) (extractedPaperMeta, error) {
	var empty extractedPaperMeta
	if !isPDFBytes(raw) {
		return empty, importCheckErr(importErrNotPDF, "file is not a PDF")
	}
	if len(raw) > importUploadMaxBytes {
		return empty, importCheckErr(importErrTooLarge, "pdf exceeds size limit")
	}
	if len(raw) < 64 {
		return empty, importCheckErr(importErrTooSmall, "pdf is too small")
	}

	meta := extractPaperMetaFromPDF(raw, "")
	norm := normalizePaperText(meta.Text)
	runes := utf8.RuneCountInString(norm)
	if meta.Images >= 8 && runes < 300 {
		return meta, importCheckErr(importErrScanned, "pdf looks like a scanned image with no extractable paper text")
	}

	hasAbs := paperHasAny(norm, []string{"abstract"}, []string{"摘要"})
	hasIntro := paperHasAny(norm, []string{"introduction"}, []string{"引言"})
	hasRefs := paperHasAny(norm, []string{"references", "bibliography"}, []string{"参考文献"})
	hasSection := paperHasAny(norm, []string{
		"related work", "relatedwork", "methodology", "method", "conclusion",
		"discussion", "experiment", "experimental",
	}, []string{"相关工作", "方法", "结论", "实验"})

	markers := 0
	if hasAbs {
		markers++
	}
	if hasIntro {
		markers++
	}
	if hasRefs {
		markers++
	}

	// Traditional papers: enough body text and at least two structural markers.
	// One-page workshop papers may pass if they have all three headings.
	ok := runes >= paperCheckMinRunes && markers >= 2 && (meta.Pages >= 2 || markers >= 3 || hasSection)
	if !ok {
		return meta, importCheckErr(importErrNotPaper, "pdf does not look like a traditional academic paper")
	}
	return meta, nil
}

func extractPaperMetaFromPDF(raw []byte, uploadName string) extractedPaperMeta {
	text, pages, images := extractPDFText(raw)
	if better := extractPDFTextPdftotext(raw); better != "" && utf8.RuneCountInString(better) > utf8.RuneCountInString(text) {
		text = better
	}
	meta := extractedPaperMeta{
		Text:   collapseWS(text),
		Pages:  pages,
		Images: images,
	}
	if title := pdfLiteralValue(rePDFTitle.Find(raw)); title != "" && !looksLikePDFInternal(title) {
		meta.Title = collapseWS(title)
	}
	if auth := pdfLiteralValue(rePDFAuthor.Find(raw)); auth != "" {
		meta.Authors = splitAuthorList(auth)
	}
	if meta.Title == "" {
		meta.Title = inferTitleFromText(meta.Text)
	}
	if meta.Title == "" {
		meta.Title = titleFromUploadName(uploadName)
	}
	if meta.Title == "" {
		meta.Title = "Imported paper"
	}
	meta.Abstract = inferAbstractFromText(meta.Text)
	meta.Year = inferYearFromText(meta.Text)
	return meta
}

func extractPDFText(raw []byte) (text string, pages, images int) {
	pages = countPDFPages(raw)
	images = len(rePDFImage.FindAllIndex(raw, -1))
	var parts []string
	eachPDFStream(raw, func(dict, data []byte) {
		payload := data
		if reFlateFilter.Match(dict) {
			if inflated, err := zlibInflate(data); err == nil && len(inflated) > 0 {
				payload = inflated
			}
		}
		if s := extractPDFStrings(payload); s != "" {
			parts = append(parts, s)
		}
	})
	if s := extractPDFStrings(raw); s != "" {
		parts = append(parts, s)
	}
	return collapseWS(strings.Join(parts, " ")), pages, images
}

func countPDFPages(raw []byte) int {
	n := 0
	for _, m := range rePDFPage.FindAllIndex(raw, -1) {
		end := m[1]
		if end < len(raw) && isPDFNameChar(raw[end]) {
			continue // /Pages, /PageMode, …
		}
		n++
	}
	return n
}

func extractPDFTextPdftotext(raw []byte) string {
	bin, err := exec.LookPath("pdftotext")
	if err != nil {
		return ""
	}
	f, err := os.CreateTemp("", "papercheck-*.pdf")
	if err != nil {
		return ""
	}
	name := f.Name()
	_, werr := f.Write(raw)
	cerr := f.Close()
	defer os.Remove(name)
	if werr != nil || cerr != nil {
		return ""
	}
	cmd := exec.Command(bin, "-f", "1", "-l", "8", "-enc", "UTF-8", "-q", name, "-")
	out, err := cmd.Output()
	if err != nil || len(out) == 0 {
		return ""
	}
	return string(out)
}

func eachPDFStream(raw []byte, fn func(dict, data []byte)) {
	idx := 0
	for idx < len(raw) {
		i := bytes.Index(raw[idx:], []byte("stream"))
		if i < 0 {
			return
		}
		i += idx
		if i > 0 && isPDFNameChar(raw[i-1]) {
			idx = i + 6
			continue
		}
		after := i + 6
		if after < len(raw) && raw[after] == '\r' {
			after++
		}
		if after < len(raw) && raw[after] == '\n' {
			after++
		}
		j := bytes.Index(raw[after:], []byte("endstream"))
		if j < 0 {
			return
		}
		dictStart := i - 400
		if dictStart < 0 {
			dictStart = 0
		}
		fn(raw[dictStart:i], raw[after:after+j])
		idx = after + j + 9
	}
}

func isPDFNameChar(b byte) bool {
	return (b >= 'A' && b <= 'Z') || (b >= 'a' && b <= 'z') || (b >= '0' && b <= '9')
}

func zlibInflate(data []byte) ([]byte, error) {
	r, err := zlib.NewReader(bytes.NewReader(data))
	if err != nil {
		return nil, err
	}
	defer r.Close()
	return io.ReadAll(io.LimitReader(r, 2<<20))
}

func extractPDFStrings(b []byte) string {
	var out strings.Builder
	for i := 0; i < len(b); i++ {
		if b[i] != '(' {
			continue
		}
		s, next := readPDFLiteral(b, i)
		i = next - 1
		s = collapseWS(s)
		if utf8.RuneCountInString(s) < 2 || looksLikePDFInternal(s) {
			continue
		}
		if out.Len() > 0 {
			out.WriteByte(' ')
		}
		out.WriteString(s)
	}
	return out.String()
}

func readPDFLiteral(b []byte, start int) (string, int) {
	if start >= len(b) || b[start] != '(' {
		return "", start + 1
	}
	var out strings.Builder
	depth := 1
	i := start + 1
	for i < len(b) && depth > 0 {
		c := b[i]
		if c == '\\' && i+1 < len(b) {
			n := b[i+1]
			switch n {
			case 'n':
				out.WriteByte('\n')
			case 'r':
				out.WriteByte('\r')
			case 't':
				out.WriteByte('\t')
			case '(', ')', '\\':
				out.WriteByte(n)
			default:
				out.WriteByte(n)
			}
			i += 2
			continue
		}
		if c == '(' {
			depth++
			out.WriteByte(c)
			i++
			continue
		}
		if c == ')' {
			depth--
			if depth == 0 {
				return out.String(), i + 1
			}
			out.WriteByte(c)
			i++
			continue
		}
		out.WriteByte(c)
		i++
	}
	return out.String(), i
}

func pdfLiteralValue(match []byte) string {
	if len(match) == 0 {
		return ""
	}
	i := bytes.IndexByte(match, '(')
	if i < 0 {
		return ""
	}
	s, _ := readPDFLiteral(match, i)
	return collapseWS(s)
}

func looksLikePDFInternal(s string) bool {
	low := strings.ToLower(strings.TrimSpace(s))
	if low == "" {
		return true
	}
	switch low {
	case "pdf", "type", "font", "page", "pages", "helvetica", "times", "catalog",
		"filter", "flatedecode", "length", "obj", "endobj":
		return true
	}
	if strings.HasPrefix(low, "http://") || strings.HasPrefix(low, "https://") {
		return utf8.RuneCountInString(low) < 12
	}
	return false
}

func normalizePaperText(s string) string {
	s = strings.ToLower(collapseWS(s))
	return s
}

func paperHasAny(norm string, en []string, zh []string) bool {
	for _, w := range en {
		if w != "" && hasLatinPhrase(norm, w) {
			return true
		}
	}
	for _, w := range zh {
		if w != "" && strings.Contains(norm, w) {
			return true
		}
	}
	return false
}

func hasLatinPhrase(norm, phrase string) bool {
	phrase = strings.ToLower(strings.TrimSpace(phrase))
	if phrase == "" {
		return false
	}
	idx := 0
	for {
		i := strings.Index(norm[idx:], phrase)
		if i < 0 {
			return false
		}
		i += idx
		leftOK := i == 0 || !isLatinAlnum(rune(norm[i-1]))
		r := i + len(phrase)
		rightOK := r >= len(norm) || !isLatinAlnum(rune(norm[r]))
		if leftOK && rightOK {
			return true
		}
		idx = i + len(phrase)
	}
}

func isLatinAlnum(r rune) bool {
	return (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9')
}

func inferTitleFromText(text string) string {
	s := collapseWS(text)
	if s == "" {
		return ""
	}
	low := strings.ToLower(s)
	cut := len(s)
	if i := indexPhrase(low, "abstract"); i >= 0 && i < cut {
		cut = i
	}
	if i := strings.Index(s, "摘要"); i >= 0 && i < cut {
		cut = i
	}
	head := collapseWS(s[:cut])
	if n := utf8.RuneCountInString(head); n >= 12 {
		runes := []rune(head)
		if len(runes) > 160 {
			runes = runes[:160]
		}
		return strings.TrimSpace(string(runes))
	}
	return ""
}

func inferAbstractFromText(raw string) string {
	src := collapseWS(raw)
	low := strings.ToLower(src)
	start := -1
	for _, key := range []string{"abstract", "摘要"} {
		if i := indexPhrase(low, key); i >= 0 {
			start = i + len(key)
			break
		}
	}
	if start < 0 {
		return ""
	}
	rest := strings.TrimLeft(src[start:], " :.-")
	end := len(rest)
	restLow := strings.ToLower(rest)
	for _, key := range []string{"introduction", "引言", "1 introduction", "1. introduction"} {
		if i := indexPhrase(restLow, key); i >= 0 && i < end {
			end = i
		}
	}
	abs := collapseWS(rest[:end])
	if utf8.RuneCountInString(abs) > 1200 {
		abs = string([]rune(abs)[:1200])
	}
	if utf8.RuneCountInString(abs) < 40 {
		return ""
	}
	return abs
}

func indexPhrase(low, key string) int {
	if containsHan(key) {
		return strings.Index(low, key)
	}
	if hasLatinPhrase(low, key) {
		return strings.Index(low, key)
	}
	return -1
}

func containsHan(s string) bool {
	for _, r := range s {
		if unicode.Is(unicode.Han, r) {
			return true
		}
	}
	return false
}

func inferYearFromText(text string) int {
	re := regexp.MustCompile(`\b(19|20)\d{2}\b`)
	if m := re.FindString(text); m != "" {
		var y int
		for _, r := range m {
			y = y*10 + int(r-'0')
		}
		if y >= 1990 && y <= 2100 {
			return y
		}
	}
	return 0
}

func splitAuthorList(s string) []string {
	s = collapseWS(s)
	if s == "" {
		return nil
	}
	parts := strings.FieldsFunc(s, func(r rune) bool {
		return r == ';' || r == ',' || r == '/'
	})
	out := make([]string, 0, len(parts))
	for _, p := range parts {
		p = collapseWS(p)
		if p != "" {
			out = append(out, p)
		}
	}
	return out
}

func titleFromUploadName(name string) string {
	name = strings.TrimSpace(name)
	if name == "" {
		return ""
	}
	name = strings.ReplaceAll(name, "\\", "/")
	if i := strings.LastIndex(name, "/"); i >= 0 {
		name = name[i+1:]
	}
	if name == "." || name == ".." || name == "" {
		return ""
	}
	if strings.ContainsRune(name, 0) {
		return ""
	}
	base := name
	if i := strings.LastIndex(base, "."); i > 0 {
		base = base[:i]
	}
	base = strings.ReplaceAll(base, "_", " ")
	base = strings.ReplaceAll(base, "-", " ")
	base = strings.Join(strings.Fields(base), " ")
	if base == "" || strings.EqualFold(base, "pdf") {
		return ""
	}
	return base
}
