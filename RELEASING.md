# Releasing

How a release of the firmfact CLI is made, and what the repositories need
before the first one. Contributors need none of this; see
[CONTRIBUTING.md](CONTRIBUTING.md).

## What a release publishes

Pushing a tag `vX.Y.Z` runs `.github/workflows/release.yml`. Its test job runs
the checks a pull request has to pass on the tagged commit again, and stops
when CHANGELOG.md has no notes for the version. The release job then waits
for a reviewer's approval in the `release` environment, and GoReleaser
(`.goreleaser.yaml`):

- publishes a GitHub release on firmfact/cli with the archives,
  `checksums.txt`, its signature `checksums.txt.sig` and an SBOM per archive,
  marked as a pre-release when the tag has a pre-release part
  (`v0.1.0-rc.1`);
- for a final release only, commits the Homebrew cask to
  firmfact/homebrew-tap (`Casks/firmfact.rb`) and the Scoop manifest to
  firmfact/scoop-bucket (`bucket/firmfact.json`);
- writes the winget manifests, which it does not publish yet (see
  [winget](#winget)).

The job keeps the winget manifests with the run, as the `winget-manifests`
artifact, and hands `checksums.txt` to an `attest` job of its own, which
attests the build provenance of every file it lists. Only that job can sign
with the workflow's identity, and it runs nothing but GitHub's artifact and
attest actions.

## Secrets

All three are secrets of the `release` environment, not of the repository,
so only a release job someone has approved can read them. release.yml hands
them to the GoReleaser step alone, where goreleaser-action, GoReleaser and the
Go commands GoReleaser starts (the builds, and the signer) can read them; syft
runs in a container without them. `RELEASE_REPOS_TOKEN` goes to a final
release only, and `WINGET_GITHUB_TOKEN` to none until winget is switched on.

| Secret | What it is | What it is for |
|---|---|---|
| `RELEASE_SIGNING_KEY` | The Ed25519 release key, as a PEM `PRIVATE KEY` (PKCS #8). Its public half is in `releaseKeys` in `internal/update/signature.go`, and the installers on firmfact.com trust the same keys. | Signs `checksums.txt`. Without it, or with a key the CLI does not trust, the release fails before anything is published. To rotate it, follow the comment on `releaseKeys`. |
| `RELEASE_REPOS_TOKEN` | A fine-grained personal access token with the `firmfact` organisation as resource owner, access to firmfact/homebrew-tap and firmfact/scoop-bucket only, and the Contents permission set to read and write. | Commits the cask and the manifest for a final release. The workflow's own `GITHUB_TOKEN` cannot write outside firmfact/cli. |
| `WINGET_GITHUB_TOKEN` | A classic personal access token with the `public_repo` scope, of an account that can push to firmfact/winget-pkgs. A fine-grained token only reaches the repositories of its own resource owner, and the pull request goes to microsoft/winget-pkgs. As `public_repo` reaches every public repository the account can write to, make it on an account that can write to that fork and nothing else. | Opens the winget pull request, once winget is switched on. Until then release.yml does not pass it on; leave it unset, and add it with the change that switches winget on (see [winget](#winget)). |

Give both tokens an expiry date and renew them before it. GoReleaser
publishes the GitHub release before it commits the cask and the manifest, so
a final release with an expired `RELEASE_REPOS_TOKEN` is out on GitHub, with
its provenance, but not in Homebrew or Scoop, and an immutable release cannot
be run again (see below). Renew the token and release the next patch version.

## Repository settings

On firmfact/cli:

- **Release immutability** (Settings, General, Releases). A published
  release, its assets and its tag can then never be changed. GoReleaser
  uploads everything to a draft and publishes it last, which is what an
  immutable release needs. A release that goes wrong after it is published
  is put right with a new patch version, never by replacing its assets.
- **Private vulnerability reporting** (Settings, Advanced Security).
  SECURITY.md and the issue forms send security reports there.
- **The `release` environment** (Settings, Environments): the release
  owner as required reviewer, deployment limited to tags matching `v*`, so
  that no branch can run the release job, and the secrets above.
- **A tag ruleset** for `v*` that lets only admins create, move or delete
  such tags. The installers on firmfact.com and the check the README shows
  accept an attestation made by release.yml for the release's own tag, and
  someone who could push such a tag could run an edited release.yml. For
  the same reason, release.yml keeps its name.

firmfact/homebrew-tap and firmfact/scoop-bucket are public, each with a
`main` branch; a first commit with a README is enough, and GoReleaser
creates `Casks/` and `bucket/`. For winget, the organisation needs a fork of
microsoft/winget-pkgs named firmfact/winget-pkgs.

## A release candidate, then the release

1. Pick the version by the rules in
   [Compatibility and versioning](CONTRIBUTING.md#compatibility-and-versioning):
   a patch release for fixes alone, a minor release for anything added, and
   a major release for anything under Breaking (before 1.0.0, a minor one).
2. Tag a candidate on `main` and push the tag:
   `git tag v0.2.0-rc.1 && git push origin v0.2.0-rc.1`. A candidate needs no
   section of its own in CHANGELOG.md: its notes are what Unreleased lists.
   Approve the release job.
3. The candidate is a pre-release on GitHub, with every archive, the signed
   checksums and the provenance. Homebrew, Scoop and winget are left alone,
   and GitHub's latest-release page, which the CLI's daily check and the
   installers read, still names the last final release. Try it:
   - `curl -fsSL https://firmfact.com/install.sh | FIRMFACT_VERSION=0.2.0-rc.1 sh`
     on Linux and on a Mac, and on Windows
     `$env:FIRMFACT_VERSION = '0.2.0-rc.1'; irm https://firmfact.com/install.ps1 | iex`;
   - `go install github.com/firmfact/cli/cmd/firmfact@v0.2.0-rc.1`;
   - an archive checked by hand with `gh attestation verify`, as the README
     shows.

   Something wrong? Fix it on `main` and tag `v0.2.0-rc.2`.
4. In CHANGELOG.md, turn Unreleased into a section for the version, with
   the day's date (`## 0.2.0 - 2026-10-01`), start a new, empty Unreleased
   above it, and merge that. `go run ./internal/relnotes v0.2.0` prints the
   notes the release will get.
5. Tag the merge commit and push the tag:
   `git tag v0.2.0 && git push origin v0.2.0`, and approve the release job.
   This time GoReleaser also commits the cask and the Scoop manifest.
6. Check the final release where people install it:
   `brew install firmfact/tap/firmfact` on a Mac (the first run must not be
   stopped by Gatekeeper), `scoop install firmfact` after adding the bucket,
   and both one-line installers. `firmfact version` names how each copy was
   installed.

The release workflow stops, before anyone is asked to approve, when
CHANGELOG.md has no section for a final version or still lists changes under
Unreleased.

## winget

The manifests are ready but not published: `skip_upload: true` in the winget
section of `.goreleaser.yaml`. Every build writes them to
`dist/winget/manifests/f/Firmfact/CLI/<version>/`; CI's release-config job
keeps a snapshot's, and a release keeps its own as the `winget-manifests`
artifact of the run.

Once the first final release has been checked:

1. Fork microsoft/winget-pkgs into the firmfact organisation as
   firmfact/winget-pkgs.
2. On Windows, download the release run's `winget-manifests` artifact and try
   it: `winget validate --manifest <folder>`, then, as administrator,
   `winget settings --enable LocalManifestFiles`, and
   `winget install --manifest <folder>`. `firmfact version` should say
   `install   winget`, and `winget uninstall --exact --id Firmfact.CLI`
   should remove it.
3. Switch winget on in one change: change winget's `skip_upload` in
   `.goreleaser.yaml` from `true` to `auto`, and pass
   `WINGET_GITHUB_TOKEN: ${{ secrets.WINGET_GITHUB_TOKEN }}` to release.yml's
   GoReleaser step, where its comment says. Add the `WINGET_GITHUB_TOKEN`
   secret to the `release` environment as you merge it. From the next final
   release on, GoReleaser pushes a branch `Firmfact.CLI-<version>` to the
   fork and opens a pull request against microsoft/winget-pkgs `master`; a
   candidate still publishes nothing there. To publish the release you
   tried without waiting for the next one, open that pull request by hand
   from the fork, with the three files of the artifact under
   `manifests/f/Firmfact/CLI/<version>/`.

winget's moderators review a new package before
`winget install --exact --id Firmfact.CLI` finds it, and the first pull
request asks for Microsoft's contributor licence agreement to be signed.
Once the package is accepted, change the README's install section to match.
