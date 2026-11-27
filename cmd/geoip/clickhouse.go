package main

import (
	"context"
	"fmt"
	"log/slog"
	"time"

	"github.com/ClickHouse/clickhouse-go/v2"
	"github.com/ClickHouse/clickhouse-go/v2/lib/driver"
)

// writer stores a merged dataset.
type writer interface {
	Write(ctx context.Context, ds *dataset) error
}

const insertChunk = 200_000

// chWriter replaces the contents of the geo.* tables via a staging table and EXCHANGE TABLES.
type chWriter struct {
	opts         *clickhouse.Options
	dictionaries []string
	conn         driver.Conn
}

func newCHWriter(addr, user, password string, dictionaries []string) *chWriter {
	return &chWriter{
		opts: &clickhouse.Options{
			Addr:        []string{addr},
			Auth:        clickhouse.Auth{Username: user, Password: password},
			DialTimeout: 10 * time.Second,
			ClientInfo:  clickhouse.ClientInfo{Products: []struct{ Name, Version string }{{"netmeta-geoip", "1.0"}}},
		},
		dictionaries: dictionaries,
	}
}

func (w *chWriter) connect(ctx context.Context) (driver.Conn, error) {
	if w.conn != nil {
		return w.conn, nil
	}
	conn, err := clickhouse.Open(w.opts)
	if err != nil {
		return nil, err
	}
	if err := conn.Ping(ctx); err != nil {
		conn.Close()
		return nil, fmt.Errorf("connecting to ClickHouse at %v: %w", w.opts.Addr, err)
	}
	w.conn = conn
	return conn, nil
}

func (w *chWriter) Write(ctx context.Context, ds *dataset) error {
	conn, err := w.connect(ctx)
	if err != nil {
		return err
	}
	err = w.write(ctx, conn, ds)
	if err != nil {
		// Reconnect on the next attempt.
		conn.Close()
		w.conn = nil
	}
	return err
}

func (w *chWriter) write(ctx context.Context, conn driver.Conn, ds *dataset) error {
	if len(ds.ASN) > 0 {
		if err := swapTable(ctx, conn, "asn", "network, asn", len(ds.ASN), func(b driver.Batch, i int) error {
			return b.Append(ds.ASN[i].Network, ds.ASN[i].ASN)
		}); err != nil {
			return err
		}
	}
	if len(ds.Country) > 0 {
		if err := swapTable(ctx, conn, "country", "network, country", len(ds.Country), func(b driver.Batch, i int) error {
			return b.Append(ds.Country[i].Network, ds.Country[i].Country)
		}); err != nil {
			return err
		}
	}
	if len(ds.Names) > 0 {
		if err := swapTable(ctx, conn, "asn_names", "asn, name, country", len(ds.Names), func(b driver.Batch, i int) error {
			return b.Append(ds.Names[i].ASN, ds.Names[i].Name, ds.Names[i].Country)
		}); err != nil {
			return err
		}
	}
	for _, d := range w.dictionaries {
		// Global (XML) dictionaries are addressed by name, DDL dictionaries by database.name. Try both; a dictionary
		// that doesn't exist yet is not an error.
		if err := conn.Exec(ctx, "SYSTEM RELOAD DICTIONARY "+d); err != nil {
			if err2 := conn.Exec(ctx, "SYSTEM RELOAD DICTIONARY geo."+d); err2 != nil {
				slog.Debug("dictionary reload failed", "dictionary", d, "error", err)
			}
		}
	}
	return nil
}

// swapTable fills geo.<table>_staging with n rows and exchanges it with geo.<table>.
func swapTable(ctx context.Context, conn driver.Conn, table, columns string, n int,
	appendRow func(driver.Batch, int) error) error {
	target := "geo." + table
	staging := target + "_staging"
	start := time.Now()

	for _, q := range []string{
		"DROP TABLE IF EXISTS " + staging,
		"CREATE TABLE " + staging + " AS " + target,
	} {
		if err := conn.Exec(ctx, q); err != nil {
			return fmt.Errorf("%s: %w", q, err)
		}
	}
	for off := 0; off < n; off += insertChunk {
		b, err := conn.PrepareBatch(ctx, "INSERT INTO "+staging+" ("+columns+")")
		if err != nil {
			return fmt.Errorf("insert into %s: %w", staging, err)
		}
		for i := off; i < min(off+insertChunk, n); i++ {
			if err := appendRow(b, i); err != nil {
				b.Close()
				return fmt.Errorf("insert into %s: %w", staging, err)
			}
		}
		if err := b.Send(); err != nil {
			return fmt.Errorf("insert into %s: %w", staging, err)
		}
	}
	for _, q := range []string{
		"EXCHANGE TABLES " + staging + " AND " + target,
		"DROP TABLE " + staging,
	} {
		if err := conn.Exec(ctx, q); err != nil {
			return fmt.Errorf("%s: %w", q, err)
		}
	}
	slog.Info("replaced table", "table", target, "rows", n, "duration", time.Since(start).Round(time.Millisecond))
	return nil
}
