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
  cleanup after a database backup.

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
- Record env answers and data-store strategies in `.bort/state.json` so resolved
  issues disappear from the next run.
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

Not implemented yet:

- Automatic creation or migration of Dokploy/Coolify source connections such as
  GitHub Apps, deploy keys, and webhooks.
- Continuous volume delta sync.
- Database replication adapters beyond the current local dump/restore and
  stopped-copy paths.
- Destructive source purge. Non-destructive cleanup inventory and metadata-only
  Dokploy cleanup are implemented, but Bort does not delete Coolify containers,
  volumes, networks, credentials, or target apps.
- Reverse and third-party platform adapters listed in the roadmap below.

## Quick start

Build the CLI:

```sh
go build -o bin/bort ./cmd/bort
```

Run the safest same-VPS audit from the source Coolify server:

```sh
sudo bin/bort scan --source coolify-local --output manifest.json
bin/bort export --manifest manifest.json --output-dir bort-bundle
bin/bort
```

`bort` with no subcommand is the normal product surface. It resumes the latest run
when one exists, creates a dry-run from `bort-bundle` when that default bundle
exists, or prompts interactively when it needs a source and run name.

When Bort asks for missing values or data decisions, use the fix commands it
prints:

```sh
bin/bort env demo-app API_TOKEN=secret DATABASE_URL=postgres://...
bin/bort data demo-app postgres --migrate
bin/bort
```

Create or refresh a named dry-run run:

```sh
bin/bort migrate --bundle bort-bundle --target dokploy --run demo-run
bin/bort status --run demo-run
bin/bort next --run demo-run
```

Only after `bort next` says the gates are clear, bootstrap target credentials and
apply live:

```sh
bin/bort init-target dokploy --install --dokploy-url http://127.0.0.1:3030
bin/bort migrate --live --run demo-run
```

After you accept the target, retire the source app stack and audit leftovers:

```sh
bin/bort commit --apply --run demo-run
bin/bort cleanup --run demo-run
bin/bort cleanup --apply --run demo-run
```

`cleanup` is the non-destructive cleanup surface. `cleanup --apply` is
metadata-only: it does not remove source containers, volumes, networks,
source-control credentials, or Dokploy target apps.

## Safety model

Bort is designed for production boxes where the safest default is "look first."

- **dry-run first:** scan, plan, validate, prepare, sync planning, cutover planning,
  rollback planning, commit planning, and cleanup inventory are non-mutating by
  default.
- **explicit live path:** target creation and traffic movement only happen through
  `bort migrate --live` after readiness gates are clear.
- **resume instead of restart:** live apply writes an apply lock and
  `.bort/runs/<name>/applied.json`, so reruns can resume or attach without
  replaying completed steps blindly.
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

## Product surface

The main product is eight verbs:

```text
bort             # show app-first migration status and fix hints
bort env         # record env values for an app
bort data        # record a data-store strategy for an app
bort migrate     # create/update a run; --live applies after gates are clear
bort rollback    # plan rollback to the source
bort commit      # accept the target; --apply stops source app containers
bort cleanup     # inventory leftovers; --apply removes safe metadata only
bort init-target # bootstrap target credentials for live execution
```

The power-user pipeline is still available when you want each artifact explicitly:

```text
bort scan      # extract source platform state
bort plan      # classify apps and topology from a manifest
bort export    # write an inspectable private migration bundle
bort validate  # validate compose, env, routes, storage, and portability
bort prepare   # plan target resources before creating anything
bort sync      # plan state copy or replication work
bort cutover   # plan traffic movement and rollback windows
bort status    # summarize a persisted run
bort next      # show the next safe action for a run
bort cleanup   # inventory and safely apply metadata-only cleanup
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

## Name

`bort` is named after Bortianor, a coastal town in Accra. The project is about
safe passage: moving apps from one harbor to another without dropping traffic
into the sea.
