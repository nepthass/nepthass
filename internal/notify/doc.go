// Package notify holds the notification configuration of v0.4.0: the guild's
// alert channel and the per-entity rules, and the administrator-only service
// that changes them.
//
// This package is configuration only. It evaluates no rule, sends nothing and
// makes no Discord call. In particular it cannot know whether a channel exists,
// belongs to the configured guild or lets the bot post: it only checks that a
// channel ID is well formed, and the Discord adapter is responsible for the rest.
//
// Every mutation requires auth.LevelAdmin and is audited in the same SQLite
// transaction as the change it describes, so a change without its audit entry
// cannot be committed. Refused attempts are audited too (outside any
// transaction, since nothing changed).
package notify
