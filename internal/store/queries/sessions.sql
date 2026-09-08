-- name: CreateGameSession :one
INSERT INTO game_sessions (user_id, game, seed, state, deadline_at)
VALUES ($1, $2, $3, $4, $5)
RETURNING *;

-- name: GetGameSession :one
SELECT * FROM game_sessions
WHERE id = $1;

-- name: LockGameSession :one
SELECT * FROM game_sessions
WHERE id = $1
FOR UPDATE;

-- name: SaveGameSessionState :one
UPDATE game_sessions
SET state = $2,
    moves = moves + 1
WHERE id = $1
RETURNING *;

-- name: FinishGameSession :one
UPDATE game_sessions
SET status      = 'finished',
    finished_at = now()
WHERE id = $1
  AND status = 'active'
RETURNING *;

-- name: AbandonActiveSessions :execrows
UPDATE game_sessions
SET status      = 'abandoned',
    finished_at = now()
WHERE user_id = $1
  AND game = $2
  AND status = 'active';

-- name: CountActiveGameSessions :one
SELECT count(*) FROM game_sessions
WHERE user_id = $1
  AND status = 'active';

-- name: CreateScoreEvent :one
INSERT INTO score_events (user_id, session_id, game, raw, points)
VALUES ($1, $2, $3, $4, $5)
RETURNING *;

-- name: GetScoreEventBySession :one
SELECT * FROM score_events
WHERE session_id = $1;

-- name: ListRecentScoreEventsForUser :many
SELECT * FROM score_events
WHERE user_id = $1
ORDER BY achieved_at DESC
LIMIT $2;

-- name: MarkScoreEventProjected :exec
UPDATE score_events
SET projected_at = now()
WHERE id = $1;

-- name: ListUnprojectedScoreEvents :many
SELECT * FROM score_events
WHERE projected_at IS NULL
ORDER BY id
LIMIT $1;

-- name: BestPointsPerUserAndGame :many
SELECT game, user_id, max(points)::int AS best
FROM score_events
WHERE (sqlc.narg(from_time)::timestamptz IS NULL OR achieved_at >= sqlc.narg(from_time))
  AND (sqlc.narg(until_time)::timestamptz IS NULL OR achieved_at < sqlc.narg(until_time))
GROUP BY game, user_id;

-- name: AbandonStaleSessions :execrows
UPDATE game_sessions
SET status      = 'abandoned',
    finished_at = now()
WHERE status = 'active'
  AND started_at < $1;

-- name: CountUnprojectedScoreEvents :one
SELECT count(*) FROM score_events
WHERE projected_at IS NULL;

-- name: Now :one
SELECT now()::timestamptz AS now;
