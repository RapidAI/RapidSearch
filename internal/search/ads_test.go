package search

import (
	"context"
	"strings"
	"testing"
)

func TestDropSponsoredChineseTitle(t *testing.T) {
	in := []Result{
		{Rank: 1, Title: "广告 · 某商品", URL: "https://shop.example.com/item/1", Snippet: "golang http server 官方教程配套"},
		{Rank: 2, Title: "net/http - Go", URL: "https://pkg.go.dev/net/http", Snippet: "Package http provides HTTP client and server implementations."},
	}
	out := Preprocess(context.Background(), in, PreprocessOpts{
		Query: "golang http server", Engine: "bing", Limit: 10,
	})
	for _, r := range out {
		if strings.Contains(r.Title, "某商品") || strings.HasPrefix(r.Title, "广告") {
			t.Fatalf("sponsored title kept: %+v", r)
		}
	}
	if len(out) == 0 {
		t.Fatal("organic hit should remain")
	}
}

func TestDropGoogleAdServicesURL(t *testing.T) {
	in := []Result{
		{Rank: 1, Title: "Buy golang book", URL: "https://www.googleadservices.com/pagead/aclk?adurl=https://example.com", Snippet: "golang http server course"},
		{Rank: 2, Title: "Click ad", URL: "https://www.google.com/aclk?sa=L&ai=foo", Snippet: "golang http server"},
		{Rank: 3, Title: "Writing HTTP servers in Go", URL: "https://go.dev/doc/articles/wiki/", Snippet: "Building a wiki with the net/http server."},
	}
	out := Preprocess(context.Background(), in, PreprocessOpts{
		Query: "golang http server", Engine: "google", Limit: 10,
	})
	for _, r := range out {
		lu := strings.ToLower(r.URL)
		if strings.Contains(lu, "googleadservices") || strings.Contains(lu, "/aclk") {
			t.Fatalf("ad url kept: %+v", r)
		}
	}
	if len(out) != 1 {
		t.Fatalf("want 1 organic, got %+v", out)
	}
}

func TestKeepOrganicAdvertisingArticle(t *testing.T) {
	in := []Result{
		{Rank: 1, Title: "How advertising works", URL: "https://journal.example.com/how-advertising-works", Snippet: "A guide to how advertising works in modern media."},
	}
	out := Preprocess(context.Background(), in, PreprocessOpts{
		Query: "how advertising works", Engine: "bing", Limit: 10,
	})
	if len(out) != 1 {
		t.Fatalf("organic advertising article dropped: %+v", out)
	}
	if out[0].URL != "https://journal.example.com/how-advertising-works" {
		t.Fatalf("wrong hit: %+v", out[0])
	}
}

func TestKeepAdobeOrganic(t *testing.T) {
	in := []Result{
		{Rank: 1, Title: "Adobe Acrobat PDF software", URL: "https://www.adobe.com/acrobat.html", Snippet: "Create, edit, and sign PDFs with Adobe Acrobat."},
	}
	out := Preprocess(context.Background(), in, PreprocessOpts{
		Query: "adobe acrobat", Engine: "google", Limit: 10,
	})
	if len(out) != 1 {
		t.Fatalf("adobe.com organic dropped: %+v", out)
	}
}

func TestDropBingSponsoredSnippet(t *testing.T) {
	in := []Result{
		{Rank: 1, Title: "Super Widgets", URL: "https://shop.example.com/widgets", Snippet: "Sponsored · Buy now"},
		{Rank: 2, Title: "Widget design patterns", URL: "https://docs.example.com/widgets", Snippet: "How to design reusable UI widgets."},
	}
	out := Preprocess(context.Background(), in, PreprocessOpts{
		Query: "widget design patterns", Engine: "bing", Limit: 10,
	})
	for _, r := range out {
		if strings.Contains(strings.ToLower(r.Snippet), "sponsored") {
			t.Fatalf("bing sponsored snippet kept: %+v", r)
		}
		if strings.Contains(r.URL, "shop.example.com") {
			t.Fatalf("sponsored shop kept: %+v", r)
		}
	}
	if len(out) != 1 {
		t.Fatalf("want organic widget docs, got %+v", out)
	}
}

func TestKeepQueryContainingGuangGao(t *testing.T) {
	in := []Result{
		{Rank: 1, Title: "广告设计教程", URL: "https://design.example.com/ad-class", Snippet: "从零学习广告设计构图与字体。"},
	}
	out := Preprocess(context.Background(), in, PreprocessOpts{
		Query: "广告设计", Engine: "baidu", Limit: 10,
	})
	if len(out) != 1 {
		t.Fatalf("organic 广告设计 page dropped: %+v", out)
	}
}

func TestAdminConsoleNotTreatedAsAd(t *testing.T) {
	in := []Result{
		{Rank: 1, Title: "Admin console overview", URL: "https://admin.example.com/console", Snippet: "Manage users from the admin console."},
	}
	out := Preprocess(context.Background(), in, PreprocessOpts{
		Query: "admin console", Engine: "bing", Limit: 10,
	})
	if len(out) != 1 {
		t.Fatalf("admin console dropped as ad: %+v", out)
	}
}
