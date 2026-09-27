# Firmfact CLI

**Your firm's single source of fact, from the command line.**

Firmfact is licence management for the complex software and data subscriptions
that need expert interpretation. It reads the documents behind every licence,
from the vendor and from inside the firm, turns them into the facts that
explain the why, and tells the people using those licences what they should
know. Market data teams at banks, asset managers and other regulated firms use
it for contracts, invoices, allocations and cost visibility, with the full
history of what changed and when.

This CLI brings firmfact to your terminal. Create an account, sign in and ask
your workspaces questions, from a prompt or from a script.

> **Status: pre-release.** The first binaries appear under
> [Releases](https://github.com/firmfact/cli/releases) shortly. Until
> then you can install it with Go (see below).

## Try it in two minutes

```bash
firmfact signup
```

Signup asks for your work email, name and organisation, emails you a 6-digit
code, and creates two workspaces:

- **your own workspace**, for real data;
- **a Demo workspace** with clearly marked sample data, ready to explore while
  the CLI shows its progress.

Signing up is free, with no trial clock. The CLI works with the Demo workspace
on every plan; reaching your own workspace's data from the CLI (as over MCP)
needs Professional or above. Pricing is published in full on
[firmfact.com/pricing](https://firmfact.com/pricing).

Then:

```bash
firmfact vendors list                               # who you pay
firmfact analyze cost-trends --entity-type vendor   # the spend by vendor, and how it moves
firmfact ask "Which contracts renew in the next 90 days?"
firmfact ask --continue "Which of those are with LSEG?"
```

After each command the CLI suggests the next step worth taking.

## Install

The same commands, and how to check a download by hand, are on
[firmfact.com/cli](https://firmfact.com/cli).

**macOS**

```bash
brew install firmfact/tap/firmfact
```

**Windows**

```powershell
scoop bucket add firmfact https://github.com/firmfact/scoop-bucket
scoop install firmfact
```

The CLI is not in winget yet. Once its first release there has been
accepted, `winget install --exact --id Firmfact.CLI` installs it too: by its
identifier, as a bare name can match another publisher's package.

**Linux**

```bash
curl -fsSL https://firmfact.com/install.sh | sh
```

The same line installs on a Mac without Homebrew. On Windows without Scoop:

```powershell
irm https://firmfact.com/install.ps1 | iex
```

The one-line installers verify a release before they install anything: its
`checksums.txt` must carry a valid signature by a firmfact release key (the
keys `firmfact update` trusts), and the archive must match its checksum there.
They install into your home directory (`~/.local/bin`, or
`%LOCALAPPDATA%\Programs\firmfact\bin` on Windows) without sudo or
administrator rights, and each is a single script you can read before you run
it.

**With Go** (1.27 or newer):

```bash
go install github.com/firmfact/cli/cmd/firmfact@latest
```

**By hand:** download the archive for your system from
[Releases](https://github.com/firmfact/cli/releases), check with the
[GitHub CLI](https://cli.github.com/) that our release workflow built it from
the release's own tag, and put `firmfact` on your `PATH`:

```bash
gh attestation verify firmfact_<version>_linux_amd64.tar.gz --repo firmfact/cli \
  --cert-identity https://github.com/firmfact/cli/.github/workflows/release.yml@refs/tags/v<version>
```

A match against `checksums.txt` alone proves little, as that file comes from
the same place as the archive; its signature, which the one-line installers
check, or the attestation above is what ties it to us. The archives are also
reproducible: building the release's tag with exactly the Go version in
`go.mod` and the GoReleaser version in `.github/workflows/release.yml` gives
the same checksum for every archive (not for the SBOMs, as each records when
it was made):

```bash
git clone --branch v<version> https://github.com/firmfact/cli firmfact-cli
cd firmfact-cli
go run ./internal/gendocs   # the completions and man pages the archives carry
goreleaser release --clean --skip=publish,sign,sbom
cat dist/checksums.txt
```

**Tab completion and man pages.** Homebrew installs both: completion for
bash, zsh and fish, and `man firmfact`, with a page for each command, such as
`man firmfact-workspaces-use`. Scoop prints the line that loads the
PowerShell completion from your profile. The archives carry them in
`completions/` (`firmfact.bash`, `_firmfact` for zsh, `firmfact.fish` and
`firmfact.ps1`) and `manpages/`, whose pages go in a `man1` directory on your
`MANPATH`, such as `~/.local/share/man/man1`. Otherwise
`firmfact completion <shell>` prints the script for your shell, and its
`--help` says where to put it.

Tab completes commands and flags, the values a workspace command's flag
takes (`firmfact analyze cost-trends --entity-type <Tab>`), profile names, and
workspace names for `--workspace`, `workspaces use`, `workspaces status` and
`config set workspace`. The workspace names are the ones your last `login`,
`whoami` or `workspaces list` saw, so a Tab never waits for the network;
`logout` forgets them.

Want it shorter? `firmfact claim` makes `ff` (fast forward, and our initials)
run the CLI:

```bash
firmfact claim           # or: firmfact claim <another-name>
ff vendors list
```

It links `ff` in `~/.local/bin` (a `ff.cmd` shim on Windows). If your shell
already has an `ff` alias or function, as many fzf setups and Omarchy do, it
shows a small marked block for your shell's rc file and changes nothing until
you confirm. After that, `ff` with arguments runs firmfact (`ff vendors list`),
while `ff` on its own, and helpers that use it in a pipe or `$(...)`, keep
running your previous `ff`, which is also available as `ff-previous`. If
firmfact is uninstalled, the block steps aside. `firmfact claim --undo`
removes exactly what it added. An rc file that is a link, as stow and chezmoi
make them, stays a link and the file it points at gets the block. One owned by
another user or by a group you are not in, such as a home-manager file in the
Nix store, is left alone, and claim shows the block for you to add yourself.
The block for bash goes into `~/.bashrc`, which bash on macOS does not read
as a login shell unless `~/.bash_profile` loads it; claim says so when yours
does not.

## Commands

| Command | What it does |
|---|---|
| `signup` | Create an account (work email, emailed code, workspace choices) |
| `login` / `logout` | Sign in through your browser; sign out and revoke the token |
| `whoami` | Who you are signed in as |
| `workspaces list` / `use` / `status` | The workspaces you can reach and how far each one's setup is; pick the default; check on a setup, or `--wait` for it |
| `vendors list`, `contracts list`, … | Look things up in a workspace |
| `analyze cost-trends`, `analyze allocations`, `analyze utilization` | Analyse spend, allocations and utilisation |
| `ask "question"` | Ask a question in plain English; `--continue` follows up in the same thread |
| `upload <files>` / `upload status` | Upload invoices, contracts and other documents for firmfact to read, and see what it read (see [Uploading documents](#uploading-documents)) |
| `chat-with-workspace` | The same, as a workspace command with `--message` and `--thread-id`; kept for scripts that use it |
| `call <tool>` | Call any workspace tool by name |
| `open [link]` | Open firmfact in your browser, on the host you use, or the page of it a link names, such as a document's review |
| `doctor` | Check the installation, proxy, connection and sign-in |
| `version` | The version, the commit it was built from, the platform, how it was installed and the latest release known |
| `update` | Update to the latest release; `--pre` takes pre-releases too, `--version` the release you name |
| `claim [name]` | Make `ff` (or another short name) run this CLI; `--undo` reverses it |

The workspace commands come from the firmfact service itself, so new ones
appear without a new CLI release (`firmfact tools refresh`). So do their
flags: `--help` on a command lists them, with the values each one accepts
and which ones are required. A flag that takes a list, such as `--fields`,
takes an item each time it is given, and commas separate items as well, so
`--fields name,temporal_id` asks for two fields; where the flag has fixed
values, one of them that has a comma of its own stays whole. `call` reads a
list's `--arg` the same way. To send any other item with a comma in it, give
`call` the list as JSON: `firmfact call <tool> --arg '<argument>=["Acme, Inc."]'`.

The list of workspace commands is kept on your machine. `login` fetches it;
on a terminal, a workspace command fetches it again alongside its own work
once it is a day old; and a command the service no longer knows, or knows
with other flags, fetches it and tries once more. Scripts get no other
refresh, so a CI job that signs in with `FIRMFACT_TOKEN` should run
`firmfact tools refresh` first, or use `firmfact call <tool>`, which fetches
the list itself when the tool is not in it. A tool whose name is not plain
lower-case letters, digits and underscores, or that would take the name of
one of the CLI's own commands (such as `help`), gets no command;
`firmfact tools list` shows which and why, and `call` still runs it.

Each tool comes with the service's word on what it does to your workspace.
`--help` and `firmfact tools list` mark a command that writes to it, such as
`chat-with-workspace`, which saves its thread. A command that may delete or
overwrite data asks before it runs, and `--yes` (`-y`) goes ahead without
asking. Off a terminal, as in a script, there is no one to ask: without
`--yes` such a command fails with exit status 2 before anything is sent.
`call` asks the same way and takes `--yes` too. A tool the service says
nothing about counts as one that may delete data, and `tools list --json`
gives each tool's `writes` and `destructive` as the CLI reads them.

Every command takes `--workspace` (or `FIRMFACT_WORKSPACE`) to target a
workspace by name or id, and `--json` for scripts: the result goes to stdout
as JSON, while progress, notes and the sign-in link go to stderr. `signup` and `claim`, which talk you through
their steps, print text even with `--json`; their errors still come as JSON.
Each exit status means one kind of failure (see [Exit codes](#exit-codes)).

When firmfact cannot be reached, the error says why in one line (the name
does not resolve, the connection was refused, no answer in time, a
certificate that does not verify) and points at `firmfact doctor`, which
checks the connection, your clock and your sign-in.

A busy service is waited out. When it answers that it is rate-limited or at
capacity (429 or 503) and says to come back within 30 seconds, the CLI waits
and tries again, twice at most; on a terminal, stderr says so (`Server busy;
retrying in 5s...`). A workspace command the service marks as read-only is
also sent once more after a proxy error (502, 504) or a dropped connection.
A chat is not, as the first attempt may have run.

`firmfact ask` puts a question to a workspace. The question is one argument,
in quotes; `-` reads it from standard input instead, so a longer question can
come from a file or a pipe. The service reads the first 10,000 characters of
a question, and the CLI says so when one is longer.

```bash
firmfact ask "Which contracts renew in the next 90 days?"
firmfact ask --continue "Which of those are with LSEG?"
firmfact ask - < question.txt
```

A chat can take a minute or more: the service streams its answer, and the
CLI waits up to five minutes for it. On a terminal, stderr counts the
seconds meanwhile (`Thinking... 12s`). The answer prints as text, markdown
and all, and on a terminal it is followed by the command that asks a
follow-up in the same thread.

Each answer belongs to a chat thread. `--continue` (`-c`) follows up in the
thread of your last `ask` on the same host and workspace, which the CLI keeps
in its cache directory until `logout`; `--thread <id>` follows up in the
thread with that id. A thread the service cannot find in the workspace gets
a new one, which the CLI says on stderr. With `--json`, the answer's thread
is `data.thread_id`. `chat-with-workspace --message "..."` asks the same way,
and stays for the scripts that use it.

Without `--json`, a list prints as a table, and an analysis prints its
totals in the workspace's currency, the period it covers and its insights
above its table. On `analyze cost-trends`, `--monthly` adds the
month-by-month totals, marking the current month and the forecast.

A list's table has a title that names the workspace and says when its data
is sample data, and the currency its amounts are in:

```console
$ firmfact vendors list
Vendors in Demo (sample data)                                costs in EUR

NAME                           CODE             13-MONTH COST  THIS MONTH
Bloomberg Finance L.P.         BLOOMBERG         1,262,160.00   97,089.23
CryptoCompare Limited          CRYPTOCOMPARE        18,000.00    1,384.62
Deutsche Börse AG              DEUTSCHE_BOERSE
FactSet Research Systems Inc.  FACTSET             412,500.00   31,730.77
ICE Data Services              ICE_DATA             96,840.50    7,449.27
LSEG Data & Analytics          LSEG                853,220.18   65,632.32
37 vendors (page 1 of 7; use --page 2 or --all).

13-month cost: cash basis, this month and six months either side.
This month: accrual basis.
Amounts in EUR, the workspace base currency.
Sample data in the Demo workspace, not your own spend.
```

The service chooses the columns and names them in the language you chose in
firmfact. A list's amounts are in the workspace's base currency, whatever
currency a vendor invoices in, and the footnotes say what each one covers.
An empty cell has no value, such as a vendor with no costs in the period.
The rows come in the service's order, by name unless the list takes
`--sort` and you give it (`--sort cost:desc`). The count, the next page and
the sample-data line go to stderr, so a pipe or a file gets the table and
its footnotes alone.

`--wide` adds the fields the table leaves out, such as each record's ID and
the currency it invoices in, and `--columns` picks fields by the names
`--json` and CSV use (`--columns name,currency_userdef_id`). A service that
does not describe its lists yet gets every field but the internal IDs, with
headers in words, and no title.

An analysis's table starts with the columns you look for first (name, id,
userdef_id, status, cost, monthly_cost, currency) and then shows every
other field. Numbers are aligned right and amounts of money grouped to the
cent. On a terminal a table fits the window: long text is cut with an
ellipsis, and columns that still do not fit are left out, with a note on
stderr saying which. `--wide` prints every cell whole, as does output to a
pipe or file.

A workspace command and `call` also take:

| Flag | Meaning |
|---|---|
| `--format table\|json\|csv\|tsv` | how to print the answer; `--json` is `--format json` |
| `--columns name,cost` | the columns to print, in that order; where the tool takes `fields`, it is also sent as `fields`, so the service sends only those |
| `--all` | fetch every page, 200 rows at a time, and print them as one list; with `--json`, one JSON row per line (NDJSON), each page as it arrives |
| `--wide` | every field of a list, and every cell whole: the table is not fitted to the terminal |

CSV and TSV print a list's rows with a header line, and the values as the
service sent them (no grouping or rounding). CSV quotes a value with a comma,
quote or line break in it, and an empty value when there is one column, so
that its row is not an empty line; TSV writes a tab, line break or backslash
in a value as `\t`, `\n` or `\\`. An answer without rows, such as a chat, cannot
be printed as CSV or TSV. A list with more pages ends with a line on stderr
saying how to get the next one (`--page 2`, or `--arg page=2` under `call`)
or all of them (`--all`). Each page `--all` fetches is waited out and retried
like any other call.

CSV and TSV are made to be opened in a spreadsheet, which runs a value that
starts with `=`, `+`, `-` or `@` as a formula, and a vendor or invoice name
can come from anyone in the workspace or from imported data. So, as
[OWASP advises](https://owasp.org/www-community/attacks/CSV_Injection), a
value or column name that starts with one of those, their full-width forms,
a tab or a line break is written with a single quote in front
(`'=HYPERLINK(...)`), and the spreadsheet shows it as text. A number keeps
its sign, `-1200.50` sent as text included. Only `--format csv` and
`--format tsv` do this: `--json` and `--jq` give every value exactly as the
service sent it.

```bash
firmfact contract-items list --all --format csv --columns name,userdef_id,monthly_cost > items.csv
firmfact vendors list --all --json | jq -r .name
```

With `--json`, a workspace command always prints the same three keys,
whichever workspace it runs against (with `--all`, the rows alone, one per
line):

```json
{
  "data": [ { "name": "Acme", "temporal_id": "..." } ],
  "meta": { "page": 1, "total_pages": 2, "total_count": 3, "truncated": true },
  "notes": [ "Showing 2 of 3 records. ..." ]
}
```

`data` holds the rows of a list (an empty list is `[]`) or the answer of an
analysis or chat, every field under its own name. `meta` holds the paging,
`workspace_data_source` when the data is Demo sample data and, for a list,
`display`: the title, columns, labels and footnotes its table is made from
(schema `list_display/1`). `notes` holds anything the service said
alongside, such as its Demo notice for an assistant; stderr has one line
for people instead.

`--jq` runs the JSON through a jq expression before it prints, as the GitHub
CLI's `--jq` does, without jq installed. It implies `--json` and works on
every command that prints JSON. Each value the expression gives goes on a
line of its own: a string as its text, anything else as JSON, on one line
(indented on a terminal). With `--all`, the expression runs on each row. A
mistake in the expression stops the command before anything is sent, with
exit status 2.

```bash
firmfact vendors list --jq '.data[].name'
firmfact contract-items list --all --jq '[.name, .monthly_cost] | @tsv'
firmfact doctor --jq '.checks[] | select(.ok | not) | .detail'
```

### Uploading documents

`firmfact upload` sends invoices, contracts, order forms and other documents
to a workspace for firmfact to read, as the upload page in the web app
does, and shows what it read: the vendor, the amounts, the contract an
invoice matches, a preview of how it compares with that contract, and what
needs a person on the document's review page. Nothing is booked until
someone publishes the document there.

```bash
firmfact upload LSEG-2026-09.pdf --workspace Acme
firmfact upload ~/Invoices/2026-09 --recursive --workspace Acme
firmfact upload invoice.pdf usage-report.xlsx --related --workspace Acme
scanimage --format=pdf | firmfact upload - --name scan-0034.pdf --workspace Acme
```

```text
Uploading to Acme: 1 new
Allowance: 21 of 25 documents left this month (it resets on 1 Oct 2026).

LSEG-2026-09.pdf: invoice, ready for review
  Vendor      Refinitiv Limited, linked to LSEG
  Invoice     INV-8841207, 1 Sep 2026, due 1 Oct 2026; EUR 12,450.00
  Contract    LSEG Workspace 2026 (C-0042), linked automatically (99%)
  Variance    EUR 1,550.00 (14.2%) above the contract (preview)
     1  Unit price 1,150.00 against 1,030.00 (+11.7%): +1,200.00
     3  Not in the contract: +350.00
  To review   Line 3 (Exchange fees): choose a contract item or skip it.
  Review      https://firmfact.com/accounts/.../documents/...
Nothing is booked until someone publishes it there.

Next steps for LSEG-2026-09.pdf, EUR 1,550.00 (14.2%) above the contract:
  firmfact open https://firmfact.com/accounts/.../documents/...  (go through the variance on its review page)
  firmfact contract-items list --query "Workspace Pro Licence" --workspace Acme  (the contract item line 1 is compared with)
  firmfact analyze cost-trends --entity-type vendor --entity-name LSEG --monthly --workspace Acme  (the vendor's costs, month by month)
```

At a terminal, an invoice whose preview shows a variance ends the results
with the commands that follow it up, as above: its review page, the
contract item of the line that differs most, and how the vendor's costs
moved month by month. A batch gets them for its first invoice with a
variance, and a count of the others; with `--fail-on-variance`, for its
first invoice over the threshold, when there is one. Scripts and `--json`
get none.

A spreadsheet or an HR file has a line for each type of record in it: how
many rows of that type firmfact read, how many of them publishing would
add, change (with the fields that differ, the most frequent first) or leave
as they are, and how many are unmatched: rows that resemble a record
already in the workspace, for a person to match on the review page.
Publishing does not wait for that, and may add them as new records,
possible duplicates, so a block with unmatched rows says so. Rows set aside
on the review page, rows that match records you cannot view, and rows past
those the review page shows (which publishing imports as they are) are
counted too, when there are any, and so are the people in the workspace
whom an HR file no longer lists. The type names are in your language, as
firmfact shows them.

```text
HR-2026-09.xlsx: HR file, ready for review
  Departments    12 read: 1 new, 1 with changes (Parent unit), 10 unchanged
  Cost centres    8 read: 8 unchanged
  People        250 read: 230 new, 15 with changes (Department, Cost centre), 5 unmatched; 3 no longer in the file
  Unmatched   Publishing may add unmatched rows as new records, possible
              duplicates, unless someone matches them first.
  To review   An item (Jane Smith): choose which changes to apply.
              An item (Robret Brown): confirm the suggested match or choose another.
              and 6 more on the review page
  Review      https://firmfact.com/accounts/.../documents/...
```

Name files, folders with `--recursive`, or patterns such as `'*.pdf'`,
which the CLI expands where the shell did not (Windows shells expand none,
and there a pattern ignores case, so `*.pdf` finds `SCAN001.PDF`). Hidden
files and folders, and the `~$` lock files Office keeps beside an open
document, are left out of folders and patterns, and so is a file there of
a type firmfact does not read, while such a file named on its own is
refused. A link in a folder counts only when it leads to a file in that
folder. `-` reads one file from standard input, and `--name` gives its
name, extension and all: firmfact tells a file's type by its extension, and
refuses one whose contents do not match it. Firmfact reads PDFs, PNG, JPEG,
GIF and WebP images, Word (`.docx`) and Excel (`.xlsx`, `.xls`) files, CSV,
TSV, text, email (`.eml`) and ZIP files, each under 50 MB.

Before it sends anything, the CLI asks firmfact what it would do with the
files, and prints that plan: which are new, which are already in the
workspace, which it refuses and why, the workspace they go to and how much
of this month's allowance of documents is left. It then sends the new
files one at a time, and waits until firmfact has read them, for 15 minutes
at most (`--wait-timeout`); on a terminal, a line says how far it has got.
`--no-wait` stops once they are sent. A file already in the workspace is
not sent again, so running the same command over a folder again is safe,
and shows what firmfact read from it before; `--new-version` sends it all
the same, as a new version. A file that changes while it is being sent is
not stored. Should the documents be stored but reading back what firmfact
made of them fail, the results show what is known, and the error says the
`firmfact upload status` command that shows the rest.

Each new file counts as a document of the monthly allowance. `--related`
sends the files as one group of related documents, such as an invoice and
its usage report, which counts once: at most 10 files and 100 MB together,
and a file of the group that firmfact refuses keeps the others back too.
Firmfact also recognises some sets of reports by their names, such as a
Bloomberg SID set, which goes together, and the plan says so.

A new sign-up's default workspace is Demo, which is rebuilt from sample data
from time to time, and a rebuild removes what you upload there. So when
neither `--workspace` nor `FIRMFACT_WORKSPACE` names the workspace and the
upload would go to Demo, the CLI asks first. Off a terminal, and with
`--json`, it exits with status 2 before reading any file, unless `--yes`
says to go ahead.

With more than three documents, the results are a table of them and a line
that sums them up. `firmfact upload status` lists your recent uploads with
their ids, and `firmfact upload status <id>` shows one in full; `--wait`
waits until firmfact has read it.

With `--json`, `data.results` holds a result for each file, in the order of
the command line: its `path`, `filename`, `size` and `sha256`, the
`outcome` (`created`, `duplicate`, `in_progress`, `busy`, `refused`,
`skipped`, `not_sent` or `failed`), a `code` and `message` saying why when
it was not stored, and the `document` as firmfact describes it, in the
schema `meta.schema` names (`document_result/1`). A file the CLI left out
or refused without reading it has no `sha256`. `data.summary` counts the
files by outcome and the documents by state, and `data.allowance` is the
monthly allowance as it was before the upload. A spreadsheet's or an HR
file's `document.read.records` has an entry for each type of record, with
a `type` that stays the same in every language (such as `person`,
`cost_centre` or `product`), its `label`, the `total` and each count.

```bash
firmfact upload ~/Invoices/2026-09 -r --workspace Acme --jq '.data.results[] | [.path, .outcome, .document.state] | @tsv'
firmfact upload HR-2026-09.xlsx --workspace Acme --jq '.data.results[].document.read.records[]? | [.type, .total, .new, .changed] | @tsv'
```

In a pipeline, `--fail-on-variance` holds each invoice to its contract
once firmfact has read it: the upload ends with exit status 9 when an
invoice's variance preview is further from the contract than the threshold
allows, above or below it. The threshold follows an `=`: a percentage of
the contracted amount (`--fail-on-variance=2%`) or an amount in the
invoice's currency (`--fail-on-variance=50`). An invoice exactly at it
passes, and without a value, any variance of a cent or more counts.
Documents that are not invoices, and invoices firmfact did not match to a
contract, never exceed it. A document whose variance cannot be checked
(someone else's upload, whose results only they see, or an invoice whose
contract you may not view or whose preview firmfact could not work out)
makes the exit status 1, as a document that could not be read does, and a
wait that ran out makes it 5: so 9 means that everything else went well,
and the error names the invoices over the threshold whatever the status.
With `--json`, `meta.variance_exceeded` lists the files over it, by their
`path`, and `meta.variance_unchecked` those whose variance could not be
checked. The check needs what firmfact read, so it cannot go with
`--no-wait`; `firmfact upload status <id>... --fail-on-variance` checks
documents uploaded before, waiting for them as `--wait` does, and lists
them by id.

```bash
status=0
firmfact upload "$INVOICES" -r --workspace Acme --fail-on-variance=2% --jq '.meta.variance_exceeded[]' > over.txt || status=$?
case $status in
  0) echo "Every invoice is within 2% of its contract." ;;
  9) echo "More than 2% from the contract, to review before they are booked:"; cat over.txt; exit 1 ;;
  *) exit "$status" ;;
esac
```

The exit status says how it went: 0 when every file was uploaded or was
already there, and was read (with `--no-wait`, sent); 1 when a file was
refused, or a document could not be read or was skipped for the allowance;
2 for a mistake on the command line, such as a pattern or folders with no
files to upload, or an unnamed Demo workspace off a terminal; 3 when not
signed in; 4 when the workspace does not exist; 5 when the wait ran out, or
firmfact was busy or rate-limited, which a later run picks up; 6 when the
host does not offer uploads yet; 9 when `--fail-on-variance` finds an
invoice over its threshold. When files ended in more than one of these
ways, 1 wins over 5, and any of them over 9. With `--json`, the results
are printed whatever the exit status once anything was sent, Ctrl-C
included.

### Exit codes

A command exits with a status that says what kind of failure stopped it, so
a script or CI job can tell a typo from an ended session from a service that
is down:

| Status | `status` | Meaning |
|---|---|---|
| 0 | | success |
| 1 | `failed` | any other failure, such as an error the workspace tool reported, a failed `doctor` check, or a file an upload refused or a document firmfact could not read |
| 2 | `usage` | the command line is wrong: an unknown command or flag, a missing argument or required flag. A typo gets a suggestion, and a command group such as `vendors` or `config` refuses a subcommand it does not have. Also a command that may delete or overwrite data, run off a terminal without `--yes`, an upload to a Demo workspace that nobody named, off a terminal, and `update --version` to an older release off a terminal without `--yes` |
| 3 | `not_signed_in` | not signed in, or the session has ended; run `firmfact login` |
| 4 | `not_found` | no such workspace, profile, tool or record, or no such release for `update --version` |
| 5 | `unavailable` | rate-limited, the service failing, or no answer at all, or a wait for a workspace's setup or for uploaded documents to be read that ran out of time; worth retrying later |
| 6 | `unsupported` | the host cannot serve this CLI: an older firmfact, another service, or a CLI below the host's minimum version |
| 9 | `variance_exceeded` | not a failure but a finding, for pipelines: `upload` or `upload status` with `--fail-on-variance` read every document, and an invoice's variance preview is over the threshold (see [Uploading documents](#uploading-documents)) |
| 130 | `interrupted` | Ctrl-C or SIGTERM |

Statuses 7 and 8 are reserved for kinds of failure that are 1 for now.

Without `--json`, an error is one line on stderr starting with `error:`. With
`--json`, it is one line of JSON on stderr instead, with the exit status as
`code` and the name from the table as `status`:

```json
{"error":{"message":"unknown command \"lst\" for \"firmfact vendors\"; did you mean `firmfact vendors list`?","code":2,"status":"usage"}}
```

An error the server did not explain, such as a 5xx or an answer the CLI did
not expect, ends with the id the server logged the request under, as in
`(request id: 68c793ba-2e9d-471c-abfa-f138e91be2f6)`. Quote it when you report
the problem; `--debug` shows the id of every request.

### Scripted signup

A script signs up in two runs. The first sends the details, has the code
emailed and exits 0; the second confirms the code, and needs only the email
address with it. It sends no second email, so it does not count against the
limit on codes per address.

```bash
firmfact signup --email jan@yourfirm.example --name "Jan de Vries" --org "Your Firm BV" \
  --type financial_services --currency EUR --language en --no-password --accept-terms
# then, with the code from the email:
FIRMFACT_SIGNUP_CODE=123456 firmfact signup --email jan@yourfirm.example
```

The second run signs in and waits for the Demo workspace, for up to
`--wait-timeout` (20 minutes). A check the server cannot answer for a moment
is tried again rather than ending the wait. After `--no-wait`, Ctrl-C or the
timeout, the setup continues on the server; `firmfact workspaces status`
says how far it is, and `--wait` picks the wait up again:

```bash
firmfact workspaces status --wait --json   # exits 0 once the Demo workspace is ready
```

Keep the password and the code out of the arguments: other users of the
machine can read those in the process list, and they end up in shell history
and CI logs. To set a password for the web sign-in as well, pipe it in instead
of passing `--no-password`; `--password-stdin` reads one line from standard
input, as `docker login` does:

```bash
firmfact signup ...other flags... --password-stdin < password.txt
```

`--code-stdin` reads the code the same way, for when `FIRMFACT_SIGNUP_CODE`
does not suit.

## Staying up to date

Once a day the CLI checks for a newer release and for the oldest version the
service supports. The check runs in the background while your command does, so
it never slows a command down, and what it finds applies from the next
command. On a terminal, a newer release gets one line telling you how to
upgrade (`brew upgrade firmfact`, `scoop update firmfact`,
`winget upgrade --exact --id Firmfact.CLI`, or `firmfact update` for a direct
download, which checks the release's signature and checksum, and that the new
binary runs and reports the right version, before replacing itself; should
the swap fail, the old binary stays). If the service no longer
supports your version, the CLI says so instead of failing in odd ways. On a
pre-release, such as a release candidate, you are offered newer
pre-releases and the release they lead up to; a release is never offered a
pre-release.

In CI (when `CI` is set) or when output goes to a pipe or a file, nobody would
see that line, so the CLI does not ask GitHub for a newer release; it still
checks the service's minimum version, and the first command that day on a
terminal still asks GitHub. Local and snapshot builds (versions such
as `0.3.1-dev+b005a6f`, or what `go build` gives a checkout) skip the check;
`go install ...@<version>` builds report that version and take part. Set
`FIRMFACT_NO_UPDATE_CHECK=1` to turn the check off; `doctor` then leaves
GitHub out too.

`firmfact update` takes the latest release, which is never a pre-release.
`firmfact update --pre` takes the newest release, pre-releases included,
and `firmfact update --version 0.2.0` (or `v0.2.0`) installs exactly that
release. Each is held to the same checks of signature, checksum and trial
run, and a release older than the one you have is installed only once you
confirm, or with `--yes` off a terminal. While only pre-releases have been
published, `firmfact update` says so and how to take the newest one:

```bash
firmfact update --pre                    # the newest release, pre-releases included
firmfact update --version 0.1.0 --yes    # back to an older release, in a script
```

Homebrew, Scoop and winget have releases only, never pre-releases, and
upgrade the copy they installed themselves: for such a copy, `update` names
the package manager's upgrade command, and with `--pre` or `--version` what
it can do instead. Scoop keeps a version with `scoop hold firmfact`; winget
installs a given release with
`winget install --exact --id Firmfact.CLI --version 0.2.0` and keeps it with
`winget pin add --exact --id Firmfact.CLI`; a Homebrew cask cannot be held.

`firmfact version` shows the version you have, the commit it was built from
and the Go version it was built with, how it was installed and the latest
release the check has found, without asking any server (`--json` for
scripts). A bug report needs its output.

## Security and privacy

- No telemetry: the CLI reports nothing about how you use it, what goes
  wrong or your machine. It connects to two places only:
  - your firmfact host (`https://firmfact.com` unless you chose another),
    for what you ask of it, and once a day for the oldest CLI version the
    host supports;
  - `github.com`, for the latest release (on a pre-release, its feed of
    releases, which lists pre-releases too): once a day on a terminal
    (never in CI; see [Staying up to date](#staying-up-to-date)) and each
    time you run `doctor`. `firmfact update`, when you run it, downloads
    the release from there, and from the host GitHub sends the download
    to. None of this goes through GitHub's API, which allows 60 requests
    an hour per address.

  `FIRMFACT_NO_UPDATE_CHECK=1` turns the daily check off and keeps `doctor`
  off GitHub, so that only `update` goes there. Each request names the
  CLI's version and the commit it was built from in its `User-Agent`
  header, so that the host's logs lead to the code that sent it.
- Behind a proxy, set `HTTPS_PROXY` (`HTTP_PROXY` for a plain http host),
  and `NO_PROXY` for the hosts to reach directly. Every request goes
  through it, GitHub's too, except one to this machine (`localhost`).
  `firmfact doctor` shows the proxy in use, and fails when the variable
  holds something that is not a proxy's address, which would otherwise be
  ignored without a word.
- The CLI trusts the certificate authorities your system trusts. On macOS
  and Windows that is the system's certificate store, where a firm's own
  authority for inspecting TLS traffic is normally installed already. On
  Linux it is the system's certificate bundle and directory;
  `SSL_CERT_FILE` and `SSL_CERT_DIR` name others to use in their place, as
  they do for OpenSSL.
- `login` uses OAuth 2 authorisation code with PKCE and a loopback redirect;
  your password never passes through the CLI.
- `firmfact upload` sends only the files you name, and those in the folders
  you name with `--recursive`, and prints its plan before it sends any. The
  plan's request carries the name, size and SHA-256 checksum of each file,
  but a file a folder or a pattern found is not read, and not named to the
  service, when firmfact does not read its type, nor is any file larger
  than firmfact takes. A link in a folder that leads out of it is left
  out, and each file must still be the one that was read when it is sent.
  A file read from standard input is copied to the system's temporary
  directory while it is sent, and removed after.
- `signup` takes a password only at its prompt, without echo, or on standard
  input with `--password-stdin`, never as an argument, where other users of
  the machine could read it.
- Tokens are kept in your operating system's keyring. Where there is none,
  they go to `credentials.json` in the config directory instead, and the CLI
  says so; `firmfact doctor` shows which store is in use. A keyring that is
  there but locked holds the CLI until you unlock it: at a terminal the CLI
  says so after 5 seconds and waits up to 2 minutes more, and a script waits
  5 seconds. A keyring that does not answer in time is not taken for a
  sign-out: the command fails with exit status 5 and says so, and your
  sign-in is not moved to a file. On Linux and macOS the CLI refuses a
  `credentials.json` that is a symbolic link or that other users can read,
  and a config directory other users can change, and says how to fix it.
- The access token expires after an hour and renews automatically. Commands
  running at the same time renew it once between them, as each renewal
  replaces the refresh token.
- Over https the CLI uses TLS 1.2 or newer. It does not follow redirects,
  so your token only ever goes to the host you chose; a host that redirects
  gets an error naming where it points. `firmfact update` follows GitHub's
  download redirects, over https only.
- `firmfact update` installs a release only if its `checksums.txt` carries a
  valid signature by our release key, which is built into the CLI, and the
  archive matches its checksum there. Each release also has build provenance
  attestations and an SBOM per archive.
- `firmfact logout` revokes the sign-in on the server and removes it from this
  machine. If the server cannot be reached, it still removes the local copy,
  says the session is still valid, and exits non-zero; interrupted with
  Ctrl-C, it keeps the local copy so a second run can finish. A workspace
  admin can also revoke it in the web app, under Connected AI clients on the
  MCP page of the workspace you chose at sign-in.
- Report security issues to security@firmfact.com; see [SECURITY.md](SECURITY.md).

## Configuration

| Setting | Meaning |
|---|---|
| `--host`, `FIRMFACT_HOST` | firmfact host (default: the profile's, else `https://firmfact.com`); https only, except `localhost:<port>`, which is plain http |
| `--insecure-http` | allow plain http to a host other than this machine (your token travels unencrypted); `config set-host` remembers it for the profile |
| `--profile`, `FIRMFACT_PROFILE` | named profile (host and default workspace); see [Profiles](#profiles) |
| `--workspace`, `FIRMFACT_WORKSPACE` | workspace id or name (default: the profile's); `firmfact workspaces use <id or name>` |
| `--json`, `--format`, `--jq` | machine-readable output: `--json` (or `--format json`) everywhere, `--format csv` or `tsv` for a workspace command's rows, `--jq <expression>` to filter the JSON |
| `--timeout`, `FIRMFACT_TIMEOUT` | limit for each request, such as `90s` or `2m` (default 60 s, 90 s for a workspace command and 5 minutes for a chat, of which at most 30 s waiting for the server to start answering) |
| `FIRMFACT_TOKEN` | use this access token instead of the stored one; it is only sent to `FIRMFACT_HOST`, or to `https://firmfact.com` when that is not set, and commands aimed at any other host refuse to run rather than send it |
| `FIRMFACT_TOKEN_STORE=file` | keep tokens in `credentials.json` without trying the keyring |
| `FIRMFACT_SIGNUP_CODE` | the 6-digit code from the email, for a scripted `signup` |
| `FIRMFACT_NO_UPDATE_CHECK=1` | skip the daily check for a newer release, and keep `doctor` off GitHub; `0` or `false` leaves it on |
| `CI` | set by CI services: the daily check does not ask GitHub for a newer release, only the service for its minimum version; `CI=false` or `0` is not CI |
| `--debug`, `FIRMFACT_DEBUG=1` | log each request and answer on stderr: the method, path, status, time taken and request id, and why the CLI retries, renews the token or opens a new MCP session. Tokens, codes and passwords are redacted, and the bodies of the sign-in and signup requests are never shown. A workspace command's arguments are shown; answers are not, but for errors |
| `FIRMFACT_CONFIG_DIR`, `FIRMFACT_CACHE_DIR` | override the config and cache locations |
| `HTTPS_PROXY`, `NO_PROXY` | reach firmfact and GitHub through a proxy, but the hosts `NO_PROXY` lists (`HTTP_PROXY` for a plain http host); `doctor` shows the proxy in use |
| `SSL_CERT_FILE`, `SSL_CERT_DIR` | on Linux, a certificate bundle and directory to trust in place of the system's; macOS and Windows use the system's certificate store |
| `NO_COLOR` | no colour; on a terminal the logo, next-step hints and progress still show, in plain text |

A flag wins over its variable, and the variable over the profile, so a shell
or CI job can export `FIRMFACT_HOST` or `FIRMFACT_WORKSPACE` once and a single
command can still aim elsewhere. `firmfact config show` says which settings
came from the environment, and `firmfact help environment` lists every
variable.

A variable never changes the profile. `firmfact login` or `signup` on the
host `FIRMFACT_HOST` names keeps that sign-in, but the profile keeps its own
host and default workspace, for when the variable is unset; `login --host`
is how to point the profile at a host as you sign in. The profile's default
workspace belongs to its host, so on any other host, named with `--host` or
`FIRMFACT_HOST`, commands use the sign-in's own default workspace instead.
`workspaces use` keeps the same rule: with `--host` it points the profile at
that host as well, and where `FIRMFACT_HOST` names a host other than the
profile's it refuses, as the profile could not keep the workspace; there,
`--workspace` or `FIRMFACT_WORKSPACE` names one.

A workspace you name with `--workspace` or `FIRMFACT_WORKSPACE` is always sent
to firmfact, which refuses one your sign-in does not reach, so a command never
quietly runs in another workspace instead.

### Profiles

A profile is a host and a default workspace. The `default` profile is always
there, on `https://firmfact.com` until you point it elsewhere.

```bash
firmfact config use-profile --create staging        # make a profile and switch to it
firmfact config set host staging.firmfact.example   # point the profile in use at a host
firmfact config set workspace <id>                  # its default workspace
firmfact config get host
firmfact config unset workspace
firmfact config profiles list                       # * marks the current profile
firmfact config profiles rename staging acceptance
firmfact config profiles delete acceptance
```

`config use-profile` only switches to a profile that exists, and suggests the
one you meant, so a typo never quietly switches you to a new profile on
`https://firmfact.com`; `--create` makes a new one. `--profile` and
`FIRMFACT_PROFILE` must name a profile that exists too: given any other name,
a command stops with exit status 4 and the names you may have meant, rather
than run on `https://firmfact.com` with the sign-in kept for it. Only
`config set` and `config set-host` make the profile they are given, so
`firmfact --profile ci config set host ci.firmfact.example` sets one up in one
go. `get`, `set` and `unset` work on the profile in use: `--profile`, else
`FIRMFACT_PROFILE`, else the current one. `set workspace` does not check the
workspace with the server; `firmfact workspaces use` does. Sign-ins are kept
by host, so renaming or deleting a profile does not sign you out.

The profiles are kept in `config.json` in the config directory. When that file
cannot be read, say after a hand edit left a trailing comma, commands stop
with its path and the line and column of the problem, rather than carry on
with the defaults and save over it. Fix the file, or run
`firmfact config reset` to move it aside (to `config.json.<date>-<time>.bak`)
and start again from the defaults. `firmfact doctor` reports such a file, and
any profile whose host the CLI would refuse.

## Development

```bash
go test -race ./...
golangci-lint run    # the linters in .golangci.yml
go build -o bin/firmfact ./cmd/firmfact
./bin/firmfact --host http://localhost:<port> doctor
```

Every pull request runs the tests with the race detector on Linux, macOS and
Windows, golangci-lint, govulncheck (also weekly), a GoReleaser snapshot
build and a coverage gate: at least 85% across all packages, measured with
`go test -coverpkg=./... -coverprofile=cover.out ./...` and checked by
`go run ./internal/covergate`, with floors of their own for sign-in,
credential storage, self-update, `claim` and the commands. The code that reads what comes over the
network (tool answers, release versions, checksums and archives) is fuzzed
for 30 seconds a target on every pull request, and for longer every night.

Releases are built by [GoReleaser](https://goreleaser.com) when a `vX.Y.Z` tag
is pushed, once the tagged commit passes the tests and the coverage gate
again and a maintainer approves the release. Each release's notes are its
section of [CHANGELOG.md](CHANGELOG.md). Issues and pull requests are welcome;
[CONTRIBUTING.md](CONTRIBUTING.md) says how, and which parts of the CLI
scripts can rely on from one release to the next.

## Licence

Apache License 2.0; see [LICENSE](LICENSE) and [NOTICE](NOTICE). The name
firmfact is a registered trademark, and the licence does not cover it or the
logo.
