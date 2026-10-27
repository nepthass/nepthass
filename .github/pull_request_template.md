## What and why

<!-- Describe the change and the reason for it. -->

## Checklist

- [ ] `gofmt -l .`, `go vet ./...` and `go test -race -shuffle=on -count=1 ./...` pass
- [ ] Tests cover the new behavior and need no live Discord or Home Assistant
- [ ] Security impact considered (authorization, secrets, new control domains)
- [ ] Docs and `CHANGELOG.md` updated if behavior changed
- [ ] No secrets, personal Discord IDs or private entity names
