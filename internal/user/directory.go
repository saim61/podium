// Package user resolves accounts for the rest of Podium.
package user

import (
	"context"
	"fmt"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/saim61/podium/internal/store/db"
)

// Directory answers username lookups by id.
type Directory struct {
	queries *db.Queries
}

// NewDirectory builds a directory over a connection pool.
func NewDirectory(pool *pgxpool.Pool) *Directory {
	return &Directory{queries: db.New(pool)}
}

// Usernames returns the username for each id that exists, in one query. Ids with no account are
// simply absent from the result - a leaderboard entry for a deleted user should render as
// missing, not fail the whole page.
func (d *Directory) Usernames(ctx context.Context, ids []int64) (map[int64]string, error) {
	names := make(map[int64]string, len(ids))
	if len(ids) == 0 {
		return names, nil
	}

	rows, err := d.queries.ListUsersByID(ctx, ids)
	if err != nil {
		return nil, fmt.Errorf("list users: %w", err)
	}

	for _, row := range rows {
		names[row.ID] = row.Username
	}
	return names, nil
}
