# ACA sandbox runners

GitHub Actions scale-set runners on Azure Container Apps (ACA) Sandboxes. One controller replica listens for jobs, creates one JIT runner per sandbox, and disposes it after the job. Runner adoption after restart, a warm pool, orphan cleanup and optional failure snapshots are retained.

## Ownership

- `controller/`: Go controller, tests and controller image.
- `infra/`: its sandbox group, registry, Key Vault, managed identity and ACA deployment.
- `image/`: Linux x64 runner, Docker Engine and JIT entrypoint.
- `scripts/` and `.github/workflows/`: credential setup, image registration, validation and deployment.

The [sandbox-group Terraform module](https://github.com/kewalaka/terraform-azurerm-ptn-cicd-sandbox-runners) remains a separate, commit-pinned dependency. Benchmarks, ACA Jobs comparisons and research stay in [sandbox-runners-lab](https://github.com/kewalaka/sandbox-runners-lab). CI tools belong to [azure-iac-toolbox](https://github.com/kewalaka/azure-iac-toolbox), not the runner image; neither build requires a toolbox image/tag.

## Deployment inputs

Create a GitHub deployment environment with Azure OIDC credentials. Cloud workflows are manual and take that environment as an input; no deployment or repository is assumed.

| Environment setting | Purpose |
| --- | --- |
| Secrets `ARM_TENANT_ID`, `ARM_SUBSCRIPTION_ID`, `ARM_CLIENT_ID` | Azure OIDC identity |
| Variables `LOCATION`, `NAME_PREFIX` | Sandbox-supported region; 3–12 lowercase letters/digits, starting with a letter |
| `RUNNER_GITHUB_CONFIG_URL` | HTTPS repository URL whose jobs the runners execute |
| `TF_STATE_RESOURCE_GROUP`, `TF_STATE_STORAGE_ACCOUNT`, `TF_STATE_CONTAINER`, `TF_STATE_KEY` | Existing Azure Blob backend; a distinct state key per deployment |
| `CI_PRINCIPAL_ID` | Optional image publisher **object ID**, not its client ID; grants AcrPush and sandbox data-plane access |
| `DATA_PLANE_OPERATORS` | JSON map of `{principal_id, principal_type}` operators; grants sandbox access and Key Vault Secrets Officer |
| `SANDBOX_ENDPOINT` | Regional sandbox management endpoint, required by runner image registration |

The deploy identity needs resource creation and role-assignment permissions on the deployment scope, plus Storage Blob Data Contributor on the backend. The image publishing identity needs `CI_PRINCIPAL_ID` configured or equivalent externally managed permissions. Backend resources must exist before deployment; they are not created or destroyed here.

Optional environment variables: `CONTROLLER_ENV` and `TAGS` are JSON maps; `SANDBOX_VNET_CONNECTION` is a JSON object with `subnet_id`; `CONTROLLER_ENVIRONMENT_ID` reuses an existing ACA environment, or `CONTROLLER_SUBNET_ID` attaches a new one to an existing infrastructure subnet. Do not set both controller network options. Sandbox subnet delegation is `Microsoft.App/environments`. Networks, DNS, routing and private endpoints remain deployment-owned; these inputs do not make the registry or vault private.

App IDs can be supplied as `RUNNER_GITHUB_APP_CLIENT_ID` and `RUNNER_GITHUB_APP_INSTALLATION_ID`, or as deploy workflow inputs. Credentials are Key Vault secrets, never Terraform values: `github-app-private-key` for an App, or `github-token` for a PAT.

## Deployment order

1. Configure the environment, backend and permissions above. Run **deploy** with `deploy_controller=false`. This creates the supporting infrastructure and controller identity without starting the controller. Resource names use `rg-<prefix>`, `sbxg-<prefix>`, and subscription-suffixed registry/vault names; use Terraform outputs for exact values.
2. Create and install a GitHub App on the target repository, storing its key directly in the new vault:
   ```sh
   pwsh scripts/create-github-app.ps1 -VaultName "$VAULT_NAME" -Owner "$GH_OWNER" -Repo "$GH_REPO"
   ```
   For organization ownership add `-OwnerType Organization`; for another GitHub host add `-GitHubHost "$GH_HOST"`. The script needs PowerShell 7 and an authenticated Azure CLI operator with vault secret-write access. It opens a browser and writes local, ignored `infra/github-app.auto.tfvars`. Copy the resulting **client and installation IDs only** into the GitHub environment settings. For manual Azure authentication, use a temporary `AZURE_CONFIG_DIR` and `az login --tenant "$ARM_TENANT_ID"`; select the deployment subscription before running scripts.
3. Run **controller-image** and **image** for the same environment. Builds push `linux/amd64` images to its registry with commit-SHA tags. **image** also registers a disk image named `runner-<commit>` and waits for Ready. Set `SANDBOX_ENDPOINT` from Terraform's `management_endpoint` output.
4. Run **deploy** with `deploy_controller=true`, the App IDs, and the controller image's commit SHA as `controller_image_tag`. Keep the same inputs/backend on every apply; no disposable state or skip-if-present shortcut is used.
5. Route target-repository jobs with `runs-on: aca-sandbox`, or the configured `SCALE_SET_NAME`. Use a distinct scale-set name per deployment targeting the same repository. Docker starts alongside the runner handshake; Docker jobs should wait until `docker info` succeeds.

The **destroy** workflow uses the same backend and requires `destroy:<environment>` confirmation. It destroys only Terraform-managed deployment resources, not external networks, an existing ACA environment or backend storage. Key Vault soft deletion can prevent immediate reuse of the same vault name; purge only the intended vault if your recovery policy permits it.

## Controller settings

`CONTROLLER_ENV` maps directly to environment-only controller configuration. Common settings:

| Setting | Default |
| --- | --- |
| `SCALE_SET_NAME`, `MAX_RUNNERS`, `WARM_POOL`, `CREATE_CONCURRENCY` | `aca-sandbox`, `10`, `0`, `8` |
| `SANDBOX_CPU`, `SANDBOX_MEMORY` | `2000m`, `4096Mi` |
| `SANDBOX_DISK_IMAGE_ID` | Unset: newest Ready image labelled `runner-*`, cached for five minutes |
| `EGRESS_ALLOW` | `*.actions.githubusercontent.com,*.github.com,github.com`; `*` disables this allowlist |
| `MAX_JOB_DURATION`, `AUTO_SUSPEND`, `MAX_SANDBOX_AGE` | `6h`, `6h15m`, `6h10m` |
| `KEEP_ON_FAILURE`, `SNAPSHOT_RESULTS`, `SNAPSHOT_TTL` | `false`, `failed`, `24h` |

Sandbox idle time measures data-plane calls, not job activity. Both suspend and maximum-age limits must exceed `MAX_JOB_DURATION`; raise them together for longer jobs. Extend the egress allowlist for job dependencies and your GitHub host. Failure snapshots retain job disks and may contain sensitive job data; leave them off unless needed.

For local execution, set `GITHUB_CONFIG_URL`, `SANDBOX_SUBSCRIPTION_ID`, `SANDBOX_RESOURCE_GROUP`, `SANDBOX_GROUP`, and `SANDBOX_ENDPOINT` (or explicit `SANDBOX_REGION`), plus a PAT or the three `GITHUB_APP_*` authentication variables; then run `go run .` from `controller/` with an Azure credential already available. No region is assumed. The optional runner-online REST probe is GitHub.com/PAT-only and never sends another host's PAT to GitHub.com.

## Local validation

```sh
(cd controller && go test -race ./... && go vet ./...)
terraform -chdir=infra init -backend=false -input=false
terraform -chdir=infra fmt -check -recursive
terraform -chdir=infra validate
terraform -chdir=infra test
bash -n image/entrypoint.sh
shellcheck image/entrypoint.sh
actionlint
```

Terraform tests mock Azure and override the external module; they do not deploy or prove service-preview availability. Actual image builds require Docker: `docker build --platform linux/amd64 controller` and `docker build --platform linux/amd64 image`.

Keep downstream-only inputs, tests and environment settings excluded using that downstream repository's `.dcrsync.yaml`. Configure it with existing `dcrsync` tooling privately; no speculative schema or sync automation is included here. `.gitignore` controls Git tracking of local credentials/state, not upstream/downstream synchronization.
