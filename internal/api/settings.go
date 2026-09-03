package api

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"strconv"
	"strings"

	_ "embed"

	"search-service/internal/proxyauth"
	"search-service/internal/search"
)

//go:embed settings.html
var settingsPageHTML []byte

//go:embed login.html
var loginPageHTML []byte

const settingsCookieMaxAge = 12 * 60 * 60

func (s *Server) authorizeSettings(w http.ResponseWriter, r *http.Request) bool {
	if !s.settingsAuthed(r) {
		writeErr(w, http.StatusUnauthorized, "unauthorized", search.CodeUnauthorized, nil, "")
		return false
	}
	return true
}

func (s *Server) settingsAuthed(r *http.Request) bool {
	if s == nil || s.auth == nil {
		return false
	}
	// Operator Bearer / ?token= may still be SEARCH_TOKEN. The browser
	// cookie is admin-only (GET {HUB}/api/admin/users), never models.
	if tok := proxyauth.BearerToken(r); tok != "" {
		return s.auth.SettingsAuthorized(tok)
	}
	return s.auth.AdminValid(proxyauth.CookieToken(r))
}

func (s *Server) handleSettingsPage(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != "/settings" && r.URL.Path != "/settings/" {
		http.NotFound(w, r)
		return
	}
	switch r.Method {
	case http.MethodGet, http.MethodHead:
		if !s.settingsAuthed(r) {
			writeSettingsHTML(w, r, loginPageHTML)
			return
		}
		writeSettingsHTML(w, r, settingsPageHTML)
	case http.MethodPost:
		s.handleSettingsLogin(w, r)
	default:
		writeErr(w, http.StatusMethodNotAllowed, "method not allowed", search.CodeBadRequest, nil, "")
	}
}

func (s *Server) handleSettingsLogin(w http.ResponseWriter, r *http.Request) {
	if r.Method == http.MethodGet || r.Method == http.MethodHead {
		if s.settingsAuthed(r) {
			writeSettingsHTML(w, r, settingsPageHTML)
			return
		}
		writeSettingsHTML(w, r, loginPageHTML)
		return
	}
	if r.Method != http.MethodPost {
		writeErr(w, http.StatusMethodNotAllowed, "method not allowed", search.CodeBadRequest, nil, "")
		return
	}
	user, pass := parseSettingsLogin(r)
	got := s.loginHubToken(user, pass)
	if got == "" {
		if wantsJSON(r) {
			writeErr(w, http.StatusUnauthorized, "invalid Hub global admin account", search.CodeUnauthorized, nil, "")
			return
		}
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.Header().Set("Cache-Control", "no-store")
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = w.Write(loginPageHTML)
		return
	}
	setSettingsCookie(w, r, got)
	if wantsJSON(r) {
		writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
		return
	}
	writeSettingsHTML(w, r, settingsPageHTML)
}

func (s *Server) handleSettingsLogout(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet && r.Method != http.MethodHead && r.Method != http.MethodPost {
		writeErr(w, http.StatusMethodNotAllowed, "method not allowed", search.CodeBadRequest, nil, "")
		return
	}
	clearSettingsCookie(w, r)
	if wantsJSON(r) {
		writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
		return
	}
	writeSettingsHTML(w, r, loginPageHTML)
}

func (s *Server) loginHubToken(user, pass string) string {
	if s == nil || s.auth == nil {
		return ""
	}
	if user == "" || pass == "" {
		return ""
	}
	return s.auth.HubPasswordLogin(user, pass)
}

func (s *Server) handleSettingsConfig(w http.ResponseWriter, r *http.Request) {
	if !s.authorizeSettings(w, r) {
		return
	}
	switch r.Method {
	case http.MethodGet:
		s.writeSettingsPublic(w)
	case http.MethodPut:
		s.putSettingsConfig(w, r)
	default:
		writeErr(w, http.StatusMethodNotAllowed, "method not allowed", search.CodeBadRequest, nil, "")
	}
}

func (s *Server) writeSettingsPublic(w http.ResponseWriter) {
	var view search.PublicView
	if s.cfg != nil {
		view = s.cfg.Public()
	} else {
		view = (&search.Store{}).Public()
		view.ConfigPath = search.ConfigPath()
	}
	writeJSON(w, http.StatusOK, view)
}

func (s *Server) putSettingsConfig(w http.ResponseWriter, r *http.Request) {
	if s.cfg == nil {
		writeErr(w, http.StatusBadGateway, "search config is not available", search.CodeEngine, nil, "")
		return
	}
	defer r.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(r.Body, 1<<20))
	if err != nil {
		writeErr(w, http.StatusBadRequest, "invalid body", search.CodeBadRequest, nil, "")
		return
	}
	var patch search.SettingsPatch
	if len(strings.TrimSpace(string(raw))) > 0 {
		if err := json.Unmarshal(raw, &patch); err != nil {
			writeErr(w, http.StatusBadRequest, "invalid json body", search.CodeBadRequest, nil, "")
			return
		}
	}
	if err := s.cfg.ApplyPatch(patch); err != nil {
		writeErr(w, http.StatusBadRequest, err.Error(), search.CodeEngine, nil, "")
		return
	}
	s.writeSettingsPublic(w)
}

func writeSettingsHTML(w http.ResponseWriter, r *http.Request, page []byte) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(http.StatusOK)
	if r == nil || r.Method != http.MethodHead {
		_, _ = w.Write(applyHTMLLang(page, acceptLang(r)))
	}
}

func acceptLang(r *http.Request) string {
	if r == nil {
		return "en"
	}
	bestZh, bestOther := -1.0, -1.0
	for _, part := range strings.Split(r.Header.Get("Accept-Language"), ",") {
		part = strings.TrimSpace(part)
		if part == "" {
			continue
		}
		q := 1.0
		bits := strings.Split(part, ";")
		tag := strings.ToLower(strings.TrimSpace(bits[0]))
		for _, b := range bits[1:] {
			b = strings.TrimSpace(strings.ToLower(b))
			if strings.HasPrefix(b, "q=") {
				if v, err := strconv.ParseFloat(strings.TrimSpace(b[2:]), 64); err == nil {
					q = v
				}
			}
		}
		if strings.HasPrefix(tag, "zh") {
			if q > bestZh {
				bestZh = q
			}
			continue
		}
		if q > bestOther {
			bestOther = q
		}
	}
	if bestZh >= 0 && bestZh >= bestOther {
		return "zh"
	}
	return "en"
}

func applyHTMLLang(page []byte, lang string) []byte {
	if lang != "zh" {
		lang = "en"
	}
	page = bytes.Replace(page, []byte(`data-accept-lang="en"`), []byte(`data-accept-lang="`+lang+`"`), 1)
	if lang == "zh" {
		page = bytes.Replace(page, []byte(`<html lang="en"`), []byte(`<html lang="zh-CN"`), 1)
	}
	return page
}

func parseSettingsLogin(r *http.Request) (user, pass string) {
	if r == nil {
		return "", ""
	}
	ct := strings.ToLower(r.Header.Get("Content-Type"))
	if strings.Contains(ct, "application/json") {
		defer r.Body.Close()
		var body struct {
			Username string `json:"username"`
			Email    string `json:"email"`
			Password string `json:"password"`
		}
		_ = json.NewDecoder(io.LimitReader(r.Body, 1<<20)).Decode(&body)
		user = strings.TrimSpace(firstNonEmpty(body.Username, body.Email))
		pass = body.Password
		return user, pass
	}
	_ = r.ParseForm()
	user = strings.TrimSpace(firstNonEmpty(r.FormValue("username"), r.FormValue("email")))
	pass = r.FormValue("password")
	return user, pass
}

func firstNonEmpty(vals ...string) string {
	for _, v := range vals {
		if strings.TrimSpace(v) != "" {
			return strings.TrimSpace(v)
		}
	}
	return ""
}

func wantsJSON(r *http.Request) bool {
	if r == nil {
		return false
	}
	ct := strings.ToLower(r.Header.Get("Content-Type"))
	if strings.Contains(ct, "application/json") {
		return true
	}
	accept := strings.ToLower(r.Header.Get("Accept"))
	return strings.Contains(accept, "application/json") && !strings.Contains(accept, "text/html")
}

func cookieSecure(r *http.Request) bool {
	if r != nil && r.TLS != nil {
		return true
	}
	if r == nil {
		return false
	}
	return strings.EqualFold(strings.TrimSpace(r.Header.Get("X-Forwarded-Proto")), "https")
}

func setSettingsCookie(w http.ResponseWriter, r *http.Request, token string) {
	http.SetCookie(w, &http.Cookie{
		Name:     proxyauth.SettingsCookie,
		Value:    token,
		Path:     "/",
		MaxAge:   settingsCookieMaxAge,
		HttpOnly: true,
		Secure:   cookieSecure(r),
		SameSite: http.SameSiteLaxMode,
	})
}

func clearSettingsCookie(w http.ResponseWriter, r *http.Request) {
	http.SetCookie(w, &http.Cookie{
		Name:     proxyauth.SettingsCookie,
		Value:    "",
		Path:     "/",
		MaxAge:   -1,
		HttpOnly: true,
		Secure:   cookieSecure(r),
		SameSite: http.SameSiteLaxMode,
	})
}
