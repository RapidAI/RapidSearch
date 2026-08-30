package download

import (
	"net/url"
	"path"
	"strings"
	"unicode"
)

// ValidateURL accepts only http/https with a host. file/javascript/data/etc are rejected.
func ValidateURL(raw string) (*url.URL, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return nil, errBadURL
	}
	u, err := url.Parse(raw)
	if err != nil || u.Scheme == "" || u.Host == "" {
		return nil, errBadURL
	}
	switch strings.ToLower(u.Scheme) {
	case "http", "https":
	default:
		return nil, errBadURL
	}
	host := strings.ToLower(u.Hostname())
	if host == "" || host == "unix" {
		return nil, errBadURL
	}
	return u, nil
}

func safeFilename(cd, rawURL string) string {
	if name := filenameFromCD(cd); name != "" {
		return name
	}
	if u, err := url.Parse(rawURL); err == nil {
		base := path.Base(u.Path)
		if n := sanitizeName(base); n != "" {
			return n
		}
	}
	return "download"
}

func filenameFromCD(cd string) string {
	if cd == "" {
		return ""
	}
	// filename*=UTF-8''...
	low := strings.ToLower(cd)
	if i := strings.Index(low, "filename*="); i >= 0 {
		rest := cd[i+len("filename*="):]
		if j := strings.IndexByte(rest, ';'); j >= 0 {
			rest = rest[:j]
		}
		rest = strings.Trim(rest, ` "'`)
		if k := strings.Index(rest, "''"); k >= 0 {
			rest = rest[k+2:]
		}
		if n := sanitizeName(rest); n != "" {
			return n
		}
	}
	if i := strings.Index(low, "filename="); i >= 0 {
		rest := cd[i+len("filename="):]
		if j := strings.IndexByte(rest, ';'); j >= 0 {
			rest = rest[:j]
		}
		rest = strings.Trim(rest, ` "'`)
		if n := sanitizeName(rest); n != "" {
			return n
		}
	}
	return ""
}

func sanitizeName(name string) string {
	name = strings.TrimSpace(name)
	if name == "" {
		return ""
	}
	// strip any path
	if i := strings.LastIndexAny(name, `/\`); i >= 0 {
		name = name[i+1:]
	}
	name = strings.Map(func(r rune) rune {
		if r < 32 || r == 127 || r == '"' {
			return -1
		}
		return r
	}, name)
	name = strings.TrimSpace(name)
	if name == "" || name == "." || name == ".." {
		return ""
	}
	// keep it short
	runes := []rune(name)
	if len(runes) > 180 {
		name = string(runes[:180])
	}
	if !hasVisible(name) {
		return ""
	}
	return name
}

func hasVisible(s string) bool {
	for _, r := range s {
		if unicode.IsLetter(r) || unicode.IsDigit(r) {
			return true
		}
	}
	return false
}

func shortURL(raw string) string {
	const max = 120
	if len(raw) <= max {
		return raw
	}
	return raw[:max] + "…"
}
