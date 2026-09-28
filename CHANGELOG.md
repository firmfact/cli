# Changelog

What changed in each release of the firmfact CLI, newest first. The notes of
a release on GitHub are its section of this file.

A change that users will notice gets a line under Unreleased in the pull
request that makes it, under one of these headings, in this order (a
section leaves out the ones it has nothing for):

- **Breaking**: what a script or a habit has to change for, and what to do
  instead; see
  [Compatibility and versioning](CONTRIBUTING.md#compatibility-and-versioning).
- **Added**: new commands, flags and behaviour.
- **Changed**: what works differently now.
- **Deprecated**: what still works but is on its way out, and what replaces
  it.
- **Removed**: what is gone.
- **Fixed**: bugs.
- **Security**: vulnerabilities.

The workspace commands and their flags come from the firmfact service and
change with it, so they are not listed here.

## Unreleased

## 0.1.0 - 2026-09-28

### Added

- `signup` creates an account from the terminal: your work email, a code
  sent to it, your own workspace, and a Demo workspace of sample data to
  explore while yours is set up. A script can sign up in two runs, with the
  password and the code on standard input.
- `login` signs in through your browser, and `logout` revokes the sign-in.
  Tokens are kept in your operating system's keyring.
- `whoami`, and `workspaces list`, `use` and `status`, with `--wait` to wait
  for a workspace's setup.
- Workspace commands, such as `vendors list` and `analyze cost-trends`, made
  from the tools the firmfact service offers, with a flag for each of their
  arguments, and kept up to date; `call` runs any tool by its name, and
  `tools list` lists them.
- `ask` puts a question to a workspace, and `--continue` follows up in the
  same thread.
- `upload` sends invoices, contracts and other documents to a workspace for
  firmfact to read: files, folders with `--recursive`, patterns, or
  standard input with `--name`. It shows the plan first (what is new, what
  is already there, what firmfact refuses and why, and the allowance left),
  waits until the documents are read, and shows what was read, the
  contract an invoice matches, a preview of how it compares with that
  contract, and what the review page asks. For a spreadsheet or an HR
  file, a line for each type of record says how many rows publishing
  would add, change (and in which fields) or leave as they are, and how
  many a person should match first, as publishing may add those as new
  records; an HR file's line for people counts those it no longer lists,
  whom publishing may mark as leavers. Nothing is booked until someone
  publishes it there.
  Files already in the workspace are not sent again;
  `--related` sends files as one group, counted once; an upload to the
  Demo workspace that nobody named asks first, or needs `--yes`. A file of
  a type firmfact does not read in a folder or a pattern, and a link that
  leads out of a folder, is left out before it is read, so firmfact is
  never told of it.
- `upload status` lists your recent uploads, or shows one in full, and
  `--wait` waits until it is read.
- At a terminal, an upload whose invoice shows a variance ends with the
  commands that follow it up: `open` with its review link,
  `contract-items list` for the contract item of the line that differs
  most, and `analyze cost-trends` for the vendor's costs month by month.
  With `--fail-on-variance`, they follow the first invoice over the
  threshold.
- `--fail-on-variance` on `upload` and `upload status`, for pipelines: once
  firmfact has read the documents, the command exits with status 9
  (`variance_exceeded`) when an invoice is further from its contract than
  the threshold allows, above or below it. The threshold is a percentage
  of the contracted amount (`--fail-on-variance=2%`) or an amount in the
  invoice's currency (`--fail-on-variance=50`), and holds the net
  difference; without one, any variance counts, lines that differ from the
  contract but add up to it included. With `--json`,
  `meta.variance_exceeded` lists those invoices.
  Documents that are not invoices, and invoices with no contract to
  compare them with, never trip it; a document whose variance cannot be
  checked, such as an invoice whose lines are not matched to its contract
  yet, makes the exit status 1, and is listed in
  `meta.variance_unchecked`.
- A command that writes to a workspace says so in its help, and one that may
  delete or overwrite data asks first, or takes `--yes`.
- Output for people and for scripts: tables that fit the terminal,
  `--format table|json|csv|tsv`, `--columns`, `--all` for every page,
  `--json` with the same three keys from every workspace command, and
  `--jq` to filter the JSON with a jq expression, no jq needed. CSV and TSV
  put a quote in front of text a spreadsheet would run as a formula.
- A list prints under a title that names its workspace and says when the
  data is sample data, with the columns the service chooses under its
  labels, in the language you chose in firmfact. Amounts are grouped and
  in the workspace's base currency, which the title line names, footnotes
  say what each covers, and a line under the table counts the list. A Demo
  workspace's data gets one quiet line on stderr where the service's notice
  for an assistant was. `--wide` adds the fields the table leaves out, such
  as IDs; CSV, TSV and `--json` keep every field under its own name. From a
  service that does not describe its lists yet, a table has headers in
  words and footnotes that say its costs are in the workspace's base
  currency.
- An exit status for each kind of failure, and errors as JSON on stderr with
  `--json`.
- Profiles for more than one account or host, and `FIRMFACT_HOST`,
  `FIRMFACT_PROFILE`, `FIRMFACT_WORKSPACE` and `FIRMFACT_TOKEN` for scripts
  and CI.
- A daily check for a newer release and for the oldest version the service
  supports, which `FIRMFACT_NO_UPDATE_CHECK=1` turns off. `update` replaces
  a downloaded binary once the release's signature and checksum check out
  and the new binary runs.
- `update --pre` takes the newest release, pre-releases included, and
  `update --version` installs the release it names, both with the same
  checks; going back to an older release asks first, or needs `--yes`. On
  a pre-release, the daily check tells you of newer pre-releases too. For a
  copy that Homebrew or Scoop installed, `update` says how that package
  manager keeps it at a version, where it can.
- `doctor` checks the installation, the proxy, the connection and the
  sign-in; `version` shows the build; `--debug` (or `FIRMFACT_DEBUG=1`) logs
  every request, with tokens, codes and passwords redacted.
- `claim` makes `ff`, or another short name, run the CLI, and `claim --undo`
  takes it back.
- `open` opens firmfact in your browser, on the host you use, or the page
  of it that a link names, such as the review of a document you uploaded.
- Tab completion for bash, zsh, fish and PowerShell, and man pages.
- Releases for Linux, macOS and Windows on amd64 and arm64: through Homebrew
  (`brew install firmfact/tap/firmfact`), Scoop, the one-line installers on
  firmfact.com/cli and `go install github.com/firmfact/cli/cmd/firmfact`, or
  as signed, reproducible archives with build provenance.

### Fixed

- While only pre-releases were published, `update` said it could not find
  the latest release and to check your connection. It now says that no
  release is published yet and, when there is a newer pre-release than
  yours, how to take it, and speaks of the connection only when GitHub
  cannot be reached.
