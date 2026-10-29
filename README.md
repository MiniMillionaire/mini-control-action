# minicontrol-runner

Ephemeral macOS GitHub Actions runners on a [mini-control](https://github.com/Lakr233/mini-control) fleet:

> job queued → grab a VM → install a JIT runner over ssh-exec → job runs → **VM deleted**.

Every job gets a factory-fresh VM; no VM outlives its job (billing runs until
deletion, so cleanup is the top design goal). Headless: one binary, one
`config.yaml`, one JSON state file.

```
                    ┌──────────────────────────────────────────────────────┐
                    │                  minicontrol-runner                  │
  ┌────────┐  poll  │ ┌──────────┐     ┌─────────────┐   ┌──────────────┐  │
  │ GitHub │◄───────│ │  poller  │────►│ dedup queue │──►│    scaler    │  │
  └────────┘        │ └──────────┘     └─────────────┘   │ 1 FSM / job  │  │
      ▲             │ ┌──────────┐     ┌─────────────┐   └──────┬───────┘  │
      │ JIT config, │ │reconciler│◄───►│ state.json  │◄─────────┤          │
      │ job status  │ └──────────┘     └─────────────┘          │          │
      └─────────────│  httpsrv: /healthz /status (no ingress)   │          │
                    └───────────────────────────────────────────┼──────────┘
                                                                ▼
                                 mini-control API ───► macOS VM (JIT runner,
                                 Bearer mck_…, worker host)     one job, exits)
```

## Per-job lifecycle

```
QUEUED → CREATING → WAIT_READY → BOOTSTRAPPING → RUNNER_UP → JOB_RUNNING → JOB_DONE
   └────────┴───────────┴─── any failure/timeout ───┴────────────┴──► DELETING → COMPLETE
```

Cleanup is quadruple-redundant: the FSM's terminal delete, a reconciler
re-issuing stuck deletes, startup resume from `state.json`, and orphan GC
(any VM under the API key with no record is deleted after a grace period —
**the mini-control API key must be dedicated to this service**).

Runners claim jobs by *label*, not name, so a runner may serve a sibling
job; the VM's lifetime follows its own runner (in-VM marker + GitHub busy
flag), and a job record only completes when GitHub says its job completed.

## Quick start

```sh
cp config.example.yaml config.yaml     # edit: base_url, scope/repos, sku, labels
# Put MINICONTROL_API_KEY and GITHUB_PAT in a local .env (never commit it).
# GitHub: Administration read/write + Actions read; account needs repo admin.
docker compose build
docker compose run --rm minicontrol-runner --config /etc/minicontrol-runner/config.yaml --dry-run
docker compose up -d --build
```

Label your workflow `runs-on: [self-hosted, macos, arm64, mini-control]`.
Discovery is pull-only — no public ingress, no webhooks; jobs are picked up
within the relevant active or quiet polling interval.

## Configuration

`config.example.yaml` documents the legacy single-token configuration.
`config.accounts.example.yaml` shows multi-account discovery and resource pools. The load-bearing ones:

| Key | Meaning |
|---|---|
| `github.scope` | `all` = serve every repo the token grants; `repo` = one repo; `org` = org runners |
| `minicontrol.base_url` | the deployment's **worker** hostname (`https://…/api/client/v1`) |
| `minicontrol.sku` | fixed hardware preset, e.g. `m4-big-v1` |
| `runner.labels` | job runs-on must be a subset of these |
| `limits.max_concurrent_vms` | fleet/bill bound; excess jobs wait in GitHub's queue |
| `state.path` | volume-mounted; losing it is survivable (orphan GC) but don't |

Secrets are referenced as `${ENV_VAR}`; unknown keys fail at startup.

### Multiple accounts and repository policies

Use `github.accounts` instead of legacy `github.auth`, `scope: repo|org`, or
`poll.repos`. Each named account has a token, optional `api_base_url`, and
`discover: all` (default) or `none`. `owners` limits discovery to listed users
or organizations; omit it to consider all visible repositories. `exclude`
accepts `owner/repo` globs. Explicit `repositories` entries override defaults
and are polled even when absent from discovery. Exclusion always wins.

Policy values inherit from global `defaults`, then account `defaults`, then
repository overrides. Set `pool`, `max_concurrent_vms`, `max_job_duration`,
`regions`, or `paused`. A repository's regions must be a nonempty subset of
its pool's regions. Region lists replace inherited lists; they never merge.
Account and pool pauses cannot be overridden by a repository.

Pools specify `sku`, optional `base_image` (exact catalog name) or an ordered
`base_images` fallback list, ordered `regions`, and `max_concurrent_vms`. Explicit
capacity rejection advances to the next permitted region with a fresh durable
idempotency key; once every region has rejected an image, the next image is
tried, wrapping back to the first. Missing or unsynced images are 409
rejections too (`base_image_not_found`, `base_image_not_ready`), so they fall
back the same way. A single region keeps retrying there. No capacity fallback
removes placement constraints. `any_region: true` is a separate explicit choice
and cannot be combined with `regions`. Transport failures retain the same
request and key until its outcome is known. Worker placement is logged.

Global, pool, and repository concurrency limits apply together. Waiting
repositories share admission opportunities, and capacity backoff holds no
reservation. Existing VMs continue to be monitored even when their account or
pool is paused or the configured limit is lowered. Deletion must be confirmed
before its reservation is released. New-mode runners register at repository
scope so repositories with different placement policies do not share runners.

Discovery refreshes the complete candidate list every five minutes (a listing
that reaches the 1,000-repository safety cap fails explicitly). Active repos
use `poll.interval`; quiet repositories, including previously unseen reruns,
use `poll.quiet_interval` (default ten minutes). Explicit repos and repos with
active state records use the faster interval. GitHub `/user/repos` is a
candidate list, **not proof of token authorization**. Queued-job access and
runner-list access are checked before provisioning; only JIT registration can
prove the token's write permission. One failed account does not stop the rest.

When accounts overlap, an explicit repository entry wins; otherwise lower
`priority` wins, then account name. Ownership is persisted separately from job
retention and survives restarts. Removing permissions never silently switches
tokens. A conflicting explicit assignment is reported in `/status`; changing
ownership requires draining and an intentional state migration. Rotating a
token under the same account name preserves ownership.

`/status` includes account/repository discovery status and admission wait
reasons. Keep this endpoint private: it includes repository and job metadata.
Actual tokens are neither configuration snapshots nor status fields.

### Upgrading an existing deployment

Back up configuration and state before upgrading. Old single-token config
remains supported. To enable accounts for the first time, drain all existing
nonterminal records first; startup refuses to reinterpret active legacy jobs.
Keep existing account names, GitHub hosts, runner groups, and referenced pools
until their active jobs finish. In-flight jobs retain their original SKU, image selection,
region candidates, selected region, and runner settings across restarts.
Configuration changes take effect on restart; there is no hot reload.

Compose reads `.env` through `env_file`, so additional token variable names do
not require editing the public Compose file. Never commit real `.env`,
`config.yaml`, state, token scripts, or deployment host details. The Docker
build context excludes local configuration, credentials, state, and Git data.


## In-VM bootstrap (3 cheap execs)

```
stage:   base64 script ─► ~/mcra-bootstrap.sh, JIT blob ─► ~/.mcra-jit.b64
launch:  nohup ~/mcra-bootstrap.sh &      (no exec ever waits on the job)
poll:    cat ~/.mcra-status               (PHASE=download|extract|run|done EXIT=n)
```

## Operations

| | |
|---|---|
| `--dry-run` | validate config + both APIs; creates nothing |
| `--smoke` | one VM → `uname -a` → delete (bills real money) |
| `GET /healthz`, `GET /status` | liveness; JSON of all job records |
| SIGTERM | keeps running VMs; next start resumes them |

## Development

```sh
make test    # fully offline: protocol-faithful fakes for both APIs
make race
make lint
```

## License

[MIT](LICENSE)
