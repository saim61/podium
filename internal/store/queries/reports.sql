-- name: TopPlayersForGameInWindow :many
SELECT user_id, max(points)::int AS points
FROM score_events
WHERE game = $1
  AND achieved_at >= $2
  AND achieved_at < $3
GROUP BY user_id
ORDER BY points DESC, min(achieved_at)
LIMIT $4;

-- name: TopPlayersForGameAllTime :many
SELECT user_id, max(points)::int AS points
FROM score_events
WHERE game = $1
GROUP BY user_id
ORDER BY points DESC, min(achieved_at)
LIMIT $2;

-- name: TopPlayersGlobalInWindow :many
SELECT user_id, sum(best)::int AS points
FROM (
    SELECT user_id, game, max(points) AS best, min(achieved_at) AS first_at
    FROM score_events
    WHERE achieved_at >= $1
      AND achieved_at < $2
    GROUP BY user_id, game
) per_game
GROUP BY user_id
ORDER BY points DESC, min(first_at)
LIMIT $3;

-- name: TopPlayersGlobalAllTime :many
SELECT user_id, sum(best)::int AS points
FROM (
    SELECT user_id, game, max(points) AS best, min(achieved_at) AS first_at
    FROM score_events
    GROUP BY user_id, game
) per_game
GROUP BY user_id
ORDER BY points DESC, min(first_at)
LIMIT $1;

-- name: UpsertLeaderboardSnapshot :one
INSERT INTO leaderboard_snapshots (scope, period, window_from, window_until, player_count, entries)
VALUES ($1, $2, $3, $4, $5, $6)
ON CONFLICT (scope, period, window_from) DO UPDATE
SET window_until = excluded.window_until,
    player_count = excluded.player_count,
    entries      = excluded.entries,
    created_at   = now()
RETURNING *;

-- name: GetLeaderboardSnapshot :one
SELECT * FROM leaderboard_snapshots
WHERE scope = $1
  AND period = $2
  AND window_from = $3;

-- name: CountLeaderboardSnapshots :one
SELECT count(*) FROM leaderboard_snapshots;
