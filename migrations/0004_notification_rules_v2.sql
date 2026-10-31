-- Migration 0004 (v0.5.0): duration rules, hysteresis, unit-bound thresholds and
-- start-up alert marks.
--
-- notification_rules is rebuilt (SQLite cannot alter a constraint) so that one
-- entity can have several rules of the same kind with different durations:
-- UNIQUE (entity_id, kind) becomes UNIQUE (entity_id, kind, duration_seconds).
-- Every existing rule keeps its ID, its AUTOINCREMENT sequence (so a rule ID is
-- never reused in the audit trail), its thresholds, cooldown and authorship; it
-- becomes an immediate rule (duration 0) with no clear value. A numeric rule
-- gets an empty unit, which means "created before unit binding": it does not
-- evaluate until it is bound (see the notify package). The 200-rule trigger is
-- recreated. The whole file runs in one transaction, so a failure leaves the
-- 0003 schema and data untouched.

CREATE TEMP TABLE _rules_seq AS SELECT seq FROM sqlite_sequence WHERE name = 'notification_rules';

CREATE TABLE notification_rules_new (
    id               INTEGER PRIMARY KEY AUTOINCREMENT,
    entity_id        TEXT NOT NULL CHECK (length(entity_id) BETWEEN 3 AND 255),
    kind             TEXT NOT NULL CHECK (kind IN ('state', 'above', 'below')),
    threshold        REAL CHECK (threshold IS NULL OR (threshold BETWEEN -1000000 AND 1000000)),
    clear_threshold  REAL CHECK (clear_threshold IS NULL OR (clear_threshold BETWEEN -1000000 AND 1000000)),
    unit             TEXT NOT NULL DEFAULT '' CHECK (length(unit) <= 32),
    duration_seconds INTEGER NOT NULL DEFAULT 0 CHECK (duration_seconds BETWEEN 0 AND 86400),
    enabled          INTEGER NOT NULL DEFAULT 1 CHECK (enabled IN (0, 1)),
    cooldown_seconds INTEGER CHECK (cooldown_seconds IS NULL OR cooldown_seconds BETWEEN 0 AND 86400),
    created_by       TEXT NOT NULL CHECK (length(created_by) > 0),
    created_at       TEXT NOT NULL,
    updated_by       TEXT NOT NULL CHECK (length(updated_by) > 0),
    updated_at       TEXT NOT NULL,
    UNIQUE (entity_id, kind, duration_seconds),
    CHECK ((kind = 'state' AND threshold IS NULL AND clear_threshold IS NULL)
        OR (kind IN ('above', 'below') AND threshold IS NOT NULL)),
    CHECK (clear_threshold IS NULL
        OR (kind = 'above' AND clear_threshold < threshold)
        OR (kind = 'below' AND clear_threshold > threshold))
);

INSERT INTO notification_rules_new
    (id, entity_id, kind, threshold, enabled, cooldown_seconds, created_by, created_at, updated_by, updated_at)
SELECT id, entity_id, kind, threshold, enabled, cooldown_seconds, created_by, created_at, updated_by, updated_at
FROM notification_rules ORDER BY id;

DROP TRIGGER notification_rules_limit;
DROP TABLE notification_rules;
ALTER TABLE notification_rules_new RENAME TO notification_rules;

CREATE TRIGGER notification_rules_limit
BEFORE INSERT ON notification_rules
WHEN (SELECT count(*) FROM notification_rules) >= 200
BEGIN
    SELECT RAISE(ABORT, 'notification rule limit reached');
END;

INSERT INTO sqlite_sequence (name, seq)
    SELECT 'notification_rules', seq FROM _rules_seq
    WHERE NOT EXISTS (SELECT 1 FROM sqlite_sequence WHERE name = 'notification_rules');
UPDATE sqlite_sequence SET seq = (SELECT seq FROM _rules_seq)
    WHERE name = 'notification_rules' AND (SELECT seq FROM _rules_seq) > seq;
DROP TABLE _rules_seq;

CREATE TABLE notification_marks (
    rule_id      INTEGER PRIMARY KEY REFERENCES notification_rules (id) ON DELETE CASCADE,
    episode_key  TEXT NOT NULL CHECK (length(episode_key) <= 64),
    delivered_at TEXT NOT NULL
) STRICT;

ALTER TABLE notification_settings ADD COLUMN startup_summary_at TEXT;
