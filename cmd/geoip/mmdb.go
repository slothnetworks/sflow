package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/netip"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/oschwald/maxminddb-golang/v2"
)

type asnRow struct {
	Network string
	ASN     uint32
}

type countryRow struct {
	Network string
	Country string
}

type nameRow struct {
	ASN     uint32
	Name    string
	Country string
}

// dataset is the content of the geo.asn, geo.country and geo.asn_names tables.
type dataset struct {
	ASN     []asnRow
	Country []countryRow
	Names   []nameRow
}

func (d *dataset) empty() bool { return len(d.ASN) == 0 && len(d.Country) == 0 && len(d.Names) == 0 }

// merge combines datasets in priority order: for each network (or ASN, for names), the first dataset wins.
func merge(sets ...*dataset) *dataset {
	out := &dataset{}
	seenASN := map[string]struct{}{}
	seenCountry := map[string]struct{}{}
	seenName := map[uint32]struct{}{}
	for _, s := range sets {
		if s == nil {
			continue
		}
		for _, r := range s.ASN {
			if _, ok := seenASN[r.Network]; !ok {
				seenASN[r.Network] = struct{}{}
				out.ASN = append(out.ASN, r)
			}
		}
		for _, r := range s.Country {
			if _, ok := seenCountry[r.Network]; !ok {
				seenCountry[r.Network] = struct{}{}
				out.Country = append(out.Country, r)
			}
		}
		for _, r := range s.Names {
			if _, ok := seenName[r.ASN]; !ok {
				seenName[r.ASN] = struct{}{}
				out.Names = append(out.Names, r)
			}
		}
	}
	return out
}

// networkString formats IPv4 networks as IPv4-mapped IPv6 CIDRs (::ffff:1.2.3.0/120) and IPv6 networks as usual.
func networkString(p netip.Prefix) string {
	if p.Addr().Is4() {
		return fmt.Sprintf("::ffff:%s/%d", p.Addr(), p.Bits()+96)
	}
	return p.Masked().String()
}

type mmdbKind int

const (
	kindUnknown mmdbKind = iota
	kindASN
	kindCountry
	kindIPinfo // ASN, AS name and country in one database
)

// detectKind maps an mmdb database_type to the data it contains.
func detectKind(databaseType string) mmdbKind {
	t := strings.ToLower(databaseType)
	switch {
	case strings.Contains(t, "ipinfo"):
		return kindIPinfo
	case strings.Contains(t, "asn"), strings.Contains(t, "isp"):
		return kindASN
	case strings.Contains(t, "country"), strings.Contains(t, "city"), strings.Contains(t, "enterprise"):
		return kindCountry
	}
	return kindUnknown
}

var errUnsupported = errors.New("unsupported database_type")

type maxmindASNRecord struct {
	ASN uint32 `maxminddb:"autonomous_system_number"`
	Org string `maxminddb:"autonomous_system_organization"`
}

type maxmindCountryRecord struct {
	Country struct {
		ISOCode string `maxminddb:"iso_code"`
	} `maxminddb:"country"`
	RegisteredCountry struct {
		ISOCode string `maxminddb:"iso_code"`
	} `maxminddb:"registered_country"`
}

type ipinfoRecord struct {
	ASN         string `maxminddb:"asn"` // "AS13335"
	ASName      string `maxminddb:"as_name"`
	CountryCode string `maxminddb:"country_code"`
}

// loadMMDB reads every network of an mmdb file. If kind is kindUnknown, it is detected from the metadata.
func loadMMDB(path string, kind mmdbKind) (*dataset, error) {
	db, err := maxminddb.Open(path)
	if err != nil {
		return nil, err
	}
	defer db.Close()
	if kind == kindUnknown {
		kind = detectKind(db.Metadata.DatabaseType)
		if kind == kindUnknown && strings.Contains(strings.ToLower(filepath.Base(path)), "ipinfo") {
			kind = kindIPinfo
		}
	}

	ds := &dataset{}
	names := map[uint32]string{}
	for res := range db.Networks() {
		network := networkString(res.Prefix())
		switch kind {
		case kindASN:
			var r maxmindASNRecord
			if err := res.Decode(&r); err != nil {
				return nil, fmt.Errorf("%s: %w", path, err)
			}
			if r.ASN != 0 {
				ds.ASN = append(ds.ASN, asnRow{network, r.ASN})
				if _, ok := names[r.ASN]; !ok && r.Org != "" {
					names[r.ASN] = r.Org
					ds.Names = append(ds.Names, nameRow{ASN: r.ASN, Name: r.Org})
				}
			}
		case kindCountry:
			var r maxmindCountryRecord
			if err := res.Decode(&r); err != nil {
				return nil, fmt.Errorf("%s: %w", path, err)
			}
			cc := r.Country.ISOCode
			if cc == "" {
				cc = r.RegisteredCountry.ISOCode
			}
			if cc != "" {
				ds.Country = append(ds.Country, countryRow{network, cc})
			}
		case kindIPinfo:
			var r ipinfoRecord
			if err := res.Decode(&r); err != nil {
				return nil, fmt.Errorf("%s: %w", path, err)
			}
			if r.CountryCode != "" {
				ds.Country = append(ds.Country, countryRow{network, r.CountryCode})
			}
			n, err := strconv.ParseUint(strings.TrimPrefix(strings.ToUpper(r.ASN), "AS"), 10, 32)
			if err == nil && n != 0 {
				asn := uint32(n)
				ds.ASN = append(ds.ASN, asnRow{network, asn})
				if _, ok := names[asn]; !ok && r.ASName != "" {
					names[asn] = r.ASName
					ds.Names = append(ds.Names, nameRow{ASN: asn, Name: r.ASName})
				}
			}
		default:
			return nil, fmt.Errorf("%s: %w %q", path, errUnsupported, db.Metadata.DatabaseType)
		}
	}
	return ds, nil
}

func validateMMDB(path string) error {
	db, err := maxminddb.Open(path)
	if err != nil {
		return fmt.Errorf("not a valid mmdb: %w", err)
	}
	return db.Close()
}

// maxmind downloads GeoLite2/GeoIP2 editions with HTTP basic auth.
type maxmind struct {
	dir       string
	baseURL   string // https://download.maxmind.com/geoip/databases
	accountID string
	key       string
	editions  []string
	maxAge    time.Duration
	client    *http.Client
	now       func() time.Time
}

func (p *maxmind) Name() string { return "maxmind" }

func (p *maxmind) path(edition string) string { return filepath.Join(p.dir, edition+".mmdb") }

func (p *maxmind) Files() []string {
	var paths []string
	for _, e := range p.editions {
		paths = append(paths, p.path(e))
	}
	return existing(paths...)
}

func (p *maxmind) Fetch(ctx context.Context) (bool, error) {
	if err := os.MkdirAll(p.dir, 0o750); err != nil {
		return false, err
	}
	var fetched bool
	var errs []error
	for _, e := range p.editions {
		if !stale(p.path(e), p.maxAge, p.now) {
			continue
		}
		u := fmt.Sprintf("%s/%s/download?suffix=tar.gz", strings.TrimRight(p.baseURL, "/"), url.PathEscape(e))
		req, err := newRequest(ctx, u)
		if err == nil {
			req.SetBasicAuth(p.accountID, p.key)
			err = download(p.client, req, p.path(e), extractMMDB, validateMMDB)
		}
		if err != nil {
			errs = append(errs, fmt.Errorf("%s: %w", e, err))
			continue
		}
		fetched = true
	}
	return fetched, errors.Join(errs...)
}

func (p *maxmind) Load(context.Context) (*dataset, error) {
	var sets []*dataset
	for _, f := range p.Files() {
		ds, err := loadMMDB(f, kindUnknown)
		if errors.Is(err, errUnsupported) {
			slog.Warn("skipping mmdb file", "error", err)
			continue
		}
		if err != nil {
			return nil, err
		}
		sets = append(sets, ds)
	}
	return merge(sets...), nil
}

// ipinfo downloads the IPinfo Lite database, which contains ASN, AS name and country.
type ipinfo struct {
	dir     string
	baseURL string // https://ipinfo.io/data/ipinfo_lite.mmdb
	token   string
	maxAge  time.Duration
	client  *http.Client
	now     func() time.Time
}

func (p *ipinfo) Name() string    { return "ipinfo" }
func (p *ipinfo) path() string    { return filepath.Join(p.dir, "ipinfo_lite.mmdb") }
func (p *ipinfo) Files() []string { return existing(p.path()) }

func (p *ipinfo) Fetch(ctx context.Context) (bool, error) {
	if !stale(p.path(), p.maxAge, p.now) {
		return false, nil
	}
	if err := os.MkdirAll(p.dir, 0o750); err != nil {
		return false, err
	}
	req, err := newRequest(ctx, p.baseURL+"?token="+url.QueryEscape(p.token))
	if err != nil {
		return false, err
	}
	if err := download(p.client, req, p.path(), copyTransform, validateMMDB); err != nil {
		return false, err
	}
	return true, nil
}

func (p *ipinfo) Load(context.Context) (*dataset, error) {
	if len(p.Files()) == 0 {
		return &dataset{}, nil
	}
	return loadMMDB(p.path(), kindIPinfo)
}

// files loads user-supplied *.mmdb files from a directory. It makes no network calls.
type files struct {
	dir string
}

func (p *files) Name() string                        { return "files" }
func (p *files) Fetch(context.Context) (bool, error) { return false, nil }

func (p *files) Files() []string {
	m, _ := filepath.Glob(filepath.Join(p.dir, "*.mmdb"))
	sort.Strings(m)
	return m
}

func (p *files) Load(context.Context) (*dataset, error) {
	var sets []*dataset
	for _, f := range p.Files() {
		ds, err := loadMMDB(f, kindUnknown)
		if errors.Is(err, errUnsupported) {
			slog.Warn("skipping mmdb file", "error", err)
			continue
		}
		if err != nil {
			return nil, err
		}
		sets = append(sets, ds)
	}
	return merge(sets...), nil
}
