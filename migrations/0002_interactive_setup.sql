-- v0.2.0 interactive setup: aliases, persistent setup sessions, and a minimal
-- registry of entity IDs seen by /sync. Existing v0.1.0 tables are untouched.

-- entity_aliases maps a canonical Home Assistant entity ID to a human-readable
-- alias. alias_key is the case-folded alias, unique across the installation so
-- an alias can never be ambiguous. An alias never grants access by itself.
CREATE TABLE entity_aliases (
    entity_id  TEXT PRIMARY KEY CHECK (length(entity_id) BETWEEN 3 AND 255),
    alias      TEXT NOT NULL CHECK (length(alias) BETWEEN 1 AND 128),
    alias_key  TEXT NOT NULL UNIQUE,
    updated_by TEXT NOT NULL,
    updated_at TEXT NOT NULL
) STRICT;

-- setup_sessions persists the /setup wizard so it survives restarts. state is
-- a JSON draft (selected entities, desired access and aliases, view state); it
-- never contains credentials. version is the optimistic-concurrency counter.
CREATE TABLE setup_sessions (
    id         TEXT PRIMARY KEY,
    guild_id   TEXT NOT NULL,
    admin_id   TEXT NOT NULL,
    step       TEXT NOT NULL CHECK (step IN ('health', 'discover', 'permissions', 'aliases', 'review', 'done')),
    status     TEXT NOT NULL CHECK (status IN ('active', 'completed', 'cancelled', 'expired')),
    version    INTEGER NOT NULL CHECK (version >= 1),
    state      TEXT NOT NULL,
    created_at TEXT NOT NULL,
    updated_at TEXT NOT NULL,
    expires_at TEXT NOT NULL
) STRICT;

-- At most one active session per guild and administrator.
CREATE UNIQUE INDEX idx_setup_sessions_one_active ON setup_sessions (guild_id, admin_id) WHERE status = 'active';
CREATE INDEX idx_setup_sessions_updated_at ON setup_sessions (updated_at);

-- known_entities is the minimal snapshot /sync keeps so it can report new,
-- removed and returned entities across restarts. Names and states are never
-- stored: Home Assistant stays the source of truth.
CREATE TABLE known_entities (
    entity_id     TEXT PRIMARY KEY CHECK (length(entity_id) BETWEEN 3 AND 255),
    domain        TEXT NOT NULL,
    first_seen_at TEXT NOT NULL,
    last_seen_at  TEXT NOT NULL,
    missing_since TEXT
) STRICT;
