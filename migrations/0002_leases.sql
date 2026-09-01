CREATE TABLE {{schema}}.{{leases}} (
    name text PRIMARY KEY,
    epoch bigint NOT NULL CHECK (epoch >= 0),
    holder bytea,
    expires_at timestamptz,
    revision bigint NOT NULL CHECK (revision >= 0),
    CHECK ((holder IS NULL) = (expires_at IS NULL))
);
