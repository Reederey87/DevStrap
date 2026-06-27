## Summary

## Tests

- [ ] `gofmt -w cmd internal`
- [ ] `go test -race ./...`

## Safety Checklist

- [ ] No secrets are logged or persisted as plaintext.
- [ ] Dirty worktrees are not overwritten.
- [ ] Agent work starts from a fetched remote default ref, not a local default branch.
- [ ] Mac-specific behavior is behind an adapter or clearly isolated.
- [ ] User-visible CLI contract changes are reflected in `spec/` and README.
