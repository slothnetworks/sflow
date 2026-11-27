package main

import (
	"bufio"
	"compress/bzip2"
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/osrg/gobgp/v4/pkg/packet/bgp"
	"github.com/osrg/gobgp/v4/pkg/packet/mrt"
)

const (
	defaultRIBBaseURL = "https://archive.routeviews.org/route-views4/bgpdata"
	defaultASNamesURL = "https://ftp.ripe.net/ripe/asnames/asn.txt"
)

// routeviews is a keyless provider: prefix -> origin ASN from the latest RouteViews RIB dump (MRT TABLE_DUMPv2), and
// AS names from RIPE's asn.txt. Parsing a full table takes about a minute, so the parsed result is cached as TSV.
type routeviews struct {
	dir        string
	ribBaseURL string
	asnamesURL string
	maxAge     time.Duration
	client     *http.Client
	now        func() time.Time
}

func (p *routeviews) Name() string { return "routeviews" }

func (p *routeviews) ribPath() string     { return filepath.Join(p.dir, "rib.tsv") }
func (p *routeviews) asnamesPath() string { return filepath.Join(p.dir, "asn.txt") }

func (p *routeviews) Files() []string { return existing(p.ribPath(), p.asnamesPath()) }

func (p *routeviews) Fetch(ctx context.Context) (bool, error) {
	if err := os.MkdirAll(p.dir, 0o750); err != nil {
		return false, err
	}
	var fetched bool
	var errs []error
	if stale(p.ribPath(), p.maxAge, p.now) {
		if err := p.fetchRIB(ctx); err != nil {
			errs = append(errs, fmt.Errorf("RIB: %w", err))
		} else {
			fetched = true
		}
	}
	if stale(p.asnamesPath(), p.maxAge, p.now) {
		req, err := newRequest(ctx, p.asnamesURL)
		if err == nil {
			err = download(p.client, req, p.asnamesPath(), copyTransform, func(path string) error {
				f, err := os.Open(path)
				if err != nil {
					return err
				}
				defer f.Close()
				rows, err := parseASNames(f)
				if err == nil && len(rows) == 0 {
					err = errors.New("no AS names found")
				}
				return err
			})
		}
		if err != nil {
			errs = append(errs, fmt.Errorf("AS names: %w", err))
		} else {
			fetched = true
		}
	}
	return fetched, errors.Join(errs...)
}

// fetchRIB finds the most recent RIB dump, then downloads and parses it into the TSV cache. Dumps are made every two
// hours; we backtrack at most four hours.
func (p *routeviews) fetchRIB(ctx context.Context) error {
	utc := p.now().UTC()
	var errs []error
	for i := 0; i < 5; i++ {
		t := utc.Add(time.Duration(-i) * time.Hour)
		u := fmt.Sprintf("%s/%s/RIBS/%s.bz2", strings.TrimRight(p.ribBaseURL, "/"), t.Format("2006.01"),
			t.Format("rib.20060102.1500"))
		req, err := newRequest(ctx, u)
		if err != nil {
			return err
		}
		err = download(p.client, req, p.ribPath(), func(r io.Reader, w io.Writer) error {
			slog.Info("downloading RIB dump", "url", u)
			n, err := parseMRT(bzip2.NewReader(r), w)
			if err == nil && n == 0 {
				err = errors.New("RIB contained no routes")
			}
			return err
		}, nil)
		if err == nil {
			return nil
		}
		if ctx.Err() != nil {
			return ctx.Err()
		}
		var se *statusError
		if !errors.As(err, &se) {
			return err // network or parse error; don't try older dumps
		}
		errs = append(errs, err)
	}
	return fmt.Errorf("no RIB dump found: %w", errors.Join(errs...))
}

func (p *routeviews) Load(context.Context) (*dataset, error) {
	ds := &dataset{}
	if f, err := os.Open(p.ribPath()); err == nil {
		defer f.Close()
		s := bufio.NewScanner(f)
		for s.Scan() {
			network, asn, ok := strings.Cut(s.Text(), "\t")
			n, err := strconv.ParseUint(asn, 10, 32)
			if !ok || err != nil {
				return nil, fmt.Errorf("corrupt RIB cache line %q", s.Text())
			}
			ds.ASN = append(ds.ASN, asnRow{Network: network, ASN: uint32(n)})
		}
		if err := s.Err(); err != nil {
			return nil, err
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return nil, err
	}
	if f, err := os.Open(p.asnamesPath()); err == nil {
		defer f.Close()
		if ds.Names, err = parseASNames(f); err != nil {
			return nil, err
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return nil, err
	}
	return ds, nil
}

// parseMRT reads a TABLE_DUMPv2 MRT stream and writes "network\tasn" lines for each prefix's origin AS to w.
// IPv4 prefixes are written as IPv4-mapped IPv6 prefixes. It returns the number of lines written.
func parseMRT(r io.Reader, w io.Writer) (int, error) {
	s := bufio.NewScanner(r)
	s.Buffer(make([]byte, 0, 64<<10), 16<<20)
	s.Split(splitMRT)
	var records, written, skipped int

	for s.Scan() {
		d := s.Bytes()
		records++

		hdr, err := mrt.ParseHeader(d)
		if err != nil {
			return written, fmt.Errorf("failed to decode header: %w", err)
		}
		if hdr.Type != mrt.TABLE_DUMPv2 {
			return written, fmt.Errorf("invalid MRT file type, expected TABLE_DUMPv2, got %v", hdr.Type)
		}
		st := mrt.MRTSubTypeTableDumpv2(hdr.SubType)
		if st != mrt.RIB_IPV4_UNICAST && st != mrt.RIB_IPV6_UNICAST {
			continue
		}

		msg, err := mrt.ParseBody(d[mrt.MRT_COMMON_HEADER_LEN:], hdr)
		if err != nil {
			// A single malformed record should not invalidate the whole table.
			skipped++
			slog.Debug("skipping unparseable MRT record", "error", err)
			continue
		}
		body, ok := msg.Body.(*mrt.Rib)
		if !ok || len(body.Entries) == 0 {
			continue
		}

		p, ok := body.Prefix.(*bgp.IPAddrPrefix)
		if !ok || !p.Prefix.IsValid() || p.Prefix.Bits() == 0 {
			continue // default route or unexpected NLRI
		}
		prefix := networkString(p.Prefix)

		// We only want to know the origin, just read the first route entry.
		asn, ok := originAS(body.Entries[0])
		if !ok {
			continue
		}
		if _, err := fmt.Fprintf(w, "%s\t%d\n", prefix, asn); err != nil {
			return written, err
		}
		written++
	}
	if err := s.Err(); err != nil {
		return written, fmt.Errorf("failed to scan input: %w", err)
	}
	slog.Info("parsed RIB", "records", records, "routes", written, "skipped", skipped)
	return written, nil
}

// splitMRT is a bufio.SplitFunc for MRT records. (gobgp's mrt.SplitMrt checks cap(data) instead of len(data), so
// it can misparse a header that straddles a buffer boundary.)
func splitMRT(data []byte, atEOF bool) (int, []byte, error) {
	if len(data) < mrt.MRT_COMMON_HEADER_LEN {
		if atEOF && len(data) > 0 {
			return 0, nil, io.ErrUnexpectedEOF
		}
		return 0, nil, nil
	}
	total := mrt.MRT_COMMON_HEADER_LEN + int(binary.BigEndian.Uint32(data[8:12]))
	if len(data) < total {
		if atEOF {
			return 0, nil, io.ErrUnexpectedEOF
		}
		return 0, nil, nil
	}
	return total, data[:total], nil
}

// originAS returns the last AS of the first AS path segment.
func originAS(e *mrt.RibEntry) (uint32, bool) {
	for _, attr := range e.PathAttributes {
		ap, ok := attr.(*bgp.PathAttributeAsPath)
		if !ok || len(ap.Value) == 0 {
			continue
		}
		if v, ok := ap.Value[0].(*bgp.As4PathParam); ok && len(v.AS) > 0 {
			return v.AS[len(v.AS)-1], true
		}
	}
	return 0, false
}

// asnLine matches lines of RIPE's asn.txt, e.g. "3 MIT-GATEWAYS - Massachusetts Institute of Technology, US".
var asnLine = regexp.MustCompile(`^(\d+) (.*), ([A-Z]{2})$`)

func parseASNames(r io.Reader) ([]nameRow, error) {
	var rows []nameRow
	s := bufio.NewScanner(r)
	for s.Scan() {
		m := asnLine.FindStringSubmatch(strings.TrimRight(s.Text(), "\r"))
		if m == nil {
			continue
		}
		asn, err := strconv.ParseUint(m[1], 10, 32)
		if err != nil {
			continue
		}
		rows = append(rows, nameRow{ASN: uint32(asn), Name: m[2], Country: m[3]})
	}
	return rows, s.Err()
}
