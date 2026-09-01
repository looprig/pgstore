CREATE TABLE {{schema}}.{{ledger_scopes}} (
    name text PRIMARY KEY,
    tip bigint NOT NULL CHECK (tip >= 0)
);

CREATE TABLE {{schema}}.{{ledger_records}} (
    name text NOT NULL REFERENCES {{schema}}.{{ledger_scopes}}(name) ON DELETE CASCADE,
    seq bigint NOT NULL CHECK (seq > 0),
    payload bytea NOT NULL,
    PRIMARY KEY (name, seq)
);

CREATE TABLE {{schema}}.{{kv}} (
    key text PRIMARY KEY,
    revision bigint NOT NULL CHECK (revision > 0),
    value bytea NOT NULL
);
