-- name: CreateUser :one
INSERT INTO users (username, email, password_hash)
VALUES ($1, $2, $3)
RETURNING *;

-- name: GetUserByID :one
SELECT * FROM users
WHERE id = $1;

-- name: GetUserByUsername :one
SELECT * FROM users
WHERE lower(username) = lower($1);

-- name: GetUserByEmail :one
SELECT * FROM users
WHERE lower(email) = lower($1);

-- name: GetUserByLogin :one
SELECT * FROM users
WHERE lower(username) = lower($1)
   OR lower(email) = lower($1);

-- name: UpdateUserPasswordHash :exec
UPDATE users
SET password_hash = $2,
    updated_at    = now()
WHERE id = $1;

-- name: ListUsersByID :many
SELECT * FROM users
WHERE id = ANY($1::bigint[]);

-- name: CountUsers :one
SELECT count(*) FROM users;
