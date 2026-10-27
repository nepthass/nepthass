# v0.2.0 design: Interactive Setup

Status: implemented on `feature/v0.2.0`. This document records the decisions the implementation follows; [architecture.md](../architecture.md) describes the resulting system.

## Goal

After the local bootstrap, an administrator configures which Home Assistant entities are shared (and at what level), and gives them human-readable aliases, entirely in Discord. No SQLite edits, no memorized entity IDs, no credentials in Discord.

## Decisions

| Topic | Decision |
| --- | --- |
| Packages | `internal/setup` (session model, state machine, store, service: Discord-free), `internal/entities` (aliases, discovery cache, sync, the shared grant rule), `internal/discord/wizard` (panels, custom IDs, component handling), `internal/discord/ui` (shared embed helpers). |
| One permission system | The wizard writes the same `entity_permissions` table and applies the same rule (`entities.CheckGrant`) as `/permissions`. No second store, no second rule set. |
| State machine | Explicit `Step` and `Event` tables in `internal/setup`; every transition goes through `Session.Transition`, which also enforces per-step guards. UI callbacks contain no transition logic. |
| Draft model | A session holds a draft: for every selected entity the desired access and alias plus the *base* (access and alias at the moment it was added). Nothing is written until the administrator confirms. |
| Persistence | `setup_sessions` row with a JSON draft, a `version` for optimistic concurrency, `status`, timestamps and `expires_at` (24 h after the last change). No credentials are ever stored. |
| Multiple sessions | At most **one active session per (guild, administrator)** (partial unique index). `/setup` resumes it (or offers to start over). Different administrators may each have a session; they cannot see or resume each other's. Concurrent edits across sessions or `/permissions` are caught at apply time by base comparison. |
| Concurrency | Every mutation is a compare-and-swap on `version`. Two concurrent interactions: one wins, the other is told the panel was refreshed. |
| Replay and stale panels | Every component custom ID carries the session ID and the session `version` it was rendered for. A mismatch re-renders the current panel instead of acting. The final confirmation additionally carries the version of the reviewed plan; applying flips `status` to `completed` in the same transaction as the writes, so a replayed confirmation cannot write twice. |
| Outdated confirmation | If an entity's permission or alias changed outside the session since it was added, apply aborts, rebases the session onto the live state, bumps the version (so the old Confirm button is dead) and sends the administrator back to Review. A permission revoked meanwhile is never silently restored. |
| Atomic apply | One SQLite transaction: verify the session (status, version), re-read live config, detect stale bases, validate, write permissions and aliases, write audit entries, mark the session completed. Any failure rolls everything back and leaves the session active. Home Assistant is queried **before** the transaction (entities must exist for grants). |
| Aliases | Stored in `entity_aliases`, keyed by canonical entity ID, unique case-insensitively across the installation (`alias_key`). 1 to 32 characters: letters, digits, spaces and `- _ ' . ( )` only, so aliases cannot carry markdown or mentions. An alias must not look like an entity ID. Duplicates are rejected, never auto-renamed, naming the conflicting entity. Aliases never grant access. |
| Alias use in commands | `/devices` and `/status` show the alias. Autocomplete suggests `Alias (entity_id)` and still submits the canonical ID. A typed alias resolves only to an entity the caller may already use at the required level; otherwise it behaves exactly like an unknown name (no existence leak). IDs are tried first, so resolution is deterministic. |
| Control | Unchanged: `light` and `switch` only, enforced when selecting, when applying, and again at execution time. |
| `/sync` | Refreshes the in-memory discovery snapshot and **persists a minimal registry**: `known_entities` holds only entity ID, domain and first/last seen / missing-since timestamps, so `/sync` can report new, removed and returned entities across restarts. Names, states and attributes are never stored; Home Assistant remains the source of truth. `/sync` never changes permissions or aliases. The first sync records a baseline and reports nothing as new. |
| Discovery cache | One shared snapshot of compact items (ID, name, state, device class, unit), refreshed on demand (Refresh, `/sync`, entering the wizard, apply) and otherwise reused for 30 s, so paging and search do not hit Home Assistant. Apply always re-fetches. |
| Discord limits | 10 entities per page, one string select (at most 25 options), at most 5 action rows, labels and values at most 100 characters, embeds at most 4000 description characters. Entity IDs longer than 100 characters cannot be offered in a select and are counted as skipped. |
| Acknowledgement | Commands and component clicks are deferred immediately (3 s limit) and the message is edited afterwards; opening a modal is the only immediate response. |

## State machine

```text
Health --next--> Discover --next--> Permissions --next--> Aliases --next--> Review --confirm--> Done
   ^------back------'  ^-----back------'  ^------back------'  ^-----back-----'
any active step --cancel--> Cancelled          (no changes are applied)
active session idle for 24 h --> Expired       (lazily, on next access)
```

Guards:

- Health to Discover: a fresh Home Assistant fetch must succeed (no stale or invented data).
- Discover to Permissions: at least one entity selected (at most 100 per session).
- Permissions: control only for lights and switches; a draft entity cannot be granted more than the rule allows.
- Aliases to Review: the draft aliases must be valid and unique among themselves.
- Review to Done: the plan must contain at least one change and pass live validation.

Operations are tied to steps: selection only in Discover, access only in Permissions, aliases only in Aliases. Other steps reject them.

## Schema (migration `0002_interactive_setup.sql`)

```sql
entity_aliases(entity_id PK, alias, alias_key UNIQUE, updated_by, updated_at)
setup_sessions(id PK, guild_id, admin_id, step, status, version, state JSON, created_at, updated_at, expires_at)
  UNIQUE INDEX on (guild_id, admin_id) WHERE status = 'active'
known_entities(entity_id PK, domain, first_seen_at, last_seen_at, missing_since)
```

No foreign keys: permissions and aliases are intentionally independent of each other and of Home Assistant (entities can disappear). Existing `entity_permissions` and `command_audit_logs` rows are untouched.

## Security boundaries

- Every interaction (command, component, modal, autocomplete) goes through the same guild and user authorization first; the setup service then requires administrator level again on every operation.
- Custom IDs and select values are untrusted: the session is loaded from the database and must belong to the calling user and guild; selected values must be members of the page the server computes itself; entity IDs are re-validated.
- No credentials in setup: the wizard only accepts search text and aliases, and the session stores only IDs, aliases and view state.
- All replies stay ephemeral and mentions are disabled. Aliases are character-restricted and escaped.
- Component clicks have their own per-user rate limit (30 per 10 seconds), separate from slash commands.
- Everything the wizard applies is audited (one entry per entity change plus a summary), in the same transaction as the change.

## Non-goals (later milestones)

WebSocket monitoring, subscriptions, notifications, rules, the `/home` dashboard, and alias-based fuzzy matching.
