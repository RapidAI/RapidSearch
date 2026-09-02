package api

import (
	"encoding/json"
	"io"
	"net/http"
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
	if s == nil || s.auth == nil || !s.auth.Authorized(proxyauth.RequestToken(r)) {
		writeErr(w, http.StatusUnauthorized, "unauthorized", search.CodeUnauthorized, nil, "")
		return false
	}
	return true
}

func (s *Server) settingsAuthed(r *http.Request) bool {
	return s != nil && s.auth != nil && s.auth.Authorized(proxyauth.RequestToken(r))
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
	user, pass, token := parseSettingsLogin(r)
	got := s.loginHubToken(user, pass, token)
	if got == "" {
		if wantsJSON(r) {
			writeErr(w, http.StatusUnauthorized, "invalid Hub account or viewer token", search.CodeUnauthorized, nil, "")
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

func (s *Server) loginHubToken(user, pass, token string) string {
	if s == nil || s.auth == nil {
		return ""
	}
	token = strings.TrimSpace(token)
	if token != "" && s.auth.HubValid(token) {
		return token
	}
	if user != "" && pass != "" {
		if got := s.auth.HubPasswordLogin(user, pass); got != "" {
			return got
		}
	}
	return ""
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
		_, _ = w.Write(page)
	}
}

func parseSettingsLogin(r *http.Request) (user, pass, token string) {
	if r == nil {
		return "", "", ""
	}
	ct := strings.ToLower(r.Header.Get("Content-Type"))
	if strings.Contains(ct, "application/json") {
		defer r.Body.Close()
		var body struct {
			Username string `json:"username"`
			Email    string `json:"email"`
			Password string `json:"password"`
			Token    string `json:"token"`
		}
		_ = json.NewDecoder(io.LimitReader(r.Body, 1<<20)).Decode(&body)
		user = strings.TrimSpace(firstNonEmpty(body.Username, body.Email))
		pass = body.Password
		token = strings.TrimSpace(body.Token)
		return user, pass, token
	}
	_ = r.ParseForm()
	user = strings.TrimSpace(firstNonEmpty(r.FormValue("username"), r.FormValue("email")))
	pass = r.FormValue("password")
	token = strings.TrimSpace(r.FormValue("token"))
	return user, pass, token
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
