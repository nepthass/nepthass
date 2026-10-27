-- v0.1.0 foundation: entity permissions and the command audit log.

-- entity_permissions is the explicit allowlist. Entities without a row are
-- invisible to NeptHass users. 'read' allows status queries; 'control' also
-- allows turning the entity on or off (only light and switch domains can ever
-- be controlled, enforced in code regardless of what is stored here).
CREATE TABLE entity_permissions (
    entity_id  TEXT PRIMARY KEY CHECK (length(entity_id) BETWEEN 3 AND 255),
    access     TEXT NOT NULL CHECK (access IN ('read', 'control')),
    granted_by TEXT NOT NULL,
    updated_at TEXT NOT NULL
) STRICT;

-- command_audit_logs records control attempts and permission changes.
-- Rows older than the configured retention are pruned periodically.
CREATE TABLE command_audit_logs (
    id          INTEGER PRIMARY KEY AUTOINCREMENT,
    occurred_at TEXT NOT NULL,
    actor_id    TEXT NOT NULL,
    command     TEXT NOT NULL,
    entity_id   TEXT,
    action      TEXT NOT NULL,
    outcome     TEXT NOT NULL CHECK (outcome IN ('success', 'denied', 'error')),
    detail      TEXT NOT NULL DEFAULT ''
) STRICT;

CREATE INDEX idx_command_audit_logs_occurred_at ON command_audit_logs (occurred_at);
