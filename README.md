# bort

<p align="center">
  <strong>move self-hosted apps between PaaS platforms without gambling on traffic, secrets, or data.</strong>
  <br />
  Coolify → Dokploy first. Dokploy → Coolify, cross-server moves, and more Docker-shaped platforms next.
  <br />
  <a href="#about">About</a>
  ·
  <a href="#quick-start">Quick start</a>
  ·
  <a href="#safety-model">Safety model</a>
  ·
  <a href="#roadmap-and-status">Roadmap</a>
</p>

## About

Bort is a migration cockpit for people running their own app platform on a VPS.
It turns a risky platform switch into a guided run: inventory what is actually
running, explain what needs attention, prepare the target privately, move state,
cut traffic over, and keep a rollback path until you accept the new home.

The first product path is **same-VPS Coolify → Dokploy**. That is the migration
operators ask for when they already have real apps, domains, env, databases,
volumes, and platform leftovers on one box, and they do not want the answer to be
"delete everything and redeploy by hand."

Bort is intentionally a small Go CLI today, but the user experience is product
shaped:

- **app-first status** instead of a wall of Docker trivia.
- **copy-paste fixes** for missing env values and data-store decisions.
- **dry-run by default** so discovery, planning, validation, cleanup inventory,
  rollback, and commit planning are safe to run repeatedly.
- **explicit live mode** only after gates are clear and target credentials exist.
- **private local artifacts** so migration bundles, state, and target API keys stay
  on the server.
- **safe cleanup** that inventories leftovers and only applies narrow metadata
  cleanup after a database backup, plus a separate confirmed source purge path
  for post-acceptance Coolify leftovers.

## Why Bort exists

Self-hosted PaaS tools make deployment easy until you need to leave one. A real
move has more surface area than "copy the compose file":

- the UI may not show the same truth as Docker on the host;
- env files and generated platform values are easy to mix up;
- databases and bind mounts need migration strategy, not vibes;
- domains need health checks, observation windows, and rollback;
- source-control integrations are platform credentials, not portable app config;
- old platform containers, volumes, networks, and proxy metadata should not be
  pruned casually.

Bort treats migration as an operation with gates. It should tell you what is safe,
what is blocked, and what to do next without making you reverse-engineer every
Traefik label, Docker network, bind mount, or platform database row yourself.

## What works today

Bort is at the local-first same-VPS migration stage.

- Scan local Docker, the Coolify API, or the source server's Docker state.
- Export a private migration bundle with compose, env, routes, storage, topology,
  source-control metadata, and a per-app runbook.
- Validate bundles for compose shape, portability risks, routes, and secret
  handling.
- Show a linear status view with per-app health, attention items, and `fix:`
  commands.
- Persist one explicitly selected current run in `.bort/state.json` so live apply,
  acceptance, and cleanup operate on the run you reviewed instead of guessing by
  file modification time.
- Record env answers and data-store strategies in `.bort/state.json` so resolved
  issues disappear from the next run.
- Keep each source-created run self-contained under `.bort/runs/<name>` with its
  private manifest, bundle, plans, progress, and apply ledger.
- Prepare Dokploy resources in dry-run form, then create/reuse projects and compose
  apps in explicit live mode.
- Upload sanitized env and deploy the captured raw compose/image snapshot without
  requiring a Dokploy GitHub App.
- Move supported local state with logical dump/restore or stopped-copy paths.
- Cut traffic over with health checks, observation, rollback planning, and an
  apply ledger so interrupted runs can resume or attach.
- Commit acceptance by stopping source app containers only after a successful live
  apply.
- Run non-destructive cleanup inventory and, when explicitly applied, remove only
  safe empty zero-domain Dokploy metadata after backing up the Dokploy database.
- Dry-run a source purge, then remove eligible source containers and networks
  only with an explicit scope and confirmation phrase. When named volumes or host
  paths are present, every listed source resource must instead be removed manually
  before Bort verifies the completed purge.

Not implemented yet:

- Automatic creation or migration of Dokploy/Coolify source connections such as
  GitHub Apps, deploy keys, and webhooks.
- Continuous volume delta sync.
- Database replication adapters beyond the current local dump/restore and
  stopped-copy paths.
- Automatic source image pruning. `cleanup purge` intentionally leaves Docker
  images alone because image tags/layers can be shared with target workloads.
- Reverse and third-party platform adapters listed in the roadmap below.

## Quick start

Install from Homebrew after the first tagged release:

```sh
brew install aikins01/tap/bort
```

Or install from source with Go:

```sh
go install github.com/aikins01/bort/cmd/bort@latest
```

Build the CLI:

```sh
mkdir -p bin
go build -o bin/bort ./cmd/bort
```

The examples below use an installed `bort`; substitute `./bin/bort` when using
the local build.

For a guided same-VPS Coolify → Dokploy migration, run Bort from the source
server in a terminal:

```sh
sudo bort
```

`bort` is the normal migration cockpit. It starts guided setup when needed and
otherwise resumes the current run. Review each app, follow the shown `fix:` and
`next:` guidance, and rerun `sudo bort` until the cockpit shows `READY`. Target
setup is offered inline when credentials are needed.

Use one privilege model for the whole lifecycle. Same-VPS discovery, state copy,
proxy cutover, source retirement, and purge need host-level Docker and filesystem
access, so the examples consistently use `sudo`. Bort keeps `.bort` private to
that OS identity (`0700` directories and `0600` files) and preserves the `sudo`
prefix in copy-paste guidance. If your regular account already has all required
Docker and source-path access, omit `sudo` from the first command and every later
lifecycle command instead; do not switch identities within one workspace.

The noninteractive start command performs discovery, exports a private bundle,
creates all run artifacts, and selects the new run in one step:

```sh
sudo bort migrate --source coolify-local
sudo bort
sudo bort migrate --live
sudo bort commit --apply
sudo bort cleanup
```

The lifecycle is deliberately explicit:

1. `sudo bort` or `sudo bort migrate --source ...` starts or resumes a reviewed
   dry-run.
2. `sudo bort` shows setup blockers and records interactive fixes.
3. `sudo bort migrate --live` applies only the selected reviewed run.
4. `sudo bort commit --apply` accepts the target and retires source app
   containers.
5. `sudo bort cleanup` audits leftovers; optional apply and purge commands remain
   separately gated.

For an existing manifest, use the same consolidated path:

```sh
sudo bort migrate --manifest manifest.json
```

Source and manifest starts create a self-contained directory under
`.bort/runs/<name>` containing the private manifest, exported bundle, reviewed
plans, progress, and apply ledger. `.bort/state.json` stores the current run along
with recorded env/data decisions and target credentials. Pass `--run <name>` to
select another run explicitly. Mutating commands never choose a run by mtime.

When Bort asks for missing values or data decisions, use the fix commands it
prints:

```sh
sudo bort env demo-app API_TOKEN=secret DATABASE_URL=postgres://...
sudo bort data demo-app postgres --migrate
sudo bort
```

After you accept the target, retire the source app stack and audit leftovers:

```sh
sudo bort commit --apply
sudo bort cleanup
sudo bort cleanup --apply
sudo bort cleanup purge
sudo bort cleanup purge --apply --app demo-app --confirm "purge <run-name>"
```

`cleanup` is the non-destructive cleanup surface. `cleanup --apply` is
metadata-only: it does not remove source containers, volumes, networks,
source-control credentials, or Dokploy target apps.

`cleanup purge` is the destructive source cleanup surface. It is still dry-run by
default, and `--apply` refuses to run unless you pass an explicit scope
(`--app`, `--project`, or `--all-apps`), the run has a successful live apply
ledger, the target has been accepted with `sudo bort commit --apply`, and you provide
the confirmation phrase shown in the dry-run output. It writes a private
purge-plan backup before deleting eligible source containers and source networks
by their exact inspected Docker IDs. Bort never deletes named volumes or host
source paths. If either is included, the dry run requires manual removal of every
listed container, volume, network, and path; `--apply` only verifies that the
whole scope remains absent instead of combining absence checks with automatic
deletion. Verification-only purges do not set `PurgedAt`, because Bort cannot
lock external tools out of recreating an absent resource. `--all-apps`
covers non-platform apps by default; platform-role leftovers such as Coolify
source/proxy resources require `--include-platform`. It does not remove
source-control credentials, target Dokploy resources, or Docker images. A full
`--all-apps` purge marks the migration complete; app- or project-scoped purges do
not claim that the whole run is finished.

## Acceptance trace

The automated lifecycle trace uses a disposable workspace, fake Dokploy HTTP
API, and fake Docker executable. It creates a self-contained run, performs live
apply, verifies `TARGET LIVE`, commits, verifies `COMMITTED`, inventories cleanup,
applies a confirmed all-app purge, and verifies `COMPLETE`:

```sh
go test ./internal/cli -run '^TestLifecycleAcceptanceTraceWithFakeDokploy$' -v
```

Real container, volume, network, bind-path, and proxy behavior still needs a
throwaway same-VPS host. Never run this destructive acceptance pass on a host
containing data you intend to keep. On a fresh VM or restorable snapshot:

1. Install Coolify and Dokploy, then deploy one stateless disposable Coolify app
   with a route, source network, and non-secret test env value.
2. Record the source container, network, and target project names before migration.
3. Run the complete lifecycle under one identity:

   ```sh
   run="bort-acceptance-$(date +%Y%m%d-%H%M%S)"
   sudo bort migrate --source coolify-local --run "$run"
   sudo bort
   sudo bort migrate --live --run "$run"
   sudo bort status --run "$run"
   sudo bort commit --apply --run "$run"
   sudo bort cleanup --run "$run"
   sudo bort cleanup purge --run "$run" --all-apps
   ```

   Review the purge plan and verify the target still serves the disposable
   workload before applying it:

   ```sh
   sudo bort cleanup purge --apply --run "$run" --all-apps --confirm "purge $run"
   sudo bort status --run "$run"
   ```

4. Confirm the target app remains healthy, `run.json` contains all three
   lifecycle timestamps, the selected source container and network are absent,
   and target resources plus source-control credentials were not removed.
5. Restore the clean snapshot and repeat with a disposable app that has a named
   volume and bind mount under a disposable directory. After the purge dry run,
   manually remove every listed source resource. Confirm `--apply` performs only
   absence verification, leaves the run committed rather than complete, and does
   not delete any replacement resource introduced before verification.
6. Destroy the VM or restore the snapshot after saving only the redacted command
   output and lifecycle metadata needed for release evidence.

## Safety model

Bort is designed for production boxes where the safest default is "look first."

- **dry-run first:** source setup, plan, validate, prepare, sync, cutover,
  rollback, commit, cleanup, and purge planning are non-mutating by default.
- **explicit live path:** target creation and traffic movement happen only through
  `sudo bort migrate --live` after setup blockers are clear. Live mode never creates a
  run implicitly and rejects source/planning flags.
- **explicit run ownership:** `.bort/state.json` names the current reviewed run.
  `--run` overrides it explicitly, and mutating commands never infer a destructive
  target from whichever `run.json` has the newest mtime.
- **immutable live plans:** after live execution begins, Bort will not regenerate
  that run's reviewed plan or reorder its apply steps. Create a new run when the
  plan itself must change.
- **serialized run mutation:** planning, live apply, target acceptance, cleanup
  apply, and purge apply share a per-run operation lock. Live apply also writes a
  separate attachable apply lock and `.bort/runs/<name>/applied.json`, so reruns
  can attach or resume without racing another mutation or replaying completed
  steps blindly.
- **persisted lifecycle:** `run.json` records when the target went live, when the
  target was accepted, and when a full source purge completed. Older runs remain
  compatible through apply-ledger inference.
- **private files:** migration bundles, state, env, and target credentials are local
  artifacts with private permissions.
- **no token flags:** Coolify API tokens are read from `BORT_COOLIFY_TOKEN`, not from
  command-line arguments.
- **sanitized target input:** Bort strips source-platform values that should not be
  replayed in Dokploy, including `COOLIFY_*`, `SOURCE_COMMIT`, Coolify labels,
  Traefik labels, and Caddy labels.
- **reviewed service magic:** `SERVICE_URL_*`, `SERVICE_FQDN_*`, and
  `SERVICE_NAME_*` are preserved because some Coolify service stacks depend on
  them, but they are surfaced for review.
- **metadata-only source control:** Bort records repository, branch, provider, source
  type, source ID, and deploy-key hints, but it does not copy GitHub App, deploy-key,
  or webhook credentials into the target platform.
- **narrow cleanup:** `cleanup --apply` backs up the Dokploy database and only deletes
  stale Dokploy platform metadata records named `coolify-proxy`, `proxy`, or
  `source` when the project is empty and still has zero domains.
- **separate purge:** destructive source deletion lives under `cleanup purge`,
  never under regular app entrypoints or metadata cleanup. `--apply` requires a
  selected `--app`, `--project`, or `--all-apps` scope, a successful live apply
  ledger, a committed target, and the exact `purge <run-name>` confirmation
  phrase. Bort removes identified containers and networks only when the scope has
  no absence-only prerequisite. A scope containing named volumes or host paths
  requires manual removal of every listed source resource, which Bort verifies
  remains absent without performing destructive operations or recording durable
  lifecycle completion.
- **rollback stays inspectable:** `sudo bort rollback` prints the selected run's stored
  rollback plan. Executable rollback is not implemented yet.

## Product surface

The primary product follows one lifecycle:

```text
sudo bort                       # start or resume the current run and review fixes
sudo bort migrate --live        # apply only the reviewed current run
sudo bort rollback              # inspect the current run's source rollback plan
sudo bort commit --apply        # accept the target and retire source containers
sudo bort cleanup               # audit leftovers; --apply removes safe metadata only
sudo bort cleanup purge         # review source purge and manual prerequisites
```

Setup and automation commands remain available for noninteractive use and future
source/target adapters:

```text
sudo bort migrate --source <adapter>       # scan, export, and create a current run
sudo bort migrate --manifest <path>        # create a current run from a manifest
sudo bort migrate --bundle <path>          # create or refresh an advanced bundle run
sudo bort env <app> KEY=value ...          # record env values noninteractively
sudo bort data <app> <store> --migrate|--recreate|--managed    # record a data-store strategy
sudo bort init-target dokploy              # bootstrap target credentials explicitly
sudo bort status                           # compatibility alias for the current cockpit
sudo bort next                             # compatibility helper for one next action
```

The artifact pipeline is also retained for inspection, scripting, compatibility,
and adapter development. Every command below is local and dry-run only, so it can
run unprivileged when its input and output paths are accessible:

```text
bort scan      # extract source platform state
bort plan      # classify apps and topology from a manifest
bort export    # write an inspectable private migration bundle
bort validate  # validate compose, env, routes, storage, and portability
bort prepare   # plan target resources before creating anything
bort sync      # plan state copy or replication work
bort cutover   # plan traffic movement and rollback windows
```

## Roadmap and status

Bort should become the migration layer for self-hosted PaaS users, not just a
one-way script. The practical roadmap starts with platforms that expose enough
Docker, Compose, Swarm, CLI, or API state to make a trustworthy migration plan.

| Lane | Status | Why it is possible | Notes |
| --- | --- | --- | --- |
| Coolify → Dokploy, same VPS | In progress now | Both platforms run Docker apps with env, domains, volumes, databases, and Traefik-facing routes; local Docker state can verify what the UI/API omits. | Current primary path. |
| Dokploy → Coolify, same VPS | Next | Dokploy apps are Docker/Compose/Swarm-shaped and can be inventoried from the host plus Dokploy metadata; Coolify can recreate apps, databases, env, and domains. | Needs a Dokploy source scanner and Coolify target applier. |
| Coolify → Dokploy, cross-server | Planned | The bundle already separates source inventory from target plans; remote transfer can build on database dumps, stopped copies, rsync, and registry/image pulls. | Intended for moving from an old Coolify VPS to a fresh Dokploy VPS. Needs transfer orchestration and stronger rollback boundaries. |
| Dokploy → Coolify, cross-server | Planned | The same bundle/transfer shape can run in reverse once Dokploy scanning and Coolify apply exist. | Intended for teams standardizing back on Coolify or moving workloads between VPS providers. |
| Other cross-server PaaS moves | Research track | Docker images, compose files, env, domains, volumes, and database dumps are portable when the source platform exposes enough state. | Bort should only graduate these when it can preserve rollback and explain unsupported resources clearly. |
| Docker Compose / plain Docker → Coolify or Dokploy | Planned | Bort already scans Docker resources and exports compose-shaped bundles. | Best when compose files, labels, env files, and named volumes are discoverable. |
| CapRover ↔ Coolify/Dokploy | Research track | CapRover is Docker Swarm-based with app env, domains, volumes, and a CLI/API surface. | Simple apps are plausible; `captain-definition`, one-click apps, and weaker Compose fidelity make complex stacks harder. |
| Dokku ↔ Coolify/Dokploy | Research track | Dokku has CLI-accessible apps, env, domains, proxy config, plugins, and database services, and many apps are buildpack or Dockerfile based. | Plugin diversity means Bort will need per-service confidence levels. |
| Easypanel / Portainer stacks → Coolify/Dokploy | Research track | These tools manage Docker or Compose-like stacks and usually expose app/stack metadata. | Feasibility depends on API coverage for env, domains, volumes, and secrets. |
| Kubernetes-backed PaaS and hosted platforms | Later | Containers, env, services, ingress, and persistent volumes can be translated in principle. | Higher variance; not the first target for safe same-VPS migrations. |

Near-term product priorities:

1. polish the Coolify → Dokploy same-VPS path until the default `bort` flow feels
   boring and safe;
2. add Dokploy source scanning and Coolify target creation for the reverse path;
3. promote the bundle format into a stable adapter contract;
4. add transfer adapters for cross-server moves;
5. graduate third-party adapters only when Bort can classify what is safe,
   blocked, or manual without hiding risk.

## Release process

Releases are automated by GitHub Actions and GoReleaser: push a `v*` tag and the
release workflow builds macOS, Linux, and Windows artifacts, Linux
`.deb`/`.rpm` packages, checksums, and updates the `aikins01/homebrew-tap` formula.

```sh
git tag v0.1.0
git push origin v0.1.0
```

The Homebrew tap update uses the `TAP_GITHUB_TOKEN` repository secret.

## Name

`bort` is named after Bortianor, a coastal town in Accra. The project is about
safe passage: moving apps from one harbor to another without dropping traffic
into the sea.
