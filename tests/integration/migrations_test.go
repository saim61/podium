package integration

import (
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/saim61/podium/internal/store/migrations"
	"github.com/saim61/podium/internal/testsupport"
)

func TestSchemaIsApplied(t *testing.T) {
	pool := testsupport.Postgres(t)

	var exists bool
	err := pool.QueryRow(t.Context(), `
		SELECT EXISTS (
			SELECT 1 FROM information_schema.tables
			WHERE table_schema = 'public' AND table_name = 'users'
		)`).Scan(&exists)

	require.NoError(t, err)
	require.True(t, exists, "users table should exist after migration")
}

func TestSchemaVersionIsRecorded(t *testing.T) {
	dsn := testsupport.PostgresDSN(t)

	version, err := migrations.Version(t.Context(), dsn)

	require.NoError(t, err)
	require.Positive(t, version)
}

func TestApplyIsIdempotent(t *testing.T) {
	dsn := testsupport.PostgresDSN(t)

	before, err := migrations.Version(t.Context(), dsn)
	require.NoError(t, err)

	applied, err := migrations.Apply(t.Context(), dsn)
	require.NoError(t, err)
	require.Empty(t, applied, "a second Apply should run nothing")

	after, err := migrations.Version(t.Context(), dsn)
	require.NoError(t, err)
	require.Equal(t, before, after)
}

func TestApplyRejectsUnreachableDatabase(t *testing.T) {
	_, err := migrations.Apply(t.Context(), "postgres://nobody@127.0.0.1:1/none?sslmode=disable")

	require.Error(t, err)
}
