# Contributing to NeptHass

Thanks for helping. NeptHass controls real homes, so changes are reviewed with security and reliability in mind. Please read the [Code of Conduct](CODE_OF_CONDUCT.md); it applies to all project spaces.

## Getting set up

You need Go (see `go.mod` for the version) or Docker.

```bash
git clone https://github.com/nepthass/nepthass.git && cd nepthass
go build ./cmd/nepthass
```

Without a local Go toolchain, run any Go command in a container:

```bash
docker run --rm -v "$PWD":/src -w /src golang:1.27 go test ./...
```

## Before you open a pull request

All of these must pass:

```bash
gofmt -l .                                   # prints nothing
go vet ./...
go test -race -shuffle=on -count=1 ./...
docker compose config --quiet
docker build -t nepthass:dev .
```

CI runs the same checks. Tests must **never** need live Discord or Home Assistant credentials: use `httptest` servers (see `tests/fakeha`) and temporary SQLite files (`t.TempDir()`). Tests must be deterministic: no fixed sleeps, wait on the event or state you care about with a bounded timeout. Assert behavior and machine-consumed values, not prose.

## Code style

- Idiomatic Go, `gofmt`ed. Handle every error; wrap with `%w`; use sentinel or typed errors that callers can check with `errors.Is`/`errors.As`.
- Pass `context.Context` first and propagate it. Keep interfaces small and defined where they are used.
- Structured logging with `log/slog`. Never log secrets, tokens or full request bodies.
- SQL is always parameterized. Schema changes are new files in `migrations/` (`NNNN_description.sql`); never edit a released migration (a checksum check will refuse to start).
- Keep Discord-specific code in `internal/discord`; business rules belong in `internal/entities` and friends.
- Prefer the standard library. A new dependency needs a clear justification in the pull request and a note in the changelog.
- Keep files focused (aim for under about 250 lines) and avoid abstractions without a second user.

## Security rules for contributions

- No secrets, tokens, real Discord IDs, personal entity IDs or private home details in code, tests, docs, issues or logs. Use obviously fake values.
- **Controlling a new Home Assistant domain requires an explicit security review.** Do not add a domain to the allowlists (`entities.Domain.Controllable`, `rest.allowedServices`) without a dedicated proposal discussed with the maintainers and tests proving unauthorized paths stay closed.
- Anything that widens who can do what (authorization, defaults, permissions) must come with tests that fail if the check is removed.
- Vulnerabilities are reported privately: see [SECURITY.md](SECURITY.md).

## Commit messages

Imperative mood, short subject (about 72 characters), optional body explaining why. Examples: `Add audit retention pruning`, `Refuse control of unlisted domains`. Keep commits focused; implementation and its tests belong together.

## Reporting bugs

Open an issue with: NeptHass version (`nepthass version`), how you deploy it, what you did, what you expected, what happened, and the relevant log lines (`LOG_LEVEL=debug` if needed). Redact tokens, IDs and entity names you consider private. Check [docs/troubleshooting.md](docs/troubleshooting.md) first.

## Pull request checklist

- [ ] Focused change with a clear description of what and why
- [ ] Tests added or updated; the verification commands above pass
- [ ] Security impact considered (authorization, secrets, new domains)
- [ ] Docs and `CHANGELOG.md` updated when behavior changes
- [ ] No secrets, personal IDs or private entity names

## Roadmap

Work follows the milestones in the README. Please discuss large features in an issue before building them so they fit the milestone.
