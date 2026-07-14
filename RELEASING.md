# Releasing

Releases are PR-driven. A release PR gets the `release-candidate` label, the preview workflow comments with the next version and release notes, and the publish workflow runs when that PR is merged.

## One-command release PR

Use this for a release that does not need code changes:

```bash
task release:pr
```

The task will:

- verify required tools are installed: `git`, `gh`, and `git-cliff`
- require a clean working tree
- compute the next version with `git-cliff --bumped-version`
- update local `main` from `origin/main`
- create an empty commit on `release/<version>`
- push the branch
- open a PR titled `chore: release <version>`
- add the `release-candidate` label

To override the generated branch name:

```bash
task release:pr RELEASE_BRANCH=release/my-release
```

## Manual Empty-commit Release PR

Use this if you need to control each step manually:

```bash
git switch main
git pull --ff-only origin main

NEXT_VERSION="$(git-cliff --bumped-version)"
RELEASE_BRANCH="release/${NEXT_VERSION}"

git switch -c "$RELEASE_BRANCH"
git commit --allow-empty -m "chore: release ${NEXT_VERSION}"
git push -u origin "$RELEASE_BRANCH"

gh pr create \
  --base main \
  --head "$RELEASE_BRANCH" \
  --title "chore: release ${NEXT_VERSION}" \
  --body "Release candidate for ${NEXT_VERSION}. Merge this PR after the release preview and CI pass."

gh pr edit "$RELEASE_BRANCH" --add-label release-candidate
```

## Release Preview

The `Release Preview` workflow runs on PR open, synchronize, or label events when the PR has the `release-candidate` label.

It does three things:

- computes the next semantic version with `git-cliff --bumped-version`
- generates changelog entries with `git-cliff --latest --strip header --unreleased --tag <version>`
- appends image, Helm chart, and cosign verification instructions to the release notes
- packages the Helm chart with the computed version and app version
- runs `goreleaser release --clean --snapshot --skip=publish`

Review the preview comment before merging. It should show the expected version and release notes, and the GoReleaser dry run should pass.

## Publishing

Merging a PR labeled `release-candidate` triggers the `Release` workflow.

The release workflow:

- computes the next version with `git-cliff --bumped-version`
- creates and pushes an annotated tag such as `v1.2.0`
- generates release notes with `git-cliff` and appends image, Helm chart, and cosign verification instructions
- logs in to GHCR once using a shared Docker-compatible registry auth file
- builds the Linux `amd64` and `arm64` binaries
- builds and pushes the multi-arch container image to `ghcr.io/dronenb/azure-k8s-role-assigner`
- writes the published image digest to `azure-k8s-role-assigner_digests.txt`
- packages the Helm chart using the release version
- pushes the Helm chart to GHCR as an OCI artifact
- signs checksums and container manifests with keyless cosign
- creates the GitHub Release and uploads the packaged Helm chart
- updates the GitHub Release notes with digest-pinned image and chart artifact references after publishing completes

## Versioning Rules

Versions are computed from conventional commits by `git-cliff`:

- `feat:` bumps the minor version
- `fix:` bumps the patch version
- breaking changes bump the major version
- `chore:`, `ci:`, `build:`, and `style:` are skipped in release notes

The empty release commit uses `chore:` so it does not change the computed version.

## Published Artifacts

| Artifact | Location |
| --- | --- |
| `manager` Linux binary | GitHub Release assets |
| Container image | `ghcr.io/dronenb/azure-k8s-role-assigner@sha256:<digest>` in final release notes |
| `latest` container tag | `ghcr.io/dronenb/azure-k8s-role-assigner:latest` |
| Helm chart OCI artifact | `ghcr.io/dronenb/charts/azure-k8s-role-assigner@sha256:<digest>` in final release notes |
| Helm chart package | GitHub Release assets |
| Checksum signature bundle | `*.sigstore.json` release asset |
| Image signature | Attached to the OCI manifest in GHCR |

## Verification

Verify the container image:

```bash
cosign verify ghcr.io/dronenb/azure-k8s-role-assigner@sha256:<digest> \
  --certificate-identity-regexp="https://github.com/dronenb/azure-k8s-role-assigner" \
  --certificate-oidc-issuer="https://token.actions.githubusercontent.com"
```

Pull the Helm chart from GHCR. Use the versioned chart reference for Helm commands, and compare the digest with the digest listed in the final release notes:

```bash
helm pull oci://ghcr.io/dronenb/charts/azure-k8s-role-assigner --version <version-without-v>
```

Install the Helm chart from GHCR:

```bash
helm upgrade --install azure-k8s-role-assigner \
  oci://ghcr.io/dronenb/charts/azure-k8s-role-assigner \
  --version <version-without-v> \
  --namespace azure-k8s-role-assigner \
  --create-namespace
```

Verify a release binary against its signature bundle:

```bash
cosign verify-blob \
  --bundle manager-linux-amd64.sigstore.json \
  manager-linux-amd64
```

## If Something Fails

If the preview fails, push fixes to the release PR branch and wait for the preview to update.

If publishing fails after the tag is pushed, inspect the failed workflow before retrying. Do not delete or recreate release tags unless you have confirmed no artifact was published for that tag.
