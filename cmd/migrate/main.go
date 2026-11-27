// Command migrate brings NetMeta's ClickHouse schema up to date and exits.
//
// It runs as a one-shot service in compose.yaml, every time the stack starts:
//
//  1. waits for ClickHouse, retrying with backoff
//  2. applies the UDFs in sql/functions (CREATE OR REPLACE, so safe to repeat),
//     then the user-editable isIncomingFlow definition
//  3. applies pending goose migrations from sql/migrations
//  4. sets the flows_raw TTL to RETENTION_DAYS
//
// The UDFs go first because the flows_raw_view migration references ParseGoFlowAddress.
package main

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"os"
	"os/signal"
	"sort"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/ClickHouse/clickhouse-go/v2"
	"github.com/pressly/goose/v3"

	schema "github.com/slothnetworks/sflow/sql"
)

type config struct {
	addr          string
	user          string
	password      string
	database      string
	retentionDays int
	incomingFile  string
	connectWait   time.Duration
}

func env(name, def string) string {
	if v, ok := os.LookupEnv(name); ok && v != "" {
		return v
	}
	return def
}

func loadConfig() (config, error) {
	c := config{
		addr:         env("CLICKHOUSE_ADDR", "clickhouse:9000"),
		user:         env("CLICKHOUSE_USER", "admin"),
		password:     os.Getenv("CLICKHOUSE_PASSWORD"),
		database:     env("CLICKHOUSE_DB", "default"),
		incomingFile: env("IS_INCOMING_FLOW_FILE", "/etc/netmeta/isIncomingFlow.sql"),
	}
	days, err := strconv.Atoi(env("RETENTION_DAYS", "7"))
	if err != nil || days < 1 {
		return c, fmt.Errorf("RETENTION_DAYS must be a positive integer, got %q", os.Getenv("RETENTION_DAYS"))
	}
	c.retentionDays = days
	c.connectWait, err = time.ParseDuration(env("CONNECT_TIMEOUT", "3m"))
	if err != nil {
		return c, fmt.Errorf("CONNECT_TIMEOUT: %w", err)
	}
	if c.password == "" {
		return c, errors.New("CLICKHOUSE_PASSWORD is not set")
	}
	return c, nil
}

func connect(ctx context.Context, c config) (*sql.DB, error) {
	db := clickhouse.OpenDB(&clickhouse.Options{
		Addr: []string{c.addr},
		Auth: clickhouse.Auth{Database: c.database, Username: c.user, Password: c.password},
		Settings: clickhouse.Settings{
			// Never rewrite existing parts when the TTL changes (flows_raw can be tens of GiB).
			"materialize_ttl_after_modify": 0,
		},
		DialTimeout: 5 * time.Second,
	})

	ctx, cancel := context.WithTimeout(ctx, c.connectWait)
	defer cancel()
	backoff := time.Second
	for {
		err := db.PingContext(ctx)
		if err == nil {
			return db, nil
		}
		slog.Info("waiting for clickhouse", "addr", c.addr, "err", err)
		select {
		case <-ctx.Done():
			db.Close()
			return nil, fmt.Errorf("clickhouse not reachable at %s: %w", c.addr, err)
		case <-time.After(backoff):
		}
		backoff = min(backoff*2, 15*time.Second)
	}
}

// applyFunctions runs every sql/functions/*.sql file in lexical order, so the numeric
// prefix decides creation order for UDFs that call other UDFs.
func applyFunctions(ctx context.Context, db *sql.DB) error {
	names, err := fs.Glob(schema.Functions, "functions/*.sql")
	if err != nil {
		return err
	}
	sort.Strings(names)
	for _, name := range names {
		body, err := fs.ReadFile(schema.Functions, name)
		if err != nil {
			return err
		}
		if err := execStatement(ctx, db, name, string(body)); err != nil {
			return err
		}
	}
	return nil
}

func execStatement(ctx context.Context, db *sql.DB, name, body string) error {
	stmt := strings.TrimSpace(body)
	stmt = strings.TrimSuffix(stmt, ";")
	if _, err := db.ExecContext(ctx, stmt); err != nil {
		return fmt.Errorf("%s: %w", name, err)
	}
	slog.Info("applied", "file", name)
	return nil
}

func applyIsIncomingFlow(ctx context.Context, db *sql.DB, path string) error {
	body, err := os.ReadFile(path)
	if errors.Is(err, fs.ErrNotExist) {
		slog.Warn("isIncomingFlow file not found, using the built-in default", "path", path)
		body = []byte("CREATE OR REPLACE FUNCTION isIncomingFlow AS " +
			"(SamplerAddress, SrcAddr, DstAddr, SrcAS, DstAS, FlowDirection) -> coalesce(FlowDirection = 0)")
	} else if err != nil {
		return err
	}
	return execStatement(ctx, db, path, stripComments(string(body)))
}

// stripComments drops full-line "--" comments, so a commented-out example can't be
// mistaken for part of the statement.
func stripComments(s string) string {
	var out []string
	for _, line := range strings.Split(s, "\n") {
		if strings.HasPrefix(strings.TrimSpace(line), "--") {
			continue
		}
		out = append(out, line)
	}
	return strings.Join(out, "\n")
}

func migrate(ctx context.Context, db *sql.DB) error {
	fsys, err := fs.Sub(schema.Migrations, "migrations")
	if err != nil {
		return err
	}
	p, err := goose.NewProvider(goose.DialectClickHouse, db, fsys)
	if err != nil {
		return err
	}
	results, err := p.Up(ctx)
	for _, r := range results {
		slog.Info("migration", "version", r.Source.Version, "file", r.Source.Path, "duration", r.Duration)
	}
	if err != nil {
		return fmt.Errorf("goose up: %w", err)
	}
	if len(results) == 0 {
		slog.Info("schema already up to date")
	}
	return nil
}

func syncTTL(ctx context.Context, db *sql.DB, days int) error {
	var engine string
	err := db.QueryRowContext(ctx,
		"SELECT engine_full FROM system.tables WHERE database = currentDatabase() AND name = 'flows_raw'").Scan(&engine)
	if err != nil {
		return fmt.Errorf("reading flows_raw TTL: %w", err)
	}
	want := fmt.Sprintf("TTL Date + toIntervalDay(%d)", days)
	if strings.Contains(engine, want) {
		return nil
	}
	if _, err := db.ExecContext(ctx, fmt.Sprintf("ALTER TABLE flows_raw MODIFY TTL Date + INTERVAL %d DAY", days)); err != nil {
		return fmt.Errorf("setting TTL: %w", err)
	}
	slog.Info("flows_raw TTL updated", "days", days)
	return nil
}

func run(ctx context.Context) error {
	c, err := loadConfig()
	if err != nil {
		return err
	}
	db, err := connect(ctx, c)
	if err != nil {
		return err
	}
	defer db.Close()

	if err := applyFunctions(ctx, db); err != nil {
		return err
	}
	if err := applyIsIncomingFlow(ctx, db, c.incomingFile); err != nil {
		return err
	}
	if err := migrate(ctx, db); err != nil {
		return err
	}
	return syncTTL(ctx, db, c.retentionDays)
}

func main() {
	slog.SetDefault(slog.New(slog.NewTextHandler(os.Stderr, nil)))
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	if err := run(ctx); err != nil {
		slog.Error("migrate failed", "err", err)
		os.Exit(1)
	}
	slog.Info("done")
}
