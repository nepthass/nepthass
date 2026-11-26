# Development and testing

## Standard checks

```bash
gofmt -l .
go mod verify
go vet ./...
go vet -tags=integration ./...
go build ./...
go test -race -shuffle=on -count=1 ./...
docker compose config --quiet
docker build -t nepthass:dev .
```

Tests use temporary SQLite databases and local HTTP/WebSocket fakes. They do not require live credentials.

## Real Home Assistant integration suite

`compose.dev.yaml` starts an isolated Home Assistant with virtual entities. It uses the `nepthass-dev` Compose project, localhost port `18123`, a private network, and project-scoped volumes.

```bash
cp .env.dev.example .env.dev
docker compose --env-file .env.dev -f compose.dev.yaml up -d homeassistant
```

Finish Home Assistant onboarding, create a long-lived token, and put it in `.env.dev` as `HA_TOKEN`. Then run:

```bash
docker compose --env-file .env.dev -f compose.dev.yaml \
  --profile test run --rm integration-tests
```

The integration tests refuse any Home Assistant that does not contain the `nepthass_dev_*` simulation. They cover REST, WebSocket events, reconnect and reconciliation, discovery, synchronization, control, monitoring, notifications, persistence, and authorization. They do not connect to Discord.

Use the scenario scripts provided by the development Home Assistant to reset or change virtual entity states. Never point the suite at a real home or reuse production volumes.

## Manual Discord validation

Use a private test guild, a dedicated bot application, the isolated Home Assistant, and separate test identities for administrator, regular user, outsider, another guild, and direct messages.

Validate startup, `/ping`, `/health`, `/setup`, `/devices`, `/status`, `/control`, `/sync`, `/permissions`, `/sensors`, `/notifications`, and `/audit`. Confirm denied identities reveal no entity data, notification channels remain private, and logs contain no tokens.

Manual checks count as passed only when their results were directly observed.

## Release images

The release-container workflow builds multi-platform images for version tags and publishes them to GitHub Container Registry with an SBOM and provenance attestation. Release-candidate tags do not move `latest`. CI pull requests build but never publish images. After the first publication, a repository owner must make the package public in its GitHub package settings so users can pull it without signing in.
