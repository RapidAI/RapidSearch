package download

import (
	"bytes"
	"context"
	"encoding/base64"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/go-rod/rod"

	"search-service/internal/browser"
)

func exportCookies(ctx context.Context, mgr *browser.Manager, raw string) []*http.Cookie {
	if mgr == nil {
		return nil
	}
	ctx, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()
	var out []*http.Cookie
	err := mgr.Do(ctx, func(page *rod.Page) error {
		u, err := url.Parse(raw)
		if err != nil {
			return err
		}
		cs, err := page.Cookies([]string{u.Scheme + "://" + u.Host + "/"})
		if err != nil {
			return err
		}
		for _, c := range cs {
			hc := &http.Cookie{
				Name:     c.Name,
				Value:    c.Value,
				Path:     c.Path,
				Domain:   strings.TrimPrefix(c.Domain, "."),
				Secure:   c.Secure,
				HttpOnly: c.HTTPOnly,
			}
			out = append(out, hc)
		}
		return nil
	})
	if err != nil {
		log.Printf("download cookie-export url=%s err=%v", shortURL(raw), err)
	}
	return out
}

type chromeResult struct {
	Status             int    `json:"status"`
	ContentType        string `json:"contentType"`
	ContentDisposition string `json:"contentDisposition"`
	BodyB64            string `json:"bodyB64"`
	Error              string `json:"error"`
}

func chromeFetch(ctx context.Context, mgr *browser.Manager, raw, rangeHdr string, maxBytes int64) (*http.Response, error) {
	if maxBytes <= 0 {
		maxBytes = chromeMaxBytes
	}
	var cr chromeResult
	err := mgr.Do(ctx, func(page *rod.Page) error {
		page = page.Context(ctx)
		if u, err := url.Parse(raw); err == nil && u.Host != "" {
			origin := u.Scheme + "://" + u.Host + "/"
			_ = page.Timeout(20 * time.Second).Navigate(origin)
		}
		js := `async (url, rangeHdr, maxBytes) => {
			const hdrs = {};
			if (rangeHdr) hdrs['Range'] = rangeHdr;
			const r = await fetch(url, {credentials:'include', redirect:'follow', headers: hdrs});
			const len = Number(r.headers.get('content-length') || '0');
			if (len && len > maxBytes) {
				return {status: r.status, contentType: r.headers.get('content-type')||'', contentDisposition: r.headers.get('content-disposition')||'', error: 'too large'};
			}
			const buf = await r.arrayBuffer();
			if (buf.byteLength > maxBytes) {
				return {status: r.status, error: 'too large'};
			}
			const bytes = new Uint8Array(buf);
			let binary = '';
			const chunk = 0x8000;
			for (let i = 0; i < bytes.length; i += chunk) {
				binary += String.fromCharCode.apply(null, bytes.subarray(i, i+chunk));
			}
			return {
				status: r.status,
				contentType: r.headers.get('content-type')||'',
				contentDisposition: r.headers.get('content-disposition')||'',
				bodyB64: btoa(binary)
			};
		}`
		val, err := page.Eval(js, raw, rangeHdr, maxBytes)
		if err != nil {
			return err
		}
		if err := val.Value.Unmarshal(&cr); err != nil {
			return err
		}
		if cr.Error == "too large" {
			return errTooLarge
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	body, err := base64.StdEncoding.DecodeString(cr.BodyB64)
	if err != nil {
		return nil, fmt.Errorf("chrome body: %w", err)
	}
	if int64(len(body)) > maxBytes {
		return nil, errTooLarge
	}
	hdr := make(http.Header)
	if cr.ContentType != "" {
		hdr.Set("Content-Type", cr.ContentType)
	} else {
		hdr.Set("Content-Type", "application/octet-stream")
	}
	if cr.ContentDisposition != "" {
		hdr.Set("Content-Disposition", cr.ContentDisposition)
	}
	status := cr.Status
	if status == 0 {
		status = http.StatusOK
	}
	return &http.Response{
		StatusCode:    status,
		Header:        hdr,
		Body:          io.NopCloser(bytes.NewReader(body)),
		ContentLength: int64(len(body)),
	}, nil
}
