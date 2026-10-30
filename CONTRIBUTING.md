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

CI runs the same checks. To try NeptHass against a real Home Assistant without any hardware, use the local development environment in [docs/development/setup.md](docs/development/setup.md); the integration tests behind the `integration` build tag run against it, and [docs/development/smoke-tests.md](docs/development/smoke-tests.md) describes them and the manual Discord checklist. Tests must **never** need live Discord or Home Assistant credentials: use `httptest` servers (see `tests/fakeha`) and temporary SQLite files (`t.TempDir()`). Tests must be deterministic: no fixed sleeps, wait on the event or state you care about with a bounded timeout. Assert behavior and machine-consumed values, not prose.

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

Open an issue using the bug report form with: NeptHass version (`nepthass version`), how you deploy it, what you did, what you expected, what happened, and the relevant log lines (`LOG_LEVEL=debug` if needed). Redact tokens, IDs and entity names you consider private. Check [docs/troubleshooting.md](docs/troubleshooting.md) first.

## Pull request checklist

- [ ] Focused change with a clear description of what and why
- [ ] Tests added or updated; the verification commands above pass
- [ ] Security impact considered (authorization, secrets, new domains)
- [ ] Docs and `CHANGELOG.md` updated when behavior changes
- [ ] No secrets, personal IDs or private entity names

## Proposing features

Open an issue first for anything beyond a small fix, describing the problem you want solved (not only the solution), who it is for, and any security impact. Work follows the milestones in the [roadmap](docs/roadmap.md); a proposal that fits the current milestone is easier to accept than a new direction. Larger changes should come with a short design note like the ones in [docs/design/](docs/design/).

## Pull request workflow

1. Branch from `main` (`feature/...`, `fix/...`, `docs/...` or `chore/...`).
2. Keep commits small and logical, with tests next to the code they cover.
3. Open a pull request against `main`; the PR template has the checklist. CI runs the formatting, vet, build and race-test job and the Compose and image-build job; both must pass.
4. Pull requests are merged with a merge commit. Please do not force-push a branch under review.

## Maintainers: releasing

A release follows the same steps every time (this is how v0.3.0 and v0.4.0 were released):

1. Finish the milestone on its `feature/vX.Y.Z` branch: tests, docs, and a dated `CHANGELOG.md` entry with the compare links.
2. Verify: the checks above, `go vet -tags=integration ./tests/integration/...`, the integration suite against a real Home Assistant, and `docker compose config --quiet` for every Compose file CI checks.
3. Push the branch and open a pull request titled `feat: vX.Y.Z <summary>`. Wait for CI.
4. Merge with a merge commit titled `Merge feature/vX.Y.Z: <Name> (#N)`.
5. Create an annotated tag `vX.Y.Z` on that merge commit and push only that tag. Never move, delete or reuse a tag.
6. Publish a GitHub Release titled `NeptHass vX.Y.Z — <Name>` (stable, latest) with the sections *What's New, Security, Reliability, Upgrade Notes, Known Limitations, Verification, Documentation* and a link to the full changelog. State what was and was not tested.
7. Never force-push `main`. Branch protection is not available on the current GitHub plan, so the process above is the control.
