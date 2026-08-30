package cache

import (
	"crypto/sha256"
	"encoding/hex"
	"strconv"
	"strings"
	"unicode"
)

// KeyInput is the request-side identity of a cached search.
// content=0 and content=1 are different keys.
type KeyInput struct {
	Query    string
	Engine   string
	Limit    int
	Content  bool
	Region   string
	Locale   string
	HL       string
	Fallback bool
}

// NormalizeQuery trims, lowercases, and collapses whitespace.
func NormalizeQuery(q string) string {
	return strings.Join(strings.FieldsFunc(strings.ToLower(strings.TrimSpace(q)), unicode.IsSpace), " ")
}

func normField(s string) string {
	return strings.ToLower(strings.TrimSpace(s))
}

func bool01(b bool) string {
	if b {
		return "1"
	}
	return "0"
}

// Canonical is the stable preimage hashed into a key. Exported for tests.
func Canonical(in KeyInput) string {
	var b strings.Builder
	b.WriteString("v2|")
	b.WriteString(NormalizeQuery(in.Query))
	b.WriteByte('|')
	b.WriteString(normField(in.Engine))
	b.WriteByte('|')
	b.WriteString(strconv.Itoa(in.Limit))
	b.WriteByte('|')
	b.WriteString(bool01(in.Content))
	b.WriteByte('|')
	b.WriteString(normField(in.Region))
	b.WriteByte('|')
	b.WriteString(normField(in.Locale))
	b.WriteByte('|')
	b.WriteString(normField(in.HL))
	b.WriteByte('|')
	b.WriteString(bool01(in.Fallback))
	return b.String()
}

// Key returns a hex SHA-256 of the canonical request identity.
func Key(in KeyInput) string {
	sum := sha256.Sum256([]byte(Canonical(in)))
	return hex.EncodeToString(sum[:])
}
