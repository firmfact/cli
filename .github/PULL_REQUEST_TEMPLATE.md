## What and why

<!-- What was wrong or missing, and what this changes. Link the issue, as in "Fixes #123". -->

## How it was tested

<!-- The tests you added or changed, and anything you tried by hand (with which command, and against which kind of host). -->

## Checklist

- [ ] `go test -race ./...` passes.
- [ ] `gofmt -l .` prints nothing, and `go vet ./...` is clean, also with `GOOS=windows` and `GOOS=darwin`.
- [ ] golangci-lint is clean, at the version in CONTRIBUTING.md.
- [ ] New behaviour and each fix have a test, which fails without the change.
- [ ] Tests keep to temporary directories (`isolate(t)`, or `FIRMFACT_CONFIG_DIR`, `FIRMFACT_CACHE_DIR`, `HOME` and `FIRMFACT_TOKEN_STORE=file`) and fake servers, never a real sign-in, keyring or service.
- [ ] The README and `--help` say what a user now sees, and CHANGELOG.md has a line under Unreleased for it.
- [ ] Every commit is signed off (`git commit -s`), as CONTRIBUTING.md asks.

## Compatibility

<!-- Does this change `--json` output, an exit status, a flag or command, an environment variable, or how workspace commands and their flags are generated? If so, say what and why; see "Compatibility and versioning" in CONTRIBUTING.md. Otherwise write "None". -->
