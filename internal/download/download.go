package download

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"log"
	"net"
	"net/http"
	"net/http/cookiejar"
	"net/url"
	"strings"
	"time"

	"search-service/internal/browser"
	"search-service/internal/cache"
)

var (
	errBadURL   = errors.New("url must be http or https")
	errTooLarge = errors.New("file too large")
)

const (
	handlerTimeout = 3 * time.Minute
	maxRedirects   = 10
	maxTries       = 3
	chromeMaxBytes = 16 << 20
	blobMax        = 2 << 20
	desktopUA      = "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/120.0.0.0 Safari/537.36"
)

type Downloader struct {
	mgr   *browser.Manager
	cache *cache.Cache
	seen  *Recent
	hc    *http.Client
}

func New(mgr *browser.Manager, c *cache.Cache) *Downloader {
	jar, _ := cookiejar.New(nil)
	tr := &http.Transport{
		Proxy:                 http.ProxyFromEnvironment,
		DialContext:           (&net.Dialer{Timeout: 20 * time.Second, KeepAlive: 30 * time.Second}).DialContext,
		TLSHandshakeTimeout:   15 * time.Second,
		ResponseHeaderTimeout: 30 * time.Second,
		ExpectContinueTimeout: 1 * time.Second,
		ForceAttemptHTTP2:     true,
		MaxIdleConns:          16,
	}
	return &Downloader{
		mgr:   mgr,
		cache: c,
		seen:  NewRecent(time.Hour),
		hc: &http.Client{
			Transport: tr,
			Jar:       jar,
			Timeout:   handlerTimeout,
			CheckRedirect: func(req *http.Request, via []*http.Request) error {
				if len(via) >= maxRedirects {
					return errors.New("too many redirects")
				}
				return nil
			},
		},
	}
}

func (d *Downloader) RememberSearchURLs(urls []string) {
	if d != nil && d.seen != nil {
		d.seen.Remember(urls)
	}
}

func (d *Downloader) Timeout() time.Duration { return handlerTimeout }

type postBody struct {
	URL string `json:"url"`
}

func ParseRequestURL(r *http.Request) (string, error) {
	switch r.Method {
	case http.MethodGet, http.MethodHead:
		q := strings.TrimSpace(r.URL.Query().Get("url"))
		if q == "" {
			return "", errBadURL
		}
		return q, nil
	case http.MethodPost:
		defer r.Body.Close()
		var body postBody
		if err := json.NewDecoder(io.LimitReader(r.Body, 1<<20)).Decode(&body); err != nil {
			return "", errBadURL
		}
		if strings.TrimSpace(body.URL) == "" {
			return "", errBadURL
		}
		return body.URL, nil
	default:
		return "", errBadURL
	}
}

func (d *Downloader) Serve(ctx context.Context, w http.ResponseWriter, r *http.Request) {
	raw, err := ParseRequestURL(r)
	if err != nil {
		writeErr(w, http.StatusBadRequest, "url must be http or https", "bad_request")
		return
	}
	u, err := ValidateURL(raw)
	if err != nil {
		writeErr(w, http.StatusBadRequest, "url must be http or https", "bad_request")
		return
	}
	raw = u.String()

	fs, _, _ := d.disk()
	maxBytes := cache.MaxObjectBytes(fs.Free)
	if maxBytes <= 0 {
		writeErr(w, http.StatusInsufficientStorage, "not enough disk headroom", "too_large")
		return
	}

	rangeHdr := r.Header.Get("Range")
	useBlob := rangeHdr == "" && r.Method != http.MethodHead
	blobID := blobKey(raw)
	if useBlob && d.cache != nil {
		if data, ok := d.cache.GetBlob(blobID); ok {
			name := safeFilename("", raw)
			w.Header().Set("Content-Type", http.DetectContentType(data))
			w.Header().Set("Content-Disposition", `attachment; filename="`+name+`"`)
			w.Header().Set("X-Download-Cache", "1")
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write(data)
			log.Printf("download cache=hit url=%s bytes=%d", shortURL(raw), len(data))
			return
		}
	}

	log.Printf("download start url=%s range=%q seen=%v", shortURL(raw), rangeHdr, d.seen.Has(raw))

	res, via, err := d.fetch(ctx, raw, rangeHdr, maxBytes)
	if err != nil {
		if errors.Is(err, errTooLarge) {
			writeErr(w, http.StatusRequestEntityTooLarge, "file exceeds size cap", "too_large")
			return
		}
		if errors.Is(err, context.DeadlineExceeded) || errors.Is(err, context.Canceled) {
			writeErr(w, http.StatusGatewayTimeout, "download timed out", "timeout")
			return
		}
		log.Printf("download fail url=%s err=%v", shortURL(raw), err)
		writeErr(w, http.StatusBadGateway, "download failed", "fetch")
		return
	}
	defer res.Body.Close()

	if cl := res.ContentLength; cl > 0 && cl > maxBytes {
		writeErr(w, http.StatusRequestEntityTooLarge, "file exceeds size cap", "too_large")
		return
	}

	ct := res.Header.Get("Content-Type")
	if ct == "" {
		ct = "application/octet-stream"
	}
	cd := res.Header.Get("Content-Disposition")
	name := safeFilename(cd, raw)
	if cd == "" {
		cd = `attachment; filename="` + name + `"`
	}

	if r.Method == http.MethodHead {
		w.Header().Set("Content-Type", ct)
		w.Header().Set("Content-Disposition", cd)
		if res.Header.Get("Content-Length") != "" {
			w.Header().Set("Content-Length", res.Header.Get("Content-Length"))
		}
		if res.Header.Get("Accept-Ranges") != "" {
			w.Header().Set("Accept-Ranges", res.Header.Get("Accept-Ranges"))
		}
		w.WriteHeader(res.StatusCode)
		return
	}

	w.Header().Set("Content-Type", ct)
	w.Header().Set("Content-Disposition", cd)
	if v := res.Header.Get("Content-Length"); v != "" {
		w.Header().Set("Content-Length", v)
	}
	if v := res.Header.Get("Content-Range"); v != "" {
		w.Header().Set("Content-Range", v)
	}
	if v := res.Header.Get("Accept-Ranges"); v != "" {
		w.Header().Set("Accept-Ranges", v)
	} else if rangeHdr == "" {
		w.Header().Set("Accept-Ranges", "bytes")
	}
	w.Header().Set("X-Download-Via", via)
	status := res.StatusCode
	if status == 0 {
		status = http.StatusOK
	}
	w.WriteHeader(status)

	limited := &capReader{R: res.Body, N: maxBytes}
	var buf []byte
	if useBlob && (res.ContentLength <= 0 || res.ContentLength <= blobMax) {
		buf, err = io.ReadAll(io.LimitReader(limited, blobMax+1))
		if err != nil && !errors.Is(err, errTooLarge) {
			log.Printf("download read url=%s via=%s err=%v", shortURL(raw), via, err)
			return
		}
		if int64(len(buf)) > maxBytes {
			return
		}
		if _, err := w.Write(buf); err != nil {
			return
		}
		if fl, ok := w.(http.Flusher); ok {
			fl.Flush()
		}
		if useBlob && int64(len(buf)) <= blobMax && status < 300 && d.cache != nil {
			d.cache.PutBlob(blobID, buf)
		}
		log.Printf("download ok url=%s via=%s status=%d bytes=%d", shortURL(raw), via, status, len(buf))
		return
	}

	n, err := io.Copy(w, limited)
	if err != nil && !errors.Is(err, errTooLarge) {
		log.Printf("download stream url=%s via=%s copied=%d err=%v", shortURL(raw), via, n, err)
		return
	}
	log.Printf("download ok url=%s via=%s status=%d bytes=%d", shortURL(raw), via, status, n)
}

func (d *Downloader) disk() (cache.FSInfo, int64, int64) {
	if d.cache != nil {
		return d.cache.Disk()
	}
	info, err := cache.Statfs("/")
	if err != nil {
		return cache.FSInfo{}, 0, 0
	}
	return info, cache.ComputeBudget(info.Size, info.Free), 0
}

func (d *Downloader) fetch(ctx context.Context, raw, rangeHdr string, maxBytes int64) (*http.Response, string, error) {
	if d.seen != nil && d.seen.Has(raw) && d.mgr != nil {
		if cookies := exportCookies(ctx, d.mgr, raw); len(cookies) > 0 {
			if u, err := url.Parse(raw); err == nil && d.hc.Jar != nil {
				d.hc.Jar.SetCookies(u, cookies)
			}
		}
	}

	res, err := d.httpFetch(ctx, raw, rangeHdr)
	if err == nil && res != nil && !shouldChrome(res) {
		return res, "http", nil
	}
	if res != nil {
		_ = res.Body.Close()
	}
	if d.mgr == nil {
		if err != nil {
			return nil, "", err
		}
		return nil, "", errors.New("upstream rejected")
	}
	log.Printf("download chrome-fallback url=%s reason=%v", shortURL(raw), err)
	cres, err := chromeFetch(ctx, d.mgr, raw, rangeHdr, min64(maxBytes, chromeMaxBytes))
	if err != nil {
		return nil, "", err
	}
	return cres, "chrome", nil
}

func (d *Downloader) httpFetch(ctx context.Context, raw, rangeHdr string) (*http.Response, error) {
	var last error
	for attempt := 1; attempt <= maxTries; attempt++ {
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, raw, nil)
		if err != nil {
			return nil, err
		}
		req.Header.Set("User-Agent", desktopUA)
		req.Header.Set("Accept", "*/*")
		req.Header.Set("Accept-Language", "en-US,en;q=0.9")
		if rangeHdr != "" {
			req.Header.Set("Range", rangeHdr)
		}
		res, err := d.hc.Do(req)
		if err != nil {
			last = err
			if !retryableNet(err) || attempt == maxTries {
				return nil, err
			}
			backoff(ctx, attempt)
			continue
		}
		if retryableStatus(res.StatusCode) && attempt < maxTries {
			_ = res.Body.Close()
			last = errors.New(res.Status)
			backoff(ctx, attempt)
			continue
		}
		return res, nil
	}
	if last == nil {
		last = errors.New("download failed")
	}
	return nil, last
}

func shouldChrome(res *http.Response) bool {
	if res == nil {
		return true
	}
	if res.StatusCode == http.StatusForbidden || res.StatusCode == http.StatusTooManyRequests {
		return true
	}
	if res.StatusCode >= 400 {
		return false
	}
	if res.ContentLength == 0 {
		return true
	}
	ct := strings.ToLower(res.Header.Get("Content-Type"))
	if strings.Contains(ct, "text/html") || ct == "" {
		// peek later via a small read? we don't want to consume the body.
		// treat 200 html as OK (example.com is html). Chrome only on 403/empty.
		return false
	}
	return false
}

func retryableStatus(code int) bool {
	return code == 429 || code >= 500
}

func retryableNet(err error) bool {
	if err == nil {
		return false
	}
	if errors.Is(err, context.DeadlineExceeded) || errors.Is(err, context.Canceled) {
		return false
	}
	var ne net.Error
	if errors.As(err, &ne) && (ne.Timeout() || ne.Temporary()) {
		return true
	}
	s := strings.ToLower(err.Error())
	return strings.Contains(s, "connection reset") ||
		strings.Contains(s, "broken pipe") ||
		strings.Contains(s, "eof") ||
		strings.Contains(s, "timeout")
}

func backoff(ctx context.Context, attempt int) {
	d := time.Duration(attempt) * 400 * time.Millisecond
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
	case <-t.C:
	}
}

func blobKey(raw string) string {
	sum := sha256.Sum256([]byte("dl|" + raw))
	return hex.EncodeToString(sum[:])
}

type capReader struct {
	R io.Reader
	N int64
	n int64
}

func (c *capReader) Read(p []byte) (int, error) {
	n, err := c.R.Read(p)
	c.n += int64(n)
	if c.N > 0 && c.n > c.N {
		return n, errTooLarge
	}
	return n, err
}

func min64(a, b int64) int64 {
	if a < b {
		return a
	}
	return b
}

func writeErr(w http.ResponseWriter, status int, msg, code string) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(map[string]string{"error": msg, "code": code})
}
