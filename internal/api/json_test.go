package api

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"search-service/internal/search"
)

func TestWriteErrJSONHasOKFalseAndCode(t *testing.T) {
	rr := httptest.NewRecorder()
	writeErr(rr, http.StatusForbidden, "search blocked by captcha", search.CodeCaptcha, []string{"google"}, "google")
	if rr.Code != 403 {
		t.Fatalf("status=%d", rr.Code)
	}
	var body errBody
	if err := json.Unmarshal(rr.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	if body.OK {
		t.Fatalf("ok should be false: %+v", body)
	}
	if body.Code != "captcha" || body.Error == "" || body.Engine != "google" {
		t.Fatalf("body=%+v", body)
	}
	if len(body.Tried) != 1 || body.Tried[0] != "google" {
		t.Fatalf("tried=%v", body.Tried)
	}
}

func TestSuccessBodyJSONHasOKTrue(t *testing.T) {
	rr := httptest.NewRecorder()
	writeJSON(rr, http.StatusOK, successBody{
		OK:              true,
		Query:           "golang",
		Engine:          "bing",
		RequestedEngine: "auto",
		Tried:           []string{"bing"},
		Skipped:         []string{"google"},
		Results:         []search.Result{{Rank: 1, Title: "Go", URL: "https://go.dev", Snippet: "Go"}},
		Count:           1,
	})
	var body map[string]interface{}
	if err := json.Unmarshal(rr.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	if body["ok"] != true {
		t.Fatalf("ok=%v", body["ok"])
	}
	skipped, _ := body["skipped"].([]interface{})
	if len(skipped) != 1 || skipped[0] != "google" {
		t.Fatalf("skipped=%v", body["skipped"])
	}
	if _, ok := body["results"]; !ok {
		t.Fatal("missing results")
	}
}

func TestBadRequestNotEmptyResults(t *testing.T) {
	s := &Server{mux: http.NewServeMux(), breaker: search.NewGoogleBreaker()}
	s.mux.HandleFunc("/search", s.handleSearch)
	rr := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/search", nil)
	s.ServeHTTP(rr, req)
	if rr.Code != http.StatusBadRequest {
		t.Fatalf("status=%d body=%s", rr.Code, rr.Body.String())
	}
	var body errBody
	if err := json.Unmarshal(rr.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	if body.OK || body.Code != search.CodeBadRequest {
		t.Fatalf("body=%+v", body)
	}
}
