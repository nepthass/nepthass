-- v0.4.0 notifications: the guild's alert channel and per-entity notification rules.
-- Existing tables are untouched. Rules deliberately have NO foreign key to
-- entity_permissions: revoking a share is a DELETE there, and a cascade would
-- destroy configuration while a restrict would block revocation. A rule whose
-- entity is no longer shared (or no longer exists) is dormant, never deleted.
--
-- No settings row means notifications are disabled.

CREATE TABLE notification_settings (
    guild_id   TEXT PRIMARY KEY
        CHECK (length(guild_id) BETWEEN 5 AND 32 AND guild_id NOT GLOB '*[^0-9]*'),
    channel_id TEXT NOT NULL
        CHECK (length(channel_id) BETWEEN 5 AND 32 AND channel_id NOT GLOB '*[^0-9]*'),
    enabled    INTEGER NOT NULL DEFAULT 1 CHECK (enabled IN (0, 1)),
    updated_by TEXT NOT NULL CHECK (length(updated_by) > 0),
    updated_at TEXT NOT NULL
);

-- kind 'state' fires on the category's alert state (open, wet, detected, low);
-- 'above' and 'below' compare a numeric sensor with threshold. The category is
-- derived from live sensor data at runtime and is therefore not stored.
-- cooldown_seconds: NULL selects the category default, 0 means no cooldown.
CREATE TABLE notification_rules (
    id               INTEGER PRIMARY KEY AUTOINCREMENT,
    entity_id        TEXT NOT NULL CHECK (length(entity_id) BETWEEN 3 AND 255),
    kind             TEXT NOT NULL CHECK (kind IN ('state', 'above', 'below')),
    threshold        REAL CHECK (threshold IS NULL OR (threshold BETWEEN -1000000 AND 1000000)),
    enabled          INTEGER NOT NULL DEFAULT 1 CHECK (enabled IN (0, 1)),
    cooldown_seconds INTEGER CHECK (cooldown_seconds IS NULL OR cooldown_seconds BETWEEN 0 AND 86400),
    created_by       TEXT NOT NULL CHECK (length(created_by) > 0),
    created_at       TEXT NOT NULL,
    updated_by       TEXT NOT NULL CHECK (length(updated_by) > 0),
    updated_at       TEXT NOT NULL,
    UNIQUE (entity_id, kind),
    CHECK ((kind = 'state' AND threshold IS NULL) OR (kind IN ('above', 'below') AND threshold IS NOT NULL))
);

-- Backstop for the 200-rule limit: the service checks it inside its immediate
-- transaction, and this trigger makes it impossible to exceed through any path.
CREATE TRIGGER notification_rules_limit
BEFORE INSERT ON notification_rules
WHEN (SELECT count(*) FROM notification_rules) >= 200
BEGIN
    SELECT RAISE(ABORT, 'notification rule limit reached');
END;
