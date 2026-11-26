-- Independent of the bounded risk history and of a draining slot's settings JSON.
CREATE TABLE IF NOT EXISTS account_risk_401_incidents (
    owner_id BIGINT NOT NULL REFERENCES accounts(id) ON DELETE CASCADE,
    group_id BIGINT NOT NULL REFERENCES groups(id) ON DELETE CASCADE,
    first_event_id TEXT NOT NULL,
    last_401_at TIMESTAMPTZ NOT NULL,
    recovered_at TIMESTAMPTZ,
    occurrences BIGINT NOT NULL CHECK (occurrences > 0),
    PRIMARY KEY (owner_id, group_id)
);
