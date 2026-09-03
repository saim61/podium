package integration

import (
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/stretchr/testify/require"

	"github.com/saim61/podium/internal/store/db"
	"github.com/saim61/podium/internal/testsupport"
)

func newUser(username, email string) db.CreateUserParams {
	return db.CreateUserParams{Username: username, Email: email, PasswordHash: "hash"}
}

func TestCreateUser(t *testing.T) {
	q, _ := testsupport.Queries(t)

	user, err := q.CreateUser(t.Context(), newUser("saeem", "saeem@example.com"))

	require.NoError(t, err)
	require.Equal(t, int64(1), user.ID)
	require.Equal(t, "saeem", user.Username)
	require.Equal(t, "hash", user.PasswordHash)
	require.False(t, user.CreatedAt.IsZero())
	require.False(t, user.UpdatedAt.IsZero())
}

func TestGetUserByID(t *testing.T) {
	q, _ := testsupport.Queries(t)

	created, err := q.CreateUser(t.Context(), newUser("saeem", "saeem@example.com"))
	require.NoError(t, err)

	found, err := q.GetUserByID(t.Context(), created.ID)

	require.NoError(t, err)
	require.Equal(t, created.ID, found.ID)
	require.Equal(t, created.Username, found.Username)
}

func TestGetMissingUserReportsNoRows(t *testing.T) {
	q, _ := testsupport.Queries(t)

	_, err := q.GetUserByID(t.Context(), 999)

	require.ErrorIs(t, err, pgx.ErrNoRows)
}

func TestLookupIsCaseInsensitive(t *testing.T) {
	q, _ := testsupport.Queries(t)

	created, err := q.CreateUser(t.Context(), newUser("Saeem", "Saeem@Example.com"))
	require.NoError(t, err)

	byName, err := q.GetUserByUsername(t.Context(), "sAeEm")
	require.NoError(t, err)
	require.Equal(t, created.ID, byName.ID)

	byEmail, err := q.GetUserByEmail(t.Context(), "saeem@EXAMPLE.com")
	require.NoError(t, err)
	require.Equal(t, created.ID, byEmail.ID)
}

func TestUsernameUniquenessIgnoresCase(t *testing.T) {
	q, _ := testsupport.Queries(t)

	_, err := q.CreateUser(t.Context(), newUser("saeem", "one@example.com"))
	require.NoError(t, err)

	_, err = q.CreateUser(t.Context(), newUser("SAEEM", "two@example.com"))

	require.Error(t, err, "usernames differing only in case must collide")
	require.Contains(t, err.Error(), "users_username_key")
}

func TestEmailUniquenessIgnoresCase(t *testing.T) {
	q, _ := testsupport.Queries(t)

	_, err := q.CreateUser(t.Context(), newUser("one", "saeem@example.com"))
	require.NoError(t, err)

	_, err = q.CreateUser(t.Context(), newUser("two", "SAEEM@EXAMPLE.COM"))

	require.Error(t, err)
	require.Contains(t, err.Error(), "users_email_key")
}

func TestListUsersByIDBatchesLookups(t *testing.T) {
	q, _ := testsupport.Queries(t)

	first, err := q.CreateUser(t.Context(), newUser("one", "one@example.com"))
	require.NoError(t, err)
	second, err := q.CreateUser(t.Context(), newUser("two", "two@example.com"))
	require.NoError(t, err)
	_, err = q.CreateUser(t.Context(), newUser("three", "three@example.com"))
	require.NoError(t, err)

	found, err := q.ListUsersByID(t.Context(), []int64{first.ID, second.ID})

	require.NoError(t, err)
	require.Len(t, found, 2)
}

func TestListUsersByIDReturnsEmptySliceNotNil(t *testing.T) {
	q, _ := testsupport.Queries(t)

	found, err := q.ListUsersByID(t.Context(), []int64{404})

	require.NoError(t, err)
	require.NotNil(t, found, "emit_empty_slices should give a marshalable empty slice")
	require.Empty(t, found)
}

func TestTruncationIsolatesTests(t *testing.T) {
	q, _ := testsupport.Queries(t)

	count, err := q.CountUsers(t.Context())

	require.NoError(t, err)
	require.Zero(t, count, "each test must start with an empty database")
}
