-- name: CreateRefreshToken :one
INSERT INTO refresh_tokens (user_id, family_id, token_hash, expires_at)
VALUES ($1, $2, $3, $4)
RETURNING *;

-- name: ClaimRefreshToken :one
UPDATE refresh_tokens
SET used_at = now()
WHERE token_hash = $1
  AND used_at IS NULL
  AND revoked_at IS NULL
  AND expires_at > now()
RETURNING *;

-- name: GetRefreshTokenByHash :one
SELECT * FROM refresh_tokens
WHERE token_hash = $1;

-- name: RevokeRefreshTokenFamily :execrows
UPDATE refresh_tokens
SET revoked_at = now()
WHERE family_id = $1
  AND revoked_at IS NULL;

-- name: CountActiveRefreshTokensInFamily :one
SELECT count(*) FROM refresh_tokens
WHERE family_id = $1
  AND revoked_at IS NULL
  AND used_at IS NULL
  AND expires_at > now();

-- name: DeleteExpiredRefreshTokens :execrows
DELETE FROM refresh_tokens
WHERE expires_at < now();
