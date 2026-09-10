-- +goose Up
CREATE TABLE leaderboard_snapshots (
    id           bigint GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    scope        text        NOT NULL,
    period       text        NOT NULL,
    window_from  timestamptz NOT NULL,
    window_until timestamptz NOT NULL,
    player_count integer     NOT NULL,
    entries      jsonb       NOT NULL,
    created_at   timestamptz NOT NULL DEFAULT now(),
    CONSTRAINT leaderboard_snapshots_window_key UNIQUE (scope, period, window_from)
);

CREATE INDEX leaderboard_snapshots_lookup_idx
    ON leaderboard_snapshots (period, window_from DESC);

-- +goose Down
DROP TABLE leaderboard_snapshots;
