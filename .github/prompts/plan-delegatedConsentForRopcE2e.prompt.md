# Plan: Delegated Consent for ROPC E2E

ROPC now works locally, but the remaining gap is consent: the e2e client needs delegated permission grant(s) for the cluster app scope without giving the GitHub Actions identity broad tenant power. The plan is to keep the existing app-permission policy for controller Graph access, add a delegated-grant path for the ROPC client, and keep the GitHub Actions service principal constrained to just the specific grants required by this repo.

**Steps**
1. Refactor the bootstrap policy in [tofu/bootstrap.ps1](tofu/bootstrap.ps1) so one permission-grant policy can allow both application permission grants and delegated permission grants, without expanding to tenant-wide consent.
2. Confirm the exact AzureAD Terraform resource to pre-consent delegated scopes for the ROPC client, using the cluster app as the resource and a dedicated public client as the caller.
3. Update `e2e/tofu/main.tf` to create the delegated permission grant for the cluster app scope, optionally scoped to the static e2e user with `user_object_id` if supported and desirable.
4. Update [Taskfile.yml](Taskfile.yml) only if needed so local and CI flows continue to fetch the e2e password from Key Vault and clear kubelogin cache between attempts.
5. Re-run `tofu apply` for static and dynamic infra, then `task e2e:verify` to confirm the user can obtain a token without interactive consent.

**Relevant files**
- [e2e/tofu/main.tf](e2e/tofu/main.tf) — add delegated permission grant for the ROPC client against the cluster app scope
- [tofu/bootstrap.ps1](tofu/bootstrap.ps1) — allow the GitHub Actions SP to bootstrap both app-role and delegated-grant consent under one policy boundary
- [Taskfile.yml](Taskfile.yml) — preserve password download and cache cleanup behavior for local verification
- [TESTING.md](TESTING.md) — document the consent model and why user consent should not be required

**Verification**
1. `tofu apply -auto-approve` in `tofu/` succeeds and the static user/password/CA resources are present.
2. `tofu apply -auto-approve` in `e2e/tofu/` succeeds and the delegated grant exists for the ROPC client.
3. `task e2e:verify` succeeds without consent_required or invalid_client errors.

**Decisions**
- Do not give GitHub Actions tenant-wide admin consent rights.
- Use one bootstrap policy boundary for both application permissions and delegated permissions where possible.
- Treat user consent as something to avoid in the e2e path, not something to prompt for.
- Keep the static e2e user and the ROPC client separate from the controller identity.
