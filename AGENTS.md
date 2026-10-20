# AGENTS.md

Guidance for AI coding agents working in this repository.

## Repository Context

- This project is a Kubernetes controller that watches `RoleBinding` and `ClusterRoleBinding` resources and assigns referenced Microsoft Entra ID groups to configured service principals.
- The repo uses Go, Helm, OpenTofu, GitHub Actions, MegaLinter, prek, Task, ko, and git-cliff.
- Main contributor docs are in `CONTRIBUTING.md`, `TESTING.md`, `RELEASING.md`, `docs/ARCHITECTURE.md`, `tofu/README.md`, and `e2e/tofu/README.md`.

## Working Style

- Prefer small, focused changes over broad refactors.
- Use Conventional Commits. The preferred style is scoped titles like `fix(ci): ...` or `feat(ci): ...`.
- Do not commit directly to `main`; it is protected. Use PR branches.
- Do not revert unrelated dirty worktree changes. Other agents or the user may be working concurrently.
- If asked to commit or push, inspect `git status`, `git diff`, and recent commits first.

## Validation

- Use `task --list` to discover repo tasks.
- For simple whitespace validation, run `git diff --check`.
- The user has said it is acceptable to skip local `prek`; CI will run it.
- CI linting should be read-only. Do not add `--fix`, `APPLY_FIXES=all`, or similar auto-fix behavior to CI.
- When touching OpenTofu, useful local checks are:
  - `tflint --chdir=tofu --recursive`
  - `tflint --chdir=e2e/tofu --recursive`
- git-cliff uses `.cliff.yml`; repo-owned commands should pass `--config .cliff.yml`.

## CI And Linting Notes

- The main lint workflow is `.github/workflows/prek.yml`.
- MegaLinter config is `.mega-linter.yml`.
- ShellCheck config is `.shellcheckrc` and is intentionally strict:
  - `enable=all`
  - `severity=style`
  - `external-sources=true`
- `REPOSITORY_GIT_DIFF` is enabled in MegaLinter to catch `git diff --check` issues.
- `actionlint` is intentionally disabled until Ubuntu 26.04 support lands upstream: <https://github.com/rhysd/actionlint/pull/683>.
- `zizmor` is used for GitHub Actions security linting and should remain strict:
  - `--persona=pedantic`
  - `--min-severity informational`
  - `--min-confidence low`
  - `--strict-collection`
- Strict `zizmor` has previously passed locally with:
  - `zizmor --persona=pedantic --min-severity informational --min-confidence low --strict-collection .github/workflows`
- For strict `zizmor`, keep workflow permissions scoped at the job level where practical, explain write permissions inline, use `persist-credentials: false` for checkout steps, avoid unnecessary `setup-go` cache in release workflows, and prefer moving GitHub expression values into `env` before using them in shell.
- All GitHub Actions should be pinned by full SHA with a version comment.

## MegaLinter SARIF

- MegaLinter may create a SARIF file that exists but has no useful results on clean runs.
- `github/codeql-action/upload-sarif` can reject effectively empty SARIF content.
- Guard SARIF uploads with a check that the SARIF file is non-empty and contains at least one run with results before calling `upload-sarif`.
- Missing or empty SARIF should skip upload; malformed JSON should still fail.

## Vulnerability Scanning

- Container image and IaC scanning uses Trivy via `.github/workflows/scan-trivy.yml`.
- The old Clair workflows (`clair-db.yml` and the Clair steps in `release-preview.yml`) were removed. Do not reintroduce them.
- `.goreleaser.yml` must set `kos.base_image` explicitly. GoReleaser does not read `.ko.yaml`, so omitting it silently falls back to ko's default base image while the OCI label still claims the pinned one.
- Container images are published with an SPDX SBOM attached and written to `dist/sbom`. Keep `kos.sbom` enabled.
- `limit-severities-for-sarif: true` is mandatory on every Trivy invocation. Without it `trivy-action` unsets `TRIVY_SEVERITY` for SARIF output and the report silently includes every severity.
- Trivy cannot read the `ko` OCI archive tarball directly. Unpack the layout with `tar -xf` and scan the directory.
- Every scan sets a distinct SARIF `category` so results stay separate from `govulncheck` and MegaLinter.
- Suppressions belong in `.trivyignore.yaml` and must include a `statement` and an `expired_at` date. Do not add path-qualified suppressions: Trivy reports targets relative to the scan root, so the same entry breaks depending on which directory is scanned.
- `govulncheck` remains the source of truth for Go dependency reachability. Trivy image scanning is the broader net for OS packages.

## Dependency Updates

- Renovate config lives in `renovate.json`; validate changes with `npx --package renovate renovate-config-validator`.
- `main` requires signed commits and linear history with no bypass actors, so `platformCommit` is enabled. Renovate commits through the GitHub API, which signs them. Do not disable it.
- There is no merge queue and no required status checks on `main`, so Renovate must not automerge. Leave `automerge` off and let a human merge.
- Pinned CLI tools installed by `curl` in workflows (git-cliff, helm, ko, kind) are tracked with `customManager` regexes. When adding a new curl pin, add a matching manager or the version will silently stop updating.
- The ko base image is pinned twice: `.ko.yaml` `defaultBaseImage` and `.goreleaser.yml` `kos.base_image`. Both need a manager so they stay in sync.
- `terraform.lock.hcl` is gitignored, so Renovate only bumps the `~>` constraints in `tofu/main.tf` and `e2e/tofu/main.tf`; the lock is regenerated on the next `tofu init`.

## Release Automation

- Release preview comments should update in place instead of creating duplicates.
- The current approach uses a hidden marker: `<!-- release-preview-comment -->`.
- Existing bot comments should be found by marker first, with a fallback for older comments containing `Release Preview:`.
- Release workflows have been hardened for strict `zizmor` findings. Preserve those patterns when editing them.

## E2E And OpenTofu Notes

- E2E tests use a real kind cluster and real Azure Entra ID resources with Workload Identity Federation.
- Before running local `task e2e`, follow `TESTING.md` to export the required static values. Maintainers can read them from GitHub repo variables or `tofu/` outputs when the static backend is initialized. Do not run e2e with empty `OIDC_ISSUER_URL`, `TEST_GROUP_CRB_ID`, `TEST_GROUP_RB_ID`, or `E2E_TEST_USER_UPN`.
- A known destroy failure pattern is an AzureAD/OpenTofu 404 on `azuread_application_identifier_uri.cluster_api`.
- Retrying full `tofu destroy` helps because the next run refreshes state and sees missing resources.
- `Taskfile.yml` `e2e:infra-down` retries `tofu destroy` up to three times with 20s/40s backoff.
- `e2e/tofu/main.tf` includes `static_oidc_issuer_url` because `Taskfile.yml` still passes it with `-var="static_oidc_issuer_url={{.OIDC_ISSUER_URL}}"`. It may need `# tflint-ignore: terraform_unused_declarations` if unused by resources.
