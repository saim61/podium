-- +goose Up
CREATE TABLE game_sessions (
    id          uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    user_id     bigint      NOT NULL REFERENCES users (id) ON DELETE CASCADE,
    game        text        NOT NULL,
    seed        bigint      NOT NULL,
    state       jsonb       NOT NULL,
    status      text        NOT NULL DEFAULT 'active',
    moves       integer     NOT NULL DEFAULT 0,
    started_at  timestamptz NOT NULL DEFAULT now(),
    deadline_at timestamptz,
    finished_at timestamptz,
    CONSTRAINT game_sessions_status_check
        CHECK (status IN ('active', 'finished', 'abandoned'))
);

CREATE INDEX game_sessions_active_idx ON game_sessions (user_id, game) WHERE status = 'active';
CREATE INDEX game_sessions_started_at_idx ON game_sessions (started_at);

CREATE TABLE score_events (
    id           bigint GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    user_id      bigint           NOT NULL REFERENCES users (id) ON DELETE CASCADE,
    session_id   uuid             NOT NULL UNIQUE REFERENCES game_sessions (id) ON DELETE CASCADE,
    game         text             NOT NULL,
    raw          double precision NOT NULL,
    points       integer          NOT NULL,
    achieved_at  timestamptz      NOT NULL DEFAULT now(),
    projected_at timestamptz,
    CONSTRAINT score_events_points_check CHECK (points BETWEEN 0 AND 10000)
);

CREATE INDEX score_events_user_game_idx ON score_events (user_id, game);
CREATE INDEX score_events_game_achieved_idx ON score_events (game, achieved_at DESC);
CREATE INDEX score_events_unprojected_idx ON score_events (id) WHERE projected_at IS NULL;

-- +goose Down
DROP TABLE score_events;
DROP TABLE game_sessions;
