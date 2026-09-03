package migrations

import (
	"context"
	"database/sql"
	"embed"
	"fmt"

	// Registers the "pgx" driver name that sql.Open below depends on.
	_ "github.com/jackc/pgx/v5/stdlib"
	"github.com/pressly/goose/v3"
)

//go:embed *.sql
var fsys embed.FS

type Applied struct {
	Version int64
	Name    string
}

func Apply(ctx context.Context, databaseURL string) ([]Applied, error) {
	db, err := sql.Open("pgx", databaseURL)
	if err != nil {
		return nil, fmt.Errorf("open database: %w", err)
	}
	defer func() { _ = db.Close() }()

	provider, err := goose.NewProvider(goose.DialectPostgres, db, fsys)
	if err != nil {
		return nil, fmt.Errorf("build migration provider: %w", err)
	}

	results, err := provider.Up(ctx)
	if err != nil {
		return nil, fmt.Errorf("apply migrations: %w", err)
	}

	applied := make([]Applied, 0, len(results))
	for _, r := range results {
		applied = append(applied, Applied{Version: r.Source.Version, Name: r.Source.Path})
	}
	return applied, nil
}

func Version(ctx context.Context, databaseURL string) (int64, error) {
	db, err := sql.Open("pgx", databaseURL)
	if err != nil {
		return 0, fmt.Errorf("open database: %w", err)
	}
	defer func() { _ = db.Close() }()

	provider, err := goose.NewProvider(goose.DialectPostgres, db, fsys)
	if err != nil {
		return 0, fmt.Errorf("build migration provider: %w", err)
	}

	version, err := provider.GetDBVersion(ctx)
	if err != nil {
		return 0, fmt.Errorf("read schema version: %w", err)
	}
	return version, nil
}
