package main

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"errors"
	"net"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"sync/atomic"
	"testing"
	"testing/iotest"
	"time"

	"github.com/maxmind/mmdbwriter"
	"github.com/maxmind/mmdbwriter/mmdbtype"
	"github.com/osrg/gobgp/v4/pkg/packet/bgp"
	"github.com/osrg/gobgp/v4/pkg/packet/mrt"
)

// --- fixtures ---

func mrtRecord(t *testing.T, sub mrt.MRTSubTypeTableDumpv2, body mrt.Body) []byte {
	t.Helper()
	m, err := mrt.NewMRTMessage(time.Unix(1700000000, 0), mrt.TABLE_DUMPv2, sub, body)
	if err != nil {
		t.Fatal(err)
	}
	b, err := m.Serialize()
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func ribEntry(path ...[]uint32) *mrt.RibEntry {
	var segs []bgp.AsPathParamInterface
	for _, p := range path {
		segs = append(segs, bgp.NewAs4PathParam(bgp.BGP_ASPATH_ATTR_TYPE_SEQ, p))
	}
	attrs := []bgp.PathAttributeInterface{bgp.NewPathAttributeOrigin(0), bgp.NewPathAttributeAsPath(segs)}
	return mrt.NewRibEntry(0, 1700000000, 0, attrs, false)
}

func pfx(t *testing.T, s string) *bgp.IPAddrPrefix {
	t.Helper()
	p, err := bgp.NewIPAddrPrefix(netip.MustParsePrefix(s))
	if err != nil {
		t.Fatal(err)
	}
	return p
}

// testMRT builds a small TABLE_DUMPv2 file.
func testMRT(t *testing.T) []byte {
	t.Helper()
	var buf bytes.Buffer
	peers := []*mrt.Peer{mrt.NewPeer(netip.MustParseAddr("192.0.2.1"), netip.MustParseAddr("192.0.2.1"), 64500, true)}
	buf.Write(mrtRecord(t, mrt.PEER_INDEX_TABLE, mrt.NewPeerIndexTable(netip.MustParseAddr("192.0.2.1"), "test", peers)))
	ribs := []struct {
		sub    mrt.MRTSubTypeTableDumpv2
		prefix *bgp.IPAddrPrefix
		entry  *mrt.RibEntry
	}{
		{mrt.RIB_IPV4_UNICAST, pfx(t, "1.1.1.0/24"), ribEntry([]uint32{64500, 174, 13335})},
		{mrt.RIB_IPV4_UNICAST, pfx(t, "0.0.0.0/0"), ribEntry([]uint32{64500})}, // default route
		{mrt.RIB_IPV4_UNICAST, pfx(t, "10.1.0.0/16"), ribEntry()},              // empty AS path
		{mrt.RIB_IPV4_UNICAST, pfx(t, "11.0.0.0/8"), ribEntry([]uint32{})},     // empty segment
		{mrt.RIB_IPV6_UNICAST, pfx(t, "2001:db8::/32"), ribEntry([]uint32{64500, 4200000000})},
	}
	for i, r := range ribs {
		family := bgp.RF_IPv4_UC
		if r.sub == mrt.RIB_IPV6_UNICAST {
			family = bgp.RF_IPv6_UC
		}
		buf.Write(mrtRecord(t, r.sub, mrt.NewRib(uint32(i), family, r.prefix, []*mrt.RibEntry{r.entry})))
	}
	return buf.Bytes()
}

func bzip2Bytes(t *testing.T, b []byte) []byte {
	t.Helper()
	if _, err := exec.LookPath("bzip2"); err != nil {
		t.Skip("bzip2 not installed")
	}
	cmd := exec.Command("bzip2", "-c")
	cmd.Stdin = bytes.NewReader(b)
	out, err := cmd.Output()
	if err != nil {
		t.Fatal(err)
	}
	return out
}

func writeMMDB(t *testing.T, path, dbType string, records map[string]mmdbtype.Map) {
	t.Helper()
	tree, err := mmdbwriter.New(mmdbwriter.Options{DatabaseType: dbType, IncludeReservedNetworks: true})
	if err != nil {
		t.Fatal(err)
	}
	for cidr, rec := range records {
		_, n, err := net.ParseCIDR(cidr)
		if err != nil {
			t.Fatal(err)
		}
		if err := tree.Insert(n, rec); err != nil {
			t.Fatal(err)
		}
	}
	f, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	if _, err := tree.WriteTo(f); err != nil {
		t.Fatal(err)
	}
}

func asnDB(t *testing.T, path string) {
	writeMMDB(t, path, "GeoLite2-ASN", map[string]mmdbtype.Map{
		"1.1.1.0/24": {"autonomous_system_number": mmdbtype.Uint32(13335),
			"autonomous_system_organization": mmdbtype.String("CLOUDFLARENET")},
		"2001:db8::/32": {"autonomous_system_number": mmdbtype.Uint32(64496),
			"autonomous_system_organization": mmdbtype.String("EXAMPLE")},
	})
}

func countryDB(t *testing.T, path string) {
	writeMMDB(t, path, "GeoLite2-Country", map[string]mmdbtype.Map{
		"1.1.1.0/24": {"country": mmdbtype.Map{"iso_code": mmdbtype.String("AU")}},
		"8.8.8.0/24": {"registered_country": mmdbtype.Map{"iso_code": mmdbtype.String("US")}},
	})
}

func ipinfoDB(t *testing.T, path string) {
	writeMMDB(t, path, "ipinfo lite.mmdb", map[string]mmdbtype.Map{
		"1.1.1.0/24": {"asn": mmdbtype.String("AS13335"), "as_name": mmdbtype.String("Cloudflare, Inc."),
			"country_code": mmdbtype.String("US"), "country": mmdbtype.String("United States")},
		"9.9.9.0/24": {"asn": mmdbtype.String("AS19281"), "as_name": mmdbtype.String("Quad9"),
			"country_code": mmdbtype.String("CH"), "country": mmdbtype.String("Switzerland")},
	})
}

func tarGz(t *testing.T, name string, content []byte) []byte {
	t.Helper()
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	_ = tw.WriteHeader(&tar.Header{Name: "GeoLite2-ASN_20261127/", Typeflag: tar.TypeDir, Mode: 0o755})
	_ = tw.WriteHeader(&tar.Header{Name: "GeoLite2-ASN_20261127/COPYRIGHT.txt", Mode: 0o644, Size: 2})
	_, _ = tw.Write([]byte("c\n"))
	if err := tw.WriteHeader(&tar.Header{Name: "GeoLite2-ASN_20261127/" + name, Mode: 0o644,
		Size: int64(len(content))}); err != nil {
		t.Fatal(err)
	}
	_, _ = tw.Write(content)
	tw.Close()
	gz.Close()
	return buf.Bytes()
}

func sortedASN(ds *dataset) map[string]uint32 {
	m := map[string]uint32{}
	for _, r := range ds.ASN {
		m[r.Network] = r.ASN
	}
	return m
}

func sortedCountry(ds *dataset) map[string]string {
	m := map[string]string{}
	for _, r := range ds.Country {
		m[r.Network] = r.Country
	}
	return m
}

// --- parsing ---

func TestParseMRT(t *testing.T) {
	var out bytes.Buffer
	n, err := parseMRT(bytes.NewReader(testMRT(t)), &out)
	if err != nil {
		t.Fatal(err)
	}
	want := "::ffff:1.1.1.0/120\t13335\n2001:db8::/32\t4200000000\n"
	if out.String() != want || n != 2 {
		t.Errorf("got %d lines:\n%s\nwant:\n%s", n, out.String(), want)
	}
}

// Records straddling read boundaries must parse (regression: gobgp's SplitMrt misparses them), and truncated input
// must fail.
func TestParseMRTSmallReads(t *testing.T) {
	data := testMRT(t)
	var out bytes.Buffer
	if n, err := parseMRT(iotest.OneByteReader(bytes.NewReader(data)), &out); err != nil || n != 2 {
		t.Errorf("one-byte reads: n=%d err=%v", n, err)
	}
	if _, err := parseMRT(bytes.NewReader(data[:len(data)-3]), &bytes.Buffer{}); err == nil {
		t.Error("expected error for truncated input")
	}
}

func TestParseMRTRejectsWrongType(t *testing.T) {
	sc, err := mrt.NewBGP4MPStateChange(1, 2, 0, netip.MustParseAddr("192.0.2.1"), netip.MustParseAddr("192.0.2.2"),
		true, mrt.IDLE, mrt.ACTIVE)
	if err != nil {
		t.Fatal(err)
	}
	m, _ := mrt.NewMRTMessage(time.Unix(0, 0), mrt.BGP4MP, mrt.STATE_CHANGE, sc)
	b, _ := m.Serialize()
	if _, err := parseMRT(bytes.NewReader(b), &bytes.Buffer{}); err == nil {
		t.Error("expected error for non-TABLE_DUMPv2 input")
	}
}

func TestParseASNames(t *testing.T) {
	in := "1 LVLT-1 - Level 3 Parent, LLC, US\n3 MIT-GATEWAYS - Massachusetts Institute of Technology, US\r\ngarbage\n"
	rows, err := parseASNames(strings.NewReader(in))
	if err != nil {
		t.Fatal(err)
	}
	want := []nameRow{{1, "LVLT-1 - Level 3 Parent, LLC", "US"},
		{3, "MIT-GATEWAYS - Massachusetts Institute of Technology", "US"}}
	if !reflect.DeepEqual(rows, want) {
		t.Errorf("got %+v, want %+v", rows, want)
	}
}

func TestDetectKind(t *testing.T) {
	for typ, want := range map[string]mmdbKind{
		"GeoLite2-ASN": kindASN, "GeoIP2-ISP": kindASN, "GeoLite2-Country": kindCountry,
		"GeoLite2-City": kindCountry, "GeoIP2-Enterprise": kindCountry, "ipinfo lite.mmdb": kindIPinfo,
		"GeoIP2-Anonymous-IP": kindUnknown,
	} {
		if got := detectKind(typ); got != want {
			t.Errorf("detectKind(%q) = %v, want %v", typ, got, want)
		}
	}
}

func TestLoadMMDB(t *testing.T) {
	dir := t.TempDir()
	asnDB(t, filepath.Join(dir, "asn.mmdb"))
	countryDB(t, filepath.Join(dir, "country.mmdb"))
	ipinfoDB(t, filepath.Join(dir, "ipinfo.mmdb"))

	ds, err := loadMMDB(filepath.Join(dir, "asn.mmdb"), kindUnknown)
	if err != nil {
		t.Fatal(err)
	}
	if got, want := sortedASN(ds), map[string]uint32{"::ffff:1.1.1.0/120": 13335, "2001:db8::/32": 64496}; !reflect.DeepEqual(got, want) {
		t.Errorf("asn: got %v, want %v", got, want)
	}
	if len(ds.Names) != 2 || len(ds.Country) != 0 {
		t.Errorf("asn: unexpected names/country: %+v", ds)
	}

	ds, err = loadMMDB(filepath.Join(dir, "country.mmdb"), kindUnknown)
	if err != nil {
		t.Fatal(err)
	}
	if got, want := sortedCountry(ds), map[string]string{"::ffff:1.1.1.0/120": "AU", "::ffff:8.8.8.0/120": "US"}; !reflect.DeepEqual(got, want) {
		t.Errorf("country: got %v, want %v", got, want)
	}

	ds, err = loadMMDB(filepath.Join(dir, "ipinfo.mmdb"), kindUnknown)
	if err != nil {
		t.Fatal(err)
	}
	if sortedASN(ds)["::ffff:9.9.9.0/120"] != 19281 || sortedCountry(ds)["::ffff:9.9.9.0/120"] != "CH" || len(ds.Names) != 2 {
		t.Errorf("ipinfo: got %+v", ds)
	}
}

func TestFilesProvider(t *testing.T) {
	dir := t.TempDir()
	asnDB(t, filepath.Join(dir, "a.mmdb"))
	countryDB(t, filepath.Join(dir, "b.mmdb"))
	writeMMDB(t, filepath.Join(dir, "c.mmdb"), "GeoIP2-Anonymous-IP", map[string]mmdbtype.Map{
		"1.1.1.0/24": {"is_anonymous": mmdbtype.Bool(true)}})
	_ = os.WriteFile(filepath.Join(dir, "README"), []byte("x"), 0o644)

	ds, err := (&files{dir: dir}).Load(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(ds.ASN) != 2 || len(ds.Country) != 2 || len(ds.Names) != 2 {
		t.Errorf("got %+v", ds)
	}
}

func TestMergePriority(t *testing.T) {
	a := &dataset{ASN: []asnRow{{"n1", 1}}, Names: []nameRow{{1, "first", ""}}}
	b := &dataset{ASN: []asnRow{{"n1", 2}, {"n2", 3}}, Country: []countryRow{{"n1", "DE"}},
		Names: []nameRow{{1, "second", "DE"}, {3, "three", "US"}}}
	got := merge(a, b)
	want := &dataset{ASN: []asnRow{{"n1", 1}, {"n2", 3}}, Country: []countryRow{{"n1", "DE"}},
		Names: []nameRow{{1, "first", ""}, {3, "three", "US"}}}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("got %+v, want %+v", got, want)
	}
}

// --- configuration ---

func TestLoadConfig(t *testing.T) {
	getenv := func(m map[string]string) func(string) string { return func(k string) string { return m[k] } }

	c, err := loadConfig(getenv(nil))
	if err != nil || len(c.Providers) != 0 || c.Refresh != 24*time.Hour || c.ClickHouseAddr != "clickhouse:9000" ||
		c.ClickHouseUser != "admin" || !reflect.DeepEqual(c.MaxMindEditions, []string{"GeoLite2-ASN", "GeoLite2-Country"}) {
		t.Fatalf("defaults: %+v, %v", c, err)
	}

	c, err = loadConfig(getenv(map[string]string{"GEOIP_PROVIDERS": " ipinfo, routeviews,files ",
		"IPINFO_TOKEN": "t"}))
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, p := range buildProviders(c, nil) {
		names = append(names, p.Name())
	}
	if !reflect.DeepEqual(names, []string{"ipinfo", "routeviews", "files"}) {
		t.Errorf("providers in wrong order: %v", names)
	}

	for _, bad := range []map[string]string{
		{"GEOIP_PROVIDERS": "bogus"},
		{"GEOIP_PROVIDERS": "maxmind"},
		{"GEOIP_PROVIDERS": "ipinfo"},
		{"GEOIP_PROVIDERS": "files,files"},
		{"GEOIP_REFRESH": "soon"},
	} {
		if _, err := loadConfig(getenv(bad)); err == nil {
			t.Errorf("expected error for %v", bad)
		}
	}
}

// --- service ---

type fakeWriter struct {
	calls atomic.Int32
	err   error
	last  *dataset
}

func (w *fakeWriter) Write(_ context.Context, ds *dataset) error {
	w.calls.Add(1)
	w.last = ds
	return w.err
}

// failTransport fails the test on any HTTP request.
type failTransport struct{ t *testing.T }

func (f failTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	f.t.Errorf("unexpected network call to %s", r.URL)
	return nil, errors.New("no network")
}

func TestNoProvidersDoesNothing(t *testing.T) {
	c, err := loadConfig(func(string) string { return "" })
	if err != nil {
		t.Fatal(err)
	}
	w := &fakeWriter{}
	s := newService(buildProviders(c, &http.Client{Transport: failTransport{t}}), w, c.Refresh)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { s.run(ctx); close(done) }()
	select {
	case <-done: // run returns immediately without providers
	case <-time.After(5 * time.Second):
		t.Fatal("run did not return")
	}
	cancel()
	if err := s.cycle(context.Background()); err != nil || w.calls.Load() != 0 {
		t.Errorf("cycle: err=%v writes=%d", err, w.calls.Load())
	}

	rec := httptest.NewRecorder()
	s.routes().ServeHTTP(rec, httptest.NewRequest("GET", "/healthz", nil))
	if rec.Code != 200 {
		t.Errorf("healthz: %d", rec.Code)
	}
	rec = httptest.NewRecorder()
	s.routes().ServeHTTP(rec, httptest.NewRequest("GET", "/status", nil))
	if rec.Code != 200 || !strings.Contains(rec.Body.String(), `"providers": []`) {
		t.Errorf("status: %d %s", rec.Code, rec.Body)
	}
}

func TestServiceCycle(t *testing.T) {
	dir := t.TempDir()
	asnDB(t, filepath.Join(dir, "a.mmdb"))
	files1 := &files{dir: dir}

	// The routeviews provider has a fresh cache, so it makes no network calls.
	rvDir := t.TempDir()
	_ = os.WriteFile(filepath.Join(rvDir, "rib.tsv"), []byte("::ffff:1.1.1.0/120\t1\n::ffff:2.2.2.0/120\t2\n"), 0o644)
	_ = os.WriteFile(filepath.Join(rvDir, "asn.txt"), []byte("13335 CLOUDFLARENET - Cloudflare, Inc., US\n"), 0o644)
	rv := &routeviews{dir: rvDir, maxAge: time.Hour, now: time.Now, client: &http.Client{Transport: failTransport{t}}}

	w := &fakeWriter{}
	s := newService([]provider{files1, rv}, w, time.Hour)
	if err := s.cycle(context.Background()); err != nil {
		t.Fatal(err)
	}
	if w.calls.Load() != 1 {
		t.Fatalf("writes = %d", w.calls.Load())
	}
	// files wins for 1.1.1.0/24, routeviews fills in 2.2.2.0/24; names from the mmdb win over asn.txt.
	want := map[string]uint32{"::ffff:1.1.1.0/120": 13335, "::ffff:2.2.2.0/120": 2, "2001:db8::/32": 64496}
	if got := sortedASN(w.last); !reflect.DeepEqual(got, want) {
		t.Errorf("got %v, want %v", got, want)
	}
	if w.last.Names[0] != (nameRow{13335, "CLOUDFLARENET", ""}) {
		t.Errorf("names: %+v", w.last.Names)
	}

	// Unchanged files: no second write.
	if err := s.cycle(context.Background()); err != nil || w.calls.Load() != 1 {
		t.Errorf("unchanged: err=%v writes=%d", err, w.calls.Load())
	}

	// Writer failure is reported and retried on the next cycle.
	countryDB(t, filepath.Join(dir, "b.mmdb"))
	w.err = errors.New("clickhouse down")
	if err := s.cycle(context.Background()); err == nil {
		t.Error("expected write error")
	}
	w.err = nil
	if err := s.cycle(context.Background()); err != nil || w.calls.Load() != 3 || len(w.last.Country) != 2 {
		t.Errorf("retry: err=%v writes=%d", err, w.calls.Load())
	}
}

// --- download clients ---

func TestMaxMindFetch(t *testing.T) {
	dbDir := t.TempDir()
	asnDB(t, filepath.Join(dbDir, "x.mmdb"))
	mmdb, _ := os.ReadFile(filepath.Join(dbDir, "x.mmdb"))
	archive := tarGz(t, "GeoLite2-ASN.mmdb", mmdb)

	var hits atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		user, pass, ok := r.BasicAuth()
		if !ok || user != "12345" || pass != "secret" {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		if r.URL.Path != "/GeoLite2-ASN/download" || r.URL.Query().Get("suffix") != "tar.gz" {
			http.NotFound(w, r)
			return
		}
		_, _ = w.Write(archive)
	}))
	defer srv.Close()

	dir := t.TempDir()
	p := &maxmind{dir: dir, baseURL: srv.URL, accountID: "12345", key: "wrong", editions: []string{"GeoLite2-ASN"},
		maxAge: time.Hour, client: srv.Client(), now: time.Now}
	if _, err := p.Fetch(context.Background()); err == nil || !strings.Contains(err.Error(), "401") {
		t.Errorf("bad key: err=%v", err)
	}
	if len(p.Files()) != 0 {
		t.Error("file written despite failure")
	}

	p.key = "secret"
	fetched, err := p.Fetch(context.Background())
	if err != nil || !fetched {
		t.Fatalf("fetch: %v %v", fetched, err)
	}
	ds, err := p.Load(context.Background())
	if err != nil || len(ds.ASN) != 2 {
		t.Fatalf("load: %+v %v", ds, err)
	}

	// Fresh cache: no request.
	before := hits.Load()
	if fetched, err := p.Fetch(context.Background()); fetched || err != nil || hits.Load() != before {
		t.Errorf("fresh cache refetched: %v %v", fetched, err)
	}
}

func TestIPinfoFetch(t *testing.T) {
	dbDir := t.TempDir()
	ipinfoDB(t, filepath.Join(dbDir, "x.mmdb"))
	mmdb, _ := os.ReadFile(filepath.Join(dbDir, "x.mmdb"))

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("token") != "tok" {
			http.Error(w, "forbidden", http.StatusForbidden)
			return
		}
		_, _ = w.Write(mmdb)
	}))
	defer srv.Close()

	p := &ipinfo{dir: t.TempDir(), baseURL: srv.URL + "/data/ipinfo_lite.mmdb", token: "bad", maxAge: time.Hour,
		client: srv.Client(), now: time.Now}
	_, err := p.Fetch(context.Background())
	if err == nil || strings.Contains(err.Error(), "bad") {
		t.Errorf("bad token: expected redacted error, got %v", err)
	}

	p.token = "tok"
	if _, err := p.Fetch(context.Background()); err != nil {
		t.Fatal(err)
	}
	ds, err := p.Load(context.Background())
	if err != nil || len(ds.ASN) != 2 || len(ds.Country) != 2 {
		t.Fatalf("load: %+v %v", ds, err)
	}
}

func TestIPinfoRejectsGarbage(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte("<html>not a database</html>"))
	}))
	defer srv.Close()
	p := &ipinfo{dir: t.TempDir(), baseURL: srv.URL, token: "tok", maxAge: time.Hour, client: srv.Client(),
		now: time.Now}
	if _, err := p.Fetch(context.Background()); err == nil || len(p.Files()) != 0 {
		t.Errorf("garbage accepted: %v", err)
	}
}

func TestRouteViewsFetch(t *testing.T) {
	rib := bzip2Bytes(t, testMRT(t))
	now := time.Date(2026, 11, 27, 17, 30, 0, 0, time.UTC)
	var failRIB atomic.Bool
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/bgpdata/2026.11/RIBS/rib.20261127.1600.bz2":
			if failRIB.Load() {
				http.Error(w, "oops", http.StatusInternalServerError)
				return
			}
			_, _ = w.Write(rib)
		case "/asn.txt":
			_, _ = w.Write([]byte("13335 CLOUDFLARENET - Cloudflare, Inc., US\n"))
		default:
			http.NotFound(w, r) // 17:00 has no dump yet
		}
	}))
	defer srv.Close()

	clock := now
	p := &routeviews{dir: t.TempDir(), ribBaseURL: srv.URL + "/bgpdata", asnamesURL: srv.URL + "/asn.txt",
		maxAge: time.Hour, client: srv.Client(), now: func() time.Time { return clock }}
	if _, err := p.Fetch(context.Background()); err != nil {
		t.Fatal(err)
	}
	ds, err := p.Load(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(ds.ASN) != 2 || ds.ASN[0] != (asnRow{"::ffff:1.1.1.0/120", 13335}) || len(ds.Names) != 1 {
		t.Errorf("got %+v", ds)
	}

	// Upstream failure with stale data: error, but the old data is still served.
	clock = now.Add(2 * time.Hour)
	failRIB.Store(true)
	if _, err := p.Fetch(context.Background()); err == nil {
		t.Error("expected error")
	}
	if ds, err := p.Load(context.Background()); err != nil || len(ds.ASN) != 2 {
		t.Errorf("stale data lost: %+v %v", ds, err)
	}
}

func TestRouteViewsNoDump(t *testing.T) {
	srv := httptest.NewServer(http.NotFoundHandler())
	defer srv.Close()
	p := &routeviews{dir: t.TempDir(), ribBaseURL: srv.URL, asnamesURL: srv.URL + "/asn.txt", maxAge: time.Hour,
		client: srv.Client(), now: time.Now}
	_, err := p.Fetch(context.Background())
	if err == nil || !strings.Contains(err.Error(), "no RIB dump found") {
		t.Errorf("got %v", err)
	}
	if ds, err := p.Load(context.Background()); err != nil || !ds.empty() {
		t.Errorf("cold start load: %+v %v", ds, err)
	}
}

func TestRedactURL(t *testing.T) {
	got := redactURL("https://u:p@ipinfo.io/data/ipinfo_lite.mmdb?token=abc&x=1")
	if strings.Contains(got, "abc") || strings.Contains(got, "u:p") {
		t.Errorf("not redacted: %s", got)
	}
}

// --- ClickHouse (needs a server) ---

func TestCHWriter(t *testing.T) {
	addr := os.Getenv("CLICKHOUSE_TEST_ADDR")
	if addr == "" {
		t.Skip("CLICKHOUSE_TEST_ADDR not set")
	}
	w := newCHWriter(addr, env(os.Getenv, "CLICKHOUSE_TEST_USER", "default"), os.Getenv("CLICKHOUSE_TEST_PASSWORD"),
		[]string{"geo_asn"})
	ctx := context.Background()
	conn, err := w.connect(ctx)
	if err != nil {
		t.Fatal(err)
	}
	for _, q := range []string{
		"CREATE DATABASE IF NOT EXISTS geo",
		"CREATE TABLE IF NOT EXISTS geo.asn (network String, asn UInt32) ENGINE = MergeTree ORDER BY network",
		"CREATE TABLE IF NOT EXISTS geo.country (network String, country LowCardinality(String)) ENGINE = MergeTree ORDER BY network",
		"CREATE TABLE IF NOT EXISTS geo.asn_names (asn UInt32, name String, country String) ENGINE = MergeTree ORDER BY asn",
	} {
		if err := conn.Exec(ctx, q); err != nil {
			t.Fatal(err)
		}
	}
	ds := &dataset{ASN: []asnRow{{"::ffff:1.1.1.0/120", 13335}}, Country: []countryRow{{"::ffff:1.1.1.0/120", "AU"}},
		Names: []nameRow{{13335, "CLOUDFLARENET", "US"}}}
	for range 2 {
		if err := w.Write(ctx, ds); err != nil {
			t.Fatal(err)
		}
	}
	var n uint64
	if err := w.conn.QueryRow(ctx, "SELECT count() FROM geo.asn").Scan(&n); err != nil || n != 1 {
		t.Errorf("count = %d, %v", n, err)
	}
}
