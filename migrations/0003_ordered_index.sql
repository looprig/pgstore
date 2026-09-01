CREATE TABLE {{schema}}.{{ordered_scopes}} (
    namespace text COLLATE "C" NOT NULL,
    ordering_scope text COLLATE "C" NOT NULL,
    next_order numeric(20, 0) NOT NULL CHECK (next_order >= 0 AND next_order <= 18446744073709551615),
    PRIMARY KEY (namespace, ordering_scope)
);

CREATE TABLE {{schema}}.{{ordered_records}} (
    namespace text COLLATE "C" NOT NULL,
    ordering_scope text COLLATE "C" NOT NULL,
    stable_key text COLLATE "C" NOT NULL,
    ranking_scope text COLLATE "C" NOT NULL,
    revision numeric(20, 0) NOT NULL CHECK (revision > 0 AND revision <= 18446744073709551615),
    order_id numeric(20, 0) NOT NULL CHECK (order_id > 0 AND order_id <= 18446744073709551615),
    value bytea NOT NULL CHECK (octet_length(value) <= 1048576),
    value_is_nil boolean NOT NULL,
    ranked boolean NOT NULL,
    rank_value bigint NOT NULL,
    due_state smallint NOT NULL,
    due_at bigint NOT NULL,
    deleted boolean NOT NULL DEFAULT false,
    PRIMARY KEY (namespace, ordering_scope, stable_key),
    CHECK ((due_state = 0 AND due_at = 0) OR due_state = 1),
    CHECK (NOT deleted OR (NOT ranked AND due_state = 0 AND due_at = 0))
);

CREATE INDEX {{ordered_order_idx}}
    ON {{schema}}.{{ordered_records}} (namespace, ordering_scope, order_id, stable_key);

CREATE INDEX {{ordered_rank_idx}}
    ON {{schema}}.{{ordered_records}} (namespace, ranking_scope, rank_value DESC, stable_key DESC, ordering_scope DESC)
    WHERE ranked AND NOT deleted;

-- ListDue has no scope filter in Storage v0.6.0. The complete key after the
-- namespace and canonical due-state discriminator is its released total order.
CREATE INDEX {{ordered_due_idx}}
    ON {{schema}}.{{ordered_records}} (namespace, due_state, due_at, stable_key, ordering_scope)
    WHERE due_state = 1 AND NOT deleted;
