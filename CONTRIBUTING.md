# Contributing

Thank you for helping with the firmfact CLI. Bug reports, fixes and ideas are
all welcome. This page says how to set up, what a change needs before it can
be merged, and which parts of the CLI scripts rely on, so that they do not
change lightly.

Please follow our [code of conduct](CODE_OF_CONDUCT.md). Report a security
issue privately, never in an issue or pull request; see
[SECURITY.md](SECURITY.md).

## Before you start

- For a bug, open an issue with the bug form. It asks for the output of
  `firmfact doctor`, which shows your email address and paths on your
  machine; take out anything private first.
- For anything bigger than a small fix, such as a new command or flag, open an
  issue first, so that we can agree on the shape before you write the code.
- The workspace commands (`vendors list`, `analyze cost-trends` and the rest)
  and their flags come from the firmfact service, not from this repository. A
  wrong answer from one of them is the service's to fix; how the CLI turns a
  tool into a command, and how it prints the answer, is this repository's.

## Setting up

The Go version is pinned in `mise.toml`. With [mise](https://mise.jdx.dev):

```bash
git clone https://github.com/firmfact/cli firmfact-cli
cd firmfact-cli
mise install
mise exec -- go build -o bin/firmfact ./cmd/firmfact
```

Without mise, a Go at least as new as the `go` line in `go.mod` works. The
commands below leave out `mise exec --`; add it unless mise is active in your
shell.

## Checks

Run these before you open a pull request; CI runs them too, and a pull
request is merged only when they pass.

```bash
gofmt -l .                  # prints nothing
go vet ./...
GOOS=windows go vet ./...   # the Windows files
GOOS=darwin go vet ./...    # and the macOS ones
go test -race ./...
go run github.com/golangci/golangci-lint/v2/cmd/golangci-lint@v2.14.0 run
go mod tidy -diff
```

The linters and their settings are in `.golangci.yml`; the version above is
the one `.github/workflows/ci.yml` pins. CI also runs the tests on macOS and
Windows, in random order (`-shuffle=on`, so a test must not depend on another
having run), checks coverage (`.github/coverage-gate` has the floors),
runs govulncheck, and builds a GoReleaser snapshot twice to check that the
archives are reproducible. A workflow of its own fuzzes the code that reads
what comes over the network (see [Fuzzing](#fuzzing)).

### Writing tests

- Tests never touch your own sign-in, configuration or keyring. Call
  `isolate(t)` in `cmd` tests, which points `HOME`, `FIRMFACT_CONFIG_DIR` and
  `FIRMFACT_CACHE_DIR` at temporary directories and sets
  `FIRMFACT_TOKEN_STORE=file`; elsewhere set those yourself with `t.Setenv`.
- Tests talk to fake servers from `net/http/httptest`, never to firmfact
  itself, so you need no server or account to run them.
- A test that only holds on some platforms goes in a file named for them,
  such as `render_linux_test.go`, or skips itself with the reason.
- A fix comes with the test that failed before it.

### Shells

`claim` writes a block for the rc file of bash, zsh or fish, and its tests
run that block in each of them, skipping a shell that is not installed. The
block has to work in bash 3.2 too, the `/bin/bash` of macOS. With Docker you
can run the tests in every shell:

```bash
dir="$(mktemp -d)"
CGO_ENABLED=0 go test -c -o "$dir/claim.test" ./internal/claim
printf 'FROM alpine:3.20\nRUN apk add --no-cache bash zsh fish\n' | docker build -q -t claim-shells -
for image in bash:3.2 bash:5 claim-shells; do
  docker run --rm --user "$(id -u)" -e HOME=/tmp -v "$dir:/t:ro" "$image" /t/claim.test -test.v
done
```

### Fuzzing

The code that reads what reaches the CLI over the network has fuzz tests: a
tool's answer on its way to a table, CSV or TSV (`FuzzFindRows` and
`FuzzColumnsFor` in `cmd`), a document an upload reads back on its way to
the terminal (`FuzzDocumentBlock` in `cmd`), and a release's version,
GitHub's feed of releases, and a release's `checksums.txt` and archive
(`FuzzVersions`, `FuzzFeed`, `FuzzChecksumFor` and `FuzzExtract` in
`internal/update`). `go test` runs their seeds and the inputs in each
package's `testdata/fuzz`, among them real release archives, a real feed
and a real `checksums.txt`. `.github/workflows/fuzz.yml` fuzzes each target for 30
seconds on every pull request and for 10 minutes every night. To fuzz one
yourself:

```bash
go test -run '^$' -fuzz '^FuzzColumnsFor$' -fuzztime 1m ./cmd
```

An input that fails is written to `testdata/fuzz/<target>/` in the package,
and CI uploads it with the run. Commit it with the fix, so that `go test`
tries it from then on. New code that parses what the network sends gets a
fuzz target of its own, and a line in the workflow's matrix.

## Trying a change by hand

The firmfact service is not part of this repository. To try a build against
it, sign up: it is free, and the CLI works with the Demo workspace on every
plan. Point the build at directories of its own, so that it stays clear of
the sign-in and settings of any firmfact CLI you have installed:

```bash
export FIRMFACT_CONFIG_DIR="$(mktemp -d)" FIRMFACT_CACHE_DIR="$(mktemp -d)" FIRMFACT_TOKEN_STORE=file
./bin/firmfact doctor
```

If you have a firmfact server running locally, point the build at it with
`--host` (or `FIRMFACT_HOST`). `localhost:<port>` is the one host the CLI
talks plain http to; any other host needs https:

```bash
./bin/firmfact --host localhost:3000 doctor
./bin/firmfact --host localhost:3000 login
./bin/firmfact --host localhost:3000 --debug vendors list
```

`--debug` logs every request and answer on stderr, with tokens, codes and
passwords redacted.

## Commits and pull requests

- Keep a pull request to one change, in commits that each build and pass the
  tests.
- The subject line says what changed, in plain words: capitalised, without a
  full stop or a prefix such as `feat:`, and no longer than about 72
  characters. The body, wrapped at 72, says what was wrong or missing, then
  what the change does (a list works well), then how you tested it. `git log`
  has plenty of examples.
- Sign off every commit (see below).
- A change that users will notice gets a line in
  [CHANGELOG.md](CHANGELOG.md), under Unreleased and the heading its
  introduction gives for that kind of change; a breaking change goes under
  Breaking and says what to do instead. The release notes are made from
  that file rather than from commit subjects, which is why a subject needs no
  type prefix.
- When a change alters what a user sees, update the README and the command's
  `--help` with it. User-facing text is in British English (licence,
  organisation, colour), without em dashes, and writes firmfact in lower case
  except at the start of a sentence.
- Say in the pull request when a change touches anything under
  [Compatibility](#compatibility-and-versioning), and why.

### Sign-off

We ask for a sign-off under the
[Developer Certificate of Origin](https://developercertificate.org) 1.1, and
no contributor licence agreement. Signing off a commit certifies that you
wrote it, or otherwise have the right to contribute it under this project's
licence. `git commit -s` adds the line, from your git name and email address:

```text
Signed-off-by: Jan de Vries <jan@example.com>
```

`git rebase --signoff main` adds it to the commits you already made. A pull
request is merged only when every commit in it is signed off.

Under section 5 of the [Apache License 2.0](LICENSE), what you contribute is
licensed on the same terms as the rest of the code, and you keep the
copyright in it. That is why a sign-off is enough. The licence grants no
rights in the firmfact name or logo (section 6, and see [NOTICE](NOTICE)),
and a contribution does not change that: Dutchcode B.V. keeps the trademark.

## Compatibility and versioning

Releases follow [Semantic Versioning](https://semver.org). Scripts and CI
jobs depend on the parts of the CLI listed below, so a change that breaks one
of them waits for a new major version. Before 1.0.0, a minor release (0.x.0)
may break them, and its release notes say so under Breaking; a patch release
never does.

**`--json` output.** The JSON each command prints on stdout: the `data`,
`meta` and `notes` keys of a workspace command, one row per line with
`--all`, the objects that built-in commands such as `doctor`, `update`,
`version` and `workspaces status` print, and the error object on stderr (`error.message`,
`error.code` and `error.status`). A minor release may add keys. Only a major
release removes or renames a key, or changes its type or meaning. Read the
JSON in a way that ignores keys you do not know.

**Exit codes.** Each status in the README's [Exit codes](README.md#exit-codes)
table keeps its number, its `status` name and its meaning. A minor release
may give a failure that exited 1 (any other failure) a more specific status,
new or existing, and its release notes say so. Any other change of status is
a major change.

**Commands, flags and environment variables.** The CLI's own commands, their
arguments and flags, and the `FIRMFACT_*` variables. A minor release may add
one. Removing or renaming one, or changing what it does, is a major change;
until then, one that is on its way out keeps working and says on stderr what
replaces it.

**Generated commands and flags.** Which workspace commands there are, their
flags and what they return belong to the service, and change with it rather
than with the CLI's version. The CLI promises how it derives them:

- a tool named `list_<things>` becomes `<things> list`, one named
  `analyze_<name>` becomes `analyze <name>`, and any other tool a command of
  its own name, with underscores turned into hyphens (a tool whose name is
  not plain, or would take the name of a built-in command, gets none, and
  `firmfact tools list` says why);
- each argument of a tool becomes a flag of the same name, hyphenated
  (`contract_id` gives `--contract-id`), and typed by the tool's schema as a
  string, integer, number or boolean, or for an array a list that takes one
  item each time the flag is given; where the items are strings with no
  `enum`, such as `fields`, commas separate items too
  (`--fields name,temporal_id`), and where they have an `enum`, a value
  that is not one of its values but whose comma-separated parts all are is
  those parts (`--statuses active,archived`); otherwise an item may hold a
  comma;
- the flags the CLI adds to workspace commands (`--columns`, `--all`,
  `--wide`, `--yes`, and `--monthly` on an analysis) give way to a tool
  argument of the same name;
- `firmfact call <tool>` runs any tool by its own name, with `--arg key=value`,
  each value read as the type the tool's schema gives the argument, a list's
  items as its flag takes them, or as a JSON array.

Changing any of these rules is a major change, and so is removing one of the
added flags.

**What may change in any release.** Everything written for people rather than
programs: tables and their columns, the wording of messages, notes and hints
on stderr, colours, `--help` and `--debug` output, and the files in the cache
directory. Scripts should read `--json` and the exit status rather than parse
these.

**The service's minimum version.** The service can require a newer CLI than
the one you run, whatever its version number. It tells the CLI the oldest
version it still works with, which the CLI checks once a day, and below it
every command exits with status 6 and says how to upgrade (local and
snapshot builds are exempt). As that stops everyone on an older version at
once, the minimum goes up only when a change to the service would otherwise
break older versions, never to hurry people onto a new release. It goes up
only to a version that has been released, which copes with the change, so
there is always one to upgrade to, and the notes of the next release say so
under Breaking, naming the new minimum.

## Releases

A maintainer releases from `main`, a release candidate first and then the
release itself, as [RELEASING.md](RELEASING.md) describes, along with the
secrets and repository settings a release needs. The version follows the
rules above, and its section of CHANGELOG.md becomes the release's notes on
GitHub, which is why a pull request adds its line under Unreleased.
