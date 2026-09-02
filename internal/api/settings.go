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

func (s *Server) authorizeSettings(w http.ResponseWriter, r *http.Request) bool {
	if s == nil || s.auth == nil || !s.auth.Authorized(proxyauth.BearerToken(r)) {
		writeErr(w, http.StatusUnauthorized, "unauthorized", search.CodeUnauthorized, nil, "")
		return false
	}
	return true
}

func (s *Server) handleSettingsPage(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != "/settings" && r.URL.Path != "/settings/" {
		http.NotFound(w, r)
		return
	}
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		writeErr(w, http.StatusMethodNotAllowed, "method not allowed", search.CodeBadRequest, nil, "")
		return
	}
	if !s.authorizeSettings(w, r) {
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(http.StatusOK)
	if r.Method != http.MethodHead {
		_, _ = w.Write(settingsPageHTML)
	}
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
