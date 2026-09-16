package api

import (
	"net/http"
	"regexp"
	"sort"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"search-service/internal/search"
)

const (
	reviewMaxRefs       = 20
	reviewMinTitleRunes = 12
	reviewMinTitleWords = 2
)

// paperReviewRef is a catalog paper mentioned by a review.
// Tunnel-friendly: id, short title, and flags only — no abstracts.
type paperReviewRef struct {
	PaperID   string `json:"paper_id"`
	ArxivID   string `json:"arxiv_id,omitempty"`
	Title     string `json:"title,omitempty"`
	HasReview bool   `json:"has_review"`
	Match     string `json:"match,omitempty"` // "arxiv_id" or "title"
}

var (
	reArxivModern = regexp.MustCompile(`(?i)(?:https?://(?:www\.)?arxiv\.org/(?:abs|pdf)/|arxiv:)?(\d{4}\.\d{4,5})(?:v\d+)?`)
	reArxivOld    = regexp.MustCompile(`(?i)(?:https?://(?:www\.)?arxiv\.org/(?:abs|pdf)/|arxiv:)?([a-z][a-z\-]+(?:\.[a-z]{2})?/\d{7})(?:v\d+)?`)
)

func reviewAnalysisText(a paperReviewAnalysis) string {
	parts := []string{a.MethodPrinciples, a.MethodEssence, a.Experiment, a.Quality}
	var b strings.Builder
	for _, p := range parts {
		p = strings.TrimSpace(p)
		if p == "" {
			continue
		}
		if b.Len() > 0 {
			b.WriteString("\n\n")
		}
		b.WriteString(p)
	}
	return b.String()
}

func stripArxivVersion(id string) string {
	id = strings.TrimSpace(id)
	if id == "" {
		return ""
	}
	low := strings.ToLower(id)
	i := strings.LastIndex(low, "v")
	if i <= 0 || i == len(id)-1 {
		return id
	}
	rest := id[i+1:]
	for _, r := range rest {
		if r < '0' || r > '9' {
			return id
		}
	}
	return id[:i]
}

func normalizeCatalogArxiv(id string) string {
	id = sanitizeArxivID(id)
	id = stripArxivVersion(id)
	id = strings.ToLower(strings.ReplaceAll(id, "/", "_"))
	return strings.Trim(id, "._")
}

func extractArxivIDs(text string) []string {
	if strings.TrimSpace(text) == "" {
		return nil
	}
	seen := map[string]bool{}
	var out []string
	add := func(raw string) {
		raw = stripArxivVersion(strings.TrimSpace(raw))
		if raw == "" {
			return
		}
		key := normalizeCatalogArxiv(raw)
		if key == "" || seen[key] {
			return
		}
		seen[key] = true
		out = append(out, raw)
	}
	for _, m := range reArxivModern.FindAllStringSubmatch(text, -1) {
		if len(m) > 1 {
			add(m[1])
		}
	}
	for _, m := range reArxivOld.FindAllStringSubmatch(text, -1) {
		if len(m) > 1 {
			add(m[1])
		}
	}
	return out
}

func normalizeTitleKey(s string) string {
	var b strings.Builder
	prevSpace := true
	for _, r := range strings.ToLower(strings.TrimSpace(s)) {
		switch {
		case unicode.IsLetter(r) || unicode.IsDigit(r):
			b.WriteRune(r)
			prevSpace = false
		case unicode.IsSpace(r) || unicode.IsPunct(r) || unicode.IsSymbol(r):
			if !prevSpace && b.Len() > 0 {
				b.WriteByte(' ')
				prevSpace = true
			}
		}
	}
	return strings.TrimSpace(b.String())
}

func titleSearchable(title string) bool {
	n := normalizeTitleKey(title)
	if n == "" {
		return false
	}
	nrunes := utf8.RuneCountInString(n)
	if nrunes < reviewMinTitleRunes {
		return false
	}
	words := strings.Fields(n)
	if len(words) >= reviewMinTitleWords {
		return true
	}
	return nrunes >= 20
}

func lastRuneBefore(s string, byteIdx int) (rune, bool) {
	if byteIdx <= 0 || byteIdx > len(s) {
		return 0, false
	}
	r, _ := utf8.DecodeLastRuneInString(s[:byteIdx])
	if r == utf8.RuneError {
		return 0, false
	}
	return r, true
}

func firstRuneAt(s string, byteIdx int) (rune, bool) {
	if byteIdx < 0 || byteIdx >= len(s) {
		return 0, false
	}
	r, _ := utf8.DecodeRuneInString(s[byteIdx:])
	if r == utf8.RuneError {
		return 0, false
	}
	return r, true
}

func isWordRune(r rune, ok bool) bool {
	return ok && (unicode.IsLetter(r) || unicode.IsDigit(r))
}

func boundedPhrase(hay, phrase string) bool {
	if phrase == "" || hay == "" {
		return false
	}
	start := 0
	for start <= len(hay)-len(phrase) {
		i := strings.Index(hay[start:], phrase)
		if i < 0 {
			return false
		}
		i += start
		leftOK := !isWordRune(lastRuneBefore(hay, i))
		rightOK := !isWordRune(firstRuneAt(hay, i+len(phrase)))
		if leftOK && rightOK {
			return true
		}
		start = i + 1
	}
	return false
}

func titleHasASCIIWord(titleKey string) bool {
	for _, r := range titleKey {
		if r < utf8.RuneSelf && (unicode.IsLetter(r) || unicode.IsDigit(r)) {
			return true
		}
	}
	return false
}

func titleMentioned(raw, lowerRaw, normHay, title string) bool {
	title = strings.TrimSpace(title)
	if title == "" {
		return false
	}
	if strings.Contains(lowerRaw, strings.ToLower(title)) {
		return true
	}
	nt := normalizeTitleKey(title)
	if nt == "" || !titleSearchable(title) {
		return false
	}
	if titleHasASCIIWord(nt) {
		return boundedPhrase(normHay, nt)
	}
	return strings.Contains(normHay, nt)
}

func catalogPaperID(p paperEntry) string {
	if id := sanitizePaperID(p.ID); id != "" {
		return id
	}
	return paperTranslateID(p)
}

func lookupCatalogPaper(papers []paperEntry, rawID string) (paperEntry, bool) {
	key := normalizeCatalogArxiv(rawID)
	alt := sanitizePaperID(rawID)
	if key == "" && alt == "" {
		return paperEntry{}, false
	}
	for _, p := range papers {
		if key != "" {
			if normalizeCatalogArxiv(p.ArxivID) == key || normalizeCatalogArxiv(p.ID) == key {
				return p, true
			}
		}
		if alt != "" && catalogPaperID(p) == alt {
			return p, true
		}
	}
	return paperEntry{}, false
}

func makeReviewRef(p paperEntry, match string) paperReviewRef {
	id := catalogPaperID(p)
	arxiv := strings.TrimSpace(p.ArxivID)
	if arxiv == "" {
		arxiv = strings.TrimSpace(p.ID)
	}
	title := strings.TrimSpace(p.Title)
	if utf8.RuneCountInString(title) > 180 {
		title = string([]rune(title)[:180])
	}
	return paperReviewRef{
		PaperID:   id,
		ArxivID:   arxiv,
		Title:     title,
		HasReview: p.HasReview,
		Match:     match,
	}
}

func matchReviewRefs(text, selfID string, papers []paperEntry) []paperReviewRef {
	selfID = sanitizePaperID(selfID)
	seen := map[string]bool{}
	if selfID != "" {
		seen[selfID] = true
	}
	var out []paperReviewRef
	add := func(p paperEntry, match string) {
		id := catalogPaperID(p)
		if id == "" || seen[id] {
			return
		}
		seen[id] = true
		out = append(out, makeReviewRef(p, match))
	}

	for _, raw := range extractArxivIDs(text) {
		p, ok := lookupCatalogPaper(papers, raw)
		if ok {
			add(p, "arxiv_id")
		}
	}

	cands := append([]paperEntry(nil), papers...)
	sort.SliceStable(cands, func(i, j int) bool {
		return utf8.RuneCountInString(strings.TrimSpace(cands[i].Title)) >
			utf8.RuneCountInString(strings.TrimSpace(cands[j].Title))
	})
	lower := strings.ToLower(text)
	normHay := normalizeTitleKey(text)
	for _, p := range cands {
		title := strings.TrimSpace(p.Title)
		if !titleSearchable(title) {
			continue
		}
		if !titleMentioned(text, lower, normHay, title) {
			continue
		}
		add(p, "title")
	}
	if len(out) > reviewMaxRefs {
		out = out[:reviewMaxRefs]
	}
	return out
}

func catalogPapersForRefs(ps *papersStore) []paperEntry {
	if ps == nil {
		return nil
	}
	cat, err := ps.catalogBase()
	if err != nil {
		return nil
	}
	papers := append([]paperEntry(nil), cat.Papers...)
	if ps.reviews != nil {
		ps.reviews.overlay(papers)
	}
	return papers
}

func (s *reviewService) applyRefs(rec *paperReviewFile, catalog []paperEntry) {
	if rec == nil {
		return
	}
	rec.Refs = matchReviewRefs(reviewAnalysisText(rec.Analysis), rec.PaperID, catalog)
	if rec.Refs == nil {
		rec.Refs = []paperReviewRef{}
	}
	rec.RefsUpdatedAt = time.Now().UTC().Format(time.RFC3339)
}

func (s *reviewService) refreshRefs(id string, catalog []paperEntry) (paperReviewFile, error) {
	id = sanitizePaperID(id)
	if s == nil || id == "" {
		return paperReviewFile{}, errReviewNotReady
	}
	m := s.lockID(id)
	m.Lock()
	defer m.Unlock()
	rec, ok := s.load(id)
	if !ok || !rec.hasAnalysis() {
		return paperReviewFile{}, errReviewNotReady
	}
	s.applyRefs(&rec, catalog)
	if err := s.persist(&rec); err != nil {
		return paperReviewFile{}, err
	}
	return rec, nil
}

func (s *Server) handlePapersReviewRefs(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeErr(w, http.StatusMethodNotAllowed, "method not allowed", search.CodeBadRequest, nil, "")
		return
	}
	id := sanitizePaperID(r.PathValue("id"))
	if id == "" {
		writeErr(w, http.StatusBadRequest, "invalid paper id", search.CodeBadRequest, nil, "")
		return
	}
	ps := s.papers()
	if ps == nil || ps.reviews == nil {
		writeErr(w, http.StatusNotFound, "review not found", "papers", nil, "")
		return
	}
	rater := s.ensureRater(w, r)
	rec, err := ps.reviews.refreshRefs(id, catalogPapersForRefs(ps))
	if err != nil {
		if err == errReviewNotReady {
			writeErr(w, http.StatusConflict, errReviewNotReady.Error(), search.CodeBusy, nil, "")
			return
		}
		writeErr(w, http.StatusBadGateway, "could not update references", search.CodeEngine, nil, "")
		return
	}
	writeJSON(w, http.StatusOK, rec.public(rater, false))
}
