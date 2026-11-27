// geoip is an optional loader for IP -> ASN, IP -> country and ASN -> name data. It downloads databases from the
// configured providers, merges them by priority and writes the result into the geo.asn, geo.country and
// geo.asn_names ClickHouse tables, which back the geo_asn, geo_country and autnums dictionaries.
//
// With GEOIP_PROVIDERS empty (the default), it does nothing: no network calls, no ClickHouse writes.
package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"time"
)

// provider is a source of geo data.
type provider interface {
	Name() string
	// Fetch downloads the provider's files into its cache directory if they are missing or stale. It reports whether
	// anything was downloaded. On error, the previously cached files are kept.
	Fetch(ctx context.Context) (bool, error)
	// Files lists the cached files that Load reads.
	Files() []string
	// Load parses the cached files. It makes no network calls.
	Load(ctx context.Context) (*dataset, error)
}

type rowCounts struct {
	ASN      int `json:"asn"`
	Country  int `json:"country"`
	ASNNames int `json:"asn_names"`
}

func countRows(ds *dataset) rowCounts {
	return rowCounts{ASN: len(ds.ASN), Country: len(ds.Country), ASNNames: len(ds.Names)}
}

type providerStatus struct {
	Name           string     `json:"name"`
	LastFetch      *time.Time `json:"last_fetch,omitempty"`
	LastError      string     `json:"last_error,omitempty"`
	LastErrorAt    *time.Time `json:"last_error_at,omitempty"`
	LastLoad       *time.Time `json:"last_load,omitempty"`
	Rows           rowCounts  `json:"rows"`
	lastFetchError time.Time
}

type status struct {
	Providers      []*providerStatus `json:"providers"`
	LastWrite      *time.Time        `json:"last_write,omitempty"`
	LastWriteError string            `json:"last_write_error,omitempty"`
	Rows           rowCounts         `json:"rows"`
}

const (
	// After a failed fetch, a provider is not retried for this long.
	fetchRetryInterval = 30 * time.Minute
	minWriteBackoff    = 5 * time.Second
	maxWriteBackoff    = 5 * time.Minute
)

type service struct {
	providers []provider
	writer    writer
	refresh   time.Duration
	now       func() time.Time

	mu      sync.Mutex
	status  status
	lastSig string // signature of the files last written to ClickHouse
}

func newService(providers []provider, w writer, refresh time.Duration) *service {
	s := &service{providers: providers, writer: w, refresh: refresh, now: time.Now}
	s.status.Providers = []*providerStatus{}
	for _, p := range providers {
		s.status.Providers = append(s.status.Providers, &providerStatus{Name: p.Name()})
	}
	return s
}

func ptr(t time.Time) *time.Time { return &t }

// signature identifies the set of cached files and their versions.
func (s *service) signature() string {
	var b strings.Builder
	for _, p := range s.providers {
		for _, f := range p.Files() {
			if fi, err := os.Stat(f); err == nil {
				fmt.Fprintf(&b, "%s|%d|%d\n", f, fi.Size(), fi.ModTime().UnixNano())
			}
		}
	}
	return b.String()
}

// cycle fetches stale provider data and, if any cached file changed since the last successful write (or nothing
// was written yet), loads and merges all providers and writes the result. It returns an error only if loading or
// writing failed; fetch failures are recorded in the status and retried after fetchRetryInterval.
func (s *service) cycle(ctx context.Context) error {
	if len(s.providers) == 0 {
		return nil
	}
	for i, p := range s.providers {
		st := s.status.Providers[i]
		s.mu.Lock()
		skip := !st.lastFetchError.IsZero() && s.now().Sub(st.lastFetchError) < fetchRetryInterval
		s.mu.Unlock()
		if skip {
			continue
		}
		fetched, err := p.Fetch(ctx)
		if ctx.Err() != nil {
			return ctx.Err()
		}
		s.mu.Lock()
		if err != nil {
			slog.Error("fetch failed, keeping previous data", "provider", p.Name(), "error", err)
			st.LastError, st.LastErrorAt, st.lastFetchError = err.Error(), ptr(s.now()), s.now()
		} else {
			st.lastFetchError = time.Time{}
			if fetched {
				st.LastFetch = ptr(s.now())
				st.LastError, st.LastErrorAt = "", nil
			}
		}
		s.mu.Unlock()
	}

	sig := s.signature()
	s.mu.Lock()
	unchanged := sig == s.lastSig
	s.mu.Unlock()
	if sig == "" || unchanged {
		return nil
	}

	sets := make([]*dataset, len(s.providers))
	for i, p := range s.providers {
		ds, err := p.Load(ctx)
		if err != nil {
			return fmt.Errorf("loading %s: %w", p.Name(), err)
		}
		sets[i] = ds
		s.mu.Lock()
		s.status.Providers[i].LastLoad = ptr(s.now())
		s.status.Providers[i].Rows = countRows(ds)
		s.mu.Unlock()
		slog.Info("loaded provider data", "provider", p.Name(), "asn", len(ds.ASN), "country", len(ds.Country),
			"asn_names", len(ds.Names))
	}
	merged := merge(sets...)
	if merged.empty() {
		return nil
	}

	err := s.writer.Write(ctx, merged)
	s.mu.Lock()
	defer s.mu.Unlock()
	if err != nil {
		s.status.LastWriteError = err.Error()
		return fmt.Errorf("writing to ClickHouse: %w", err)
	}
	s.lastSig = sig
	s.status.LastWrite, s.status.LastWriteError, s.status.Rows = ptr(s.now()), "", countRows(merged)
	return nil
}

// run executes cycles until ctx is cancelled: hourly (or every GEOIP_REFRESH, if shorter), and with exponential
// backoff after a failed load or ClickHouse write. Only one cycle runs at a time.
func (s *service) run(ctx context.Context) {
	if len(s.providers) == 0 {
		slog.Info("no providers configured (GEOIP_PROVIDERS is empty), nothing to do")
		return
	}
	interval := min(time.Hour, s.refresh)
	var backoff time.Duration
	for {
		next := interval
		if err := s.cycle(ctx); err != nil && ctx.Err() == nil {
			backoff = min(max(backoff*2, minWriteBackoff), maxWriteBackoff)
			next = backoff
			slog.Error("refresh failed", "error", err, "retryIn", backoff)
		} else {
			backoff = 0
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(next):
		}
	}
}

func (s *service) handleStatus(w http.ResponseWriter, _ *http.Request) {
	s.mu.Lock()
	b, err := json.MarshalIndent(&s.status, "", "  ")
	s.mu.Unlock()
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	_, _ = w.Write(append(b, '\n'))
}

func (s *service) routes() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) { _, _ = io.WriteString(w, "ok\n") })
	mux.HandleFunc("GET /status", s.handleStatus)
	return mux
}

type config struct {
	Listen            string
	Providers         []string
	DataDir           string
	FilesDir          string
	Refresh           time.Duration
	MaxMindAccountID  string
	MaxMindLicenseKey string
	MaxMindEditions   []string
	IPinfoToken       string
	RIBBaseURL        string
	ASNamesURL        string
	ClickHouseAddr    string
	ClickHouseUser    string
	ClickHousePass    string
	Dictionaries      []string
}

func env(getenv func(string) string, key, def string) string {
	if v := strings.TrimSpace(getenv(key)); v != "" {
		return v
	}
	return def
}

func splitList(s string) []string {
	var out []string
	for _, f := range strings.Split(s, ",") {
		if f = strings.TrimSpace(f); f != "" {
			out = append(out, f)
		}
	}
	return out
}

func loadConfig(getenv func(string) string) (*config, error) {
	c := &config{
		Listen:            env(getenv, "GEOIP_LISTEN", ":8080"),
		Providers:         splitList(strings.ToLower(getenv("GEOIP_PROVIDERS"))),
		DataDir:           env(getenv, "GEOIP_DATA_DIR", "/var/lib/netmeta-geoip"),
		FilesDir:          env(getenv, "GEOIP_FILES_DIR", "/etc/netmeta/geoip"),
		MaxMindAccountID:  env(getenv, "MAXMIND_ACCOUNT_ID", ""),
		MaxMindLicenseKey: env(getenv, "MAXMIND_LICENSE_KEY", ""),
		MaxMindEditions:   splitList(env(getenv, "MAXMIND_EDITIONS", "GeoLite2-ASN,GeoLite2-Country")),
		IPinfoToken:       env(getenv, "IPINFO_TOKEN", ""),
		RIBBaseURL:        env(getenv, "ROUTEVIEWS_RIB_URL", defaultRIBBaseURL),
		ASNamesURL:        env(getenv, "ROUTEVIEWS_ASNAMES_URL", defaultASNamesURL),
		ClickHouseAddr:    env(getenv, "CLICKHOUSE_ADDR", "clickhouse:9000"),
		ClickHouseUser:    env(getenv, "CLICKHOUSE_USER", "admin"),
		ClickHousePass:    getenv("CLICKHOUSE_PASSWORD"),
		Dictionaries:      splitList(env(getenv, "GEOIP_DICTIONARIES", "geo_asn,geo_country,autnums")),
	}
	var err error
	if c.Refresh, err = time.ParseDuration(env(getenv, "GEOIP_REFRESH", "24h")); err != nil || c.Refresh <= 0 {
		return nil, fmt.Errorf("invalid GEOIP_REFRESH %q", getenv("GEOIP_REFRESH"))
	}
	seen := map[string]bool{}
	for _, p := range c.Providers {
		if seen[p] {
			return nil, fmt.Errorf("provider %q listed twice in GEOIP_PROVIDERS", p)
		}
		seen[p] = true
		switch p {
		case "maxmind":
			if c.MaxMindAccountID == "" || c.MaxMindLicenseKey == "" {
				return nil, errors.New("provider maxmind requires MAXMIND_ACCOUNT_ID and MAXMIND_LICENSE_KEY")
			}
			if len(c.MaxMindEditions) == 0 {
				return nil, errors.New("MAXMIND_EDITIONS is empty")
			}
		case "ipinfo":
			if c.IPinfoToken == "" {
				return nil, errors.New("provider ipinfo requires IPINFO_TOKEN")
			}
		case "files", "routeviews":
		default:
			return nil, fmt.Errorf("unknown provider %q in GEOIP_PROVIDERS (want maxmind, ipinfo, files, routeviews)", p)
		}
	}
	return c, nil
}

// buildProviders instantiates the configured providers in priority order.
func buildProviders(c *config, client *http.Client) []provider {
	var out []provider
	for _, name := range c.Providers {
		dir := filepath.Join(c.DataDir, name)
		switch name {
		case "maxmind":
			out = append(out, &maxmind{dir: dir, baseURL: "https://download.maxmind.com/geoip/databases",
				accountID: c.MaxMindAccountID, key: c.MaxMindLicenseKey, editions: c.MaxMindEditions,
				maxAge: c.Refresh, client: client, now: time.Now})
		case "ipinfo":
			out = append(out, &ipinfo{dir: dir, baseURL: "https://ipinfo.io/data/ipinfo_lite.mmdb",
				token: c.IPinfoToken, maxAge: c.Refresh, client: client, now: time.Now})
		case "files":
			out = append(out, &files{dir: c.FilesDir})
		case "routeviews":
			out = append(out, &routeviews{dir: dir, ribBaseURL: c.RIBBaseURL, asnamesURL: c.ASNamesURL,
				maxAge: c.Refresh, client: client, now: time.Now})
		}
	}
	return out
}

func main() {
	level := slog.LevelInfo
	if strings.EqualFold(os.Getenv("GEOIP_LOG_LEVEL"), "debug") {
		level = slog.LevelDebug
	}
	slog.SetDefault(slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: level})))

	cfg, err := loadConfig(os.Getenv)
	if err != nil {
		slog.Error("invalid configuration", "error", err)
		os.Exit(2)
	}
	svc := newService(buildProviders(cfg, newHTTPClient()),
		newCHWriter(cfg.ClickHouseAddr, cfg.ClickHouseUser, cfg.ClickHousePass, cfg.Dictionaries), cfg.Refresh)

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		svc.run(ctx)
	}()

	srv := &http.Server{
		Addr:              cfg.Listen,
		Handler:           svc.routes(),
		ReadHeaderTimeout: 10 * time.Second,
		WriteTimeout:      30 * time.Second,
		IdleTimeout:       2 * time.Minute,
	}
	errc := make(chan error, 1)
	go func() {
		slog.Info("listening", "addr", cfg.Listen, "providers", cfg.Providers)
		errc <- srv.ListenAndServe()
	}()

	select {
	case err := <-errc:
		slog.Error("server failed", "error", err)
		stop()
		wg.Wait()
		os.Exit(1)
	case <-ctx.Done():
	}
	slog.Info("shutting down")
	shutdownCtx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	if err := srv.Shutdown(shutdownCtx); err != nil {
		slog.Error("shutdown failed", "error", err)
	}
	wg.Wait()
}
