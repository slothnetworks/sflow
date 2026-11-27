package main

import (
	"archive/tar"
	"bufio"
	"compress/gzip"
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// Identify ourselves to the upstream data sources.
const userAgent = "NetMeta-geoip/1.0 (+https://github.com/leoluk/NetMeta)"

func newHTTPClient() *http.Client {
	return &http.Client{
		// Databases are up to ~100 MB; the RIB dump is parsed while it streams in.
		Timeout: 15 * time.Minute,
		Transport: &http.Transport{
			Proxy:                 http.ProxyFromEnvironment,
			DialContext:           (&net.Dialer{Timeout: 10 * time.Second, KeepAlive: 30 * time.Second}).DialContext,
			TLSHandshakeTimeout:   10 * time.Second,
			ResponseHeaderTimeout: 30 * time.Second,
			IdleConnTimeout:       90 * time.Second,
			MaxIdleConns:          4,
		},
	}
}

type statusError struct {
	URL    string
	Status string
}

func (e *statusError) Error() string {
	return fmt.Sprintf("GET %s: unexpected status %s", e.URL, e.Status)
}

// redactURL removes credentials (userinfo and all query values, e.g. IPinfo's token or a presigned redirect's
// signature) from a URL so it can be logged.
func redactURL(s string) string {
	u, err := url.Parse(s)
	if err != nil {
		return "<invalid url>"
	}
	u.User = nil
	if u.RawQuery != "" {
		q := u.Query()
		for k := range q {
			q.Set(k, "REDACTED")
		}
		u.RawQuery = q.Encode()
	}
	return u.String()
}

func newRequest(ctx context.Context, u string) (*http.Request, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return nil, fmt.Errorf("invalid URL %s", redactURL(u))
	}
	req.Header.Set("User-Agent", userAgent)
	return req, nil
}

// download performs req and, if the response is 200, writes the body through transform into a temporary file next
// to dst. If validate (optional) accepts the file, it atomically replaces dst. dst is left untouched on any error.
func download(c *http.Client, req *http.Request, dst string, transform func(io.Reader, io.Writer) error,
	validate func(path string) error) error {
	resp, err := c.Do(req)
	if err != nil {
		var ue *url.Error
		if errors.As(err, &ue) {
			ue.URL = redactURL(ue.URL)
		}
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 64<<10))
		return &statusError{URL: redactURL(req.URL.String()), Status: resp.Status}
	}

	tmp, err := os.CreateTemp(filepath.Dir(dst), filepath.Base(dst)+".*.tmp")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name()) // no-op after a successful rename

	bw := bufio.NewWriterSize(tmp, 1<<20)
	err = transform(resp.Body, bw)
	if err == nil {
		err = bw.Flush()
	}
	if cerr := tmp.Close(); err == nil {
		err = cerr
	}
	if err == nil && validate != nil {
		err = validate(tmp.Name())
	}
	if err != nil {
		return err
	}
	return os.Rename(tmp.Name(), dst)
}

func copyTransform(r io.Reader, w io.Writer) error {
	_, err := io.Copy(w, r)
	return err
}

// extractMMDB copies the first *.mmdb file in a tar.gz stream to w.
func extractMMDB(r io.Reader, w io.Writer) error {
	gz, err := gzip.NewReader(r)
	if err != nil {
		return err
	}
	tr := tar.NewReader(gz)
	for {
		h, err := tr.Next()
		if err == io.EOF {
			return errors.New("no .mmdb file in archive")
		}
		if err != nil {
			return err
		}
		if h.Typeflag == tar.TypeReg && strings.HasSuffix(h.Name, ".mmdb") {
			_, err := io.Copy(w, tr)
			return err
		}
	}
}

// stale reports whether path is missing or older than maxAge.
func stale(path string, maxAge time.Duration, now func() time.Time) bool {
	fi, err := os.Stat(path)
	return err != nil || now().Sub(fi.ModTime()) >= maxAge
}

// existing returns those of paths that exist.
func existing(paths ...string) []string {
	var out []string
	for _, p := range paths {
		if _, err := os.Stat(p); err == nil {
			out = append(out, p)
		}
	}
	return out
}
