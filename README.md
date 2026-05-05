# bort

`bort` is a migration orchestrator for self-hosted PaaS platforms. The first target is moving apps from Coolify to Dokploy with live traffic cutover, data sync, health checks, and rollback.

The project is intentionally starting as a small Go CLI. The core idea is to extract a source platform into a portable manifest, recreate the app on the target platform, sync state, validate privately, then flip traffic through a migration gateway.

## Current Status

This repository is at the local-first same-VPS migration stage. Dry-run planning is still the default: Bort can scan local Docker resources, the Coolify API, or source-server Docker state; export a private migration bundle; validate topology; and produce prepare, sync, cutover, rollback, cleanup, and commit plans. When all gates are clear, explicit live mode can create Dokploy resources and move traffic on the same VPS.

Implemented now:

- `bort scan --source docker` discovers local Docker containers, routes, mounts, volumes, and networks.
- `bort scan --source coolify` exports Coolify applications, services, databases, env vars, storages, compose config, git metadata, and domains through the Coolify API.
- `bort scan --source coolify-local` is intended to run on the source Coolify server and uses local Docker state as the migration-grade inventory path.
- `bort plan` reads a manifest and prints a first migration readiness summary.
- `bort export` writes a local migration bundle with compose, env, routes, storages, topology, and a report per app.
- `bort validate` checks exported bundles for compose validity, portability risks, missing routes, and secret handling.
- `bort prepare` reads an exported bundle and prints a dry-run target preparation plan, including Dokploy render specs, without creating resources.
- `bort sync` plans state sync work without copying data.
- `bort cutover` plans route cutover, health checks, observation, and rollback windows without changing routes.
- `bort rollback` plans route rollback to the source without changing routes.
- `bort commit` plans final target acceptance and source retirement; `bort commit --apply` only stops the source app stack after a successful live apply and does not delete source resources.
- `bort cleanup` inventories stale Dokploy platform metadata, source containers, source volumes/networks, source-control credentials, and target artifacts. `bort cleanup --apply` only deletes safe zero-domain Dokploy metadata records after a Dokploy DB backup.
- `bort init-target dokploy` can bootstrap a same-VPS Dokploy admin/API key from a verified Coolify admin password, optionally installing Dokploy in shadow mode first.
- `bort` (no args) renders a linear, app-first migration status view: per-app resource health, attention items, and copy-pasteable `fix:` commands. `bort env <app> KEY=value` and `bort data <app> <store> --recreate|--migrate|--managed` record those answers in a single `.bort/state.json` so the next `bort` invocation drops the resolved issues automatically.
- `bort migrate`, `bort status`, and `bort next` keep the local-run workflow under `.bort/runs/<name>` with persisted dry-run artifacts and concise next-step summaries. `bort migrate --live` is the explicit opt-in path that executes the prepared Dokploy steps and writes an applied-step ledger so interrupted runs can resume or attach.
- The analyzer uses Coolify/Dokploy-informed resource classification so known databases, app volumes, support resources, and platform internals become setup decisions instead of generic manual-review blockers.
- Export and apply sanitize source-platform compose/env details before Dokploy receives them: `COOLIFY_*`, `SOURCE_COMMIT`, Coolify labels, Traefik labels, and Caddy labels are stripped; `SERVICE_URL_*`, `SERVICE_FQDN_*`, and `SERVICE_NAME_*` are preserved but surfaced for review.
- Bort captures non-secret source-control metadata such as repository, branch, provider, source type, source ID, and deploy-key ID, but does not copy Coolify GitHub App, deploy-key, or webhook credentials into Dokploy.

Not implemented yet:

- Automatic creation or migration of Dokploy GitHub App/source connections.
- Database replication adapters beyond the current local logical-dump/restore and stopped-copy execution paths.
- Continuous volume delta sync.
- Destructive source purge. Bort stops source app containers during explicit commit, and `cleanup --apply` is metadata-only; deleting Coolify containers, volumes, networks, or credentials remains a future explicit workflow.

## Usage

Build the CLI:

```sh
go build -o bin/bort ./cmd/bort
```

Scan the local Docker host:

```sh
bin/bort scan --output manifest.json
```

Scan a Coolify instance:

```sh
export BORT_COOLIFY_URL=https://coolify.example.com
export BORT_COOLIFY_TOKEN=your-token-here
bin/bort scan --source coolify --output manifest.json
```

Coolify API tokens are only read from `BORT_COOLIFY_TOKEN`, not from a command-line flag, so they do not appear in the process table. Scan manifests are written with private file permissions.

Scan from the source Coolify server for a more complete migration inventory:

```sh
sudo bin/bort scan --source coolify-local --output manifest.json
```

The Coolify API scan is a safe preflight, but it is not the migration source of truth. Server-local scanning can see the actual Docker containers, images, networks, volumes, bind mounts, and labels that the API may omit.

`coolify-local` enriches Docker groups from Coolify labels when available, including resource names, resource type, project/environment, compose file paths, and whether a group looks like a migration candidate or Coolify platform support.

`bort plan` also infers topology from the manifest: Docker networks, internal dependencies such as Postgres or Redis services, first-class data stores with migration strategies, possible linked support resources, stateful volumes, risk reasons, and likely external requirements from redacted env var names such as `DATABASE_URL`, `REDIS_URL`, `MINIO_ENDPOINT`, or SMTP settings.

Review a migration plan:

```sh
bin/bort plan --manifest manifest.json --target dokploy
```

Filter a migration plan to a single app or migration role:

```sh
bin/bort plan --manifest manifest.json --app demo-web
bin/bort plan --manifest manifest.json --role candidate
bin/bort plan --manifest manifest.json --role support
```

Export an inspectable migration bundle:

```sh
bin/bort export --manifest manifest.json --output-dir bort-bundle
```

Export only one app:

```sh
bin/bort export --manifest manifest.json --app my-app --output-dir bort-bundle
```

Validate an exported bundle:

```sh
bin/bort validate --bundle bort-bundle
```

Plan target-side preparation from an exported bundle without mutating the target:

```sh
bin/bort prepare --bundle bort-bundle --target dokploy
```

`bort prepare --format json` emits a versioned dry-run contract with structured target resource specs, Dokploy-specific render specs under `targetResources.dokploy`, heuristic linked-resource candidates, and readiness gates. The app shell can be ready to create while gates still require env input, resource decisions, or manual data-store review before migration proceeds.

Dry-run plan commands can persist their text or JSON output with `--output`. When `--format json` is used, the output is a local plan artifact that later stages can consume instead of recomputing every upstream stage from the bundle:

```sh
bin/bort prepare --bundle bort-bundle --target dokploy --format json --output prepare.json
bin/bort sync --from-prepare prepare.json --format json --output sync.json
bin/bort cutover --from-prepare prepare.json --from-sync sync.json --format json --output cutover.json
bin/bort rollback --from-cutover cutover.json
bin/bort commit --from-cutover cutover.json
```

Artifact consumption is opt-in. `sync` accepts `--from-prepare`; `cutover` accepts `--from-prepare` and can also accept `--from-sync` when the matching prepare artifact is supplied; `rollback` and `commit` accept `--from-cutover`. The CLI checks artifact API versions, dry-run metadata where present, bundle and target compatibility when those flags are supplied, and `--app` filters before building the next dry-run plan. `commit --from-cutover` uses the cutover artifact rollback window unless `--rollback-window` is supplied explicitly.

For a simpler local-first loop, `bort migrate` creates a named dry-run run under `.bort/runs/<name>` and writes the same JSON artifacts plus `run.json` metadata:

```sh
bin/bort migrate --bundle bort-bundle --target dokploy --run demo-run
bin/bort status --run demo-run
bin/bort next --run demo-run
```

When `bort next` reports that all gates are clear, live execution is still opt-in. Bort first needs target credentials in `.bort/state.json`; `init-target` can create them from a verified Coolify admin password, and `--install` installs Dokploy in same-VPS shadow mode on a non-public port before bootstrapping the API key:

```sh
bin/bort init-target dokploy --dokploy-url http://127.0.0.1:3030
bin/bort init-target dokploy --install --dokploy-url http://127.0.0.1:3030
```

The API key is written to `.bort/state.json` with private file permissions and is not printed. After that, the explicit live command is:

```sh
bin/bort migrate --live --run demo-run
```

Live apply uses the same persisted plan artifacts. It creates/reuses Dokploy projects and compose apps, uploads sanitized env, deploys the captured raw compose/image snapshot, runs planned state moves, attaches domains, and swaps the Coolify/Dokploy proxy only when routes require ports 80/443. It writes `.bort/runs/<name>/applied.json` and an apply lock so reruns can resume or attach instead of starting over.

Running `bin/bort` with no subcommand is the linear path. It resumes the latest local run when one exists, creates a new dry-run from `bort-bundle` when that default bundle exists, or prompts for source, run name, and manifest path when running interactively with no bundle. The output is an app-first status report (one block per app: resource health, what needs attention, and the exact `bort env`/`bort data` snippet that fixes each issue). Power-user subcommands are available through `bin/bort help --advanced`.

Recorded user answers live in `.bort/state.json` (mode 0600). Two verbs write to it:

```sh
bin/bort env <app> KEY=value [KEY=value ...]
bin/bort data <app> <store> --recreate|--migrate|--managed
```

The next `bort` run merges those values into the per-app private env files in the bundle before scanning, and overrides recorded data store strategies in-memory, so the resolved issues drop out of the linear view. Values are written to private 0600 files only and are never printed back.

The default run workflow does not create Dokploy resources, copy state, change routes, commit ownership, or clean up source resources. It recomputes local dry-run plans from the exported bundle, persists `prepare.json`, `sync.json`, `cutover.json`, `rollback.json`, `commit.json`, grouped `decisions.json`, and local-only `progress.json`, then prints concise app, route, state-sync, decision, and next-safe-step summaries. `blocked` and `needs_input` gates outrank manual decisions; live execution only happens through `bort migrate --live` after those gates are clear and target credentials exist.

`bort` is designed to run on the same server as the source PaaS, so env values can be read directly from disk; the older redacted/include env-mode prompt has been retired. The power-user `bort scan --include-env-values` and `bort export --include-env-values` flags still exist for scripted manifest workflows.

Exported bundles are local artifacts with private directory and file permissions. When raw and resolved Coolify compose are both present, `bort export` uses raw compose; resolved compose is skipped unless it was explicitly included in the manifest, because it may contain interpolated secret values. Generated Docker bundles write service-specific env examples such as `.env.web.example` to avoid collisions between services. Each app bundle also includes `topology.json`, a machine-readable summary of networks, dependencies, external requirements, possible linked support resources, data stores, stateful volumes, routes, source-control metadata, and risk reasons for later prepare/sync/cutover steps. The companion `migration-runbook.md` turns the same topology into a manual checklist for routes, environment, data stores, state sync, validation, cutover, source control, and rollback readiness.

Compose and env sanitization is conservative. Bort strips source-platform routing and generated values that should not be replayed in Dokploy (`COOLIFY_*`, `SOURCE_COMMIT`, `coolify.*`, `traefik.*`, `caddy`, `caddy_*`, and `caddy.*`). It keeps normal app env and preserves Coolify service-stack magic values such as `SERVICE_URL_*`, `SERVICE_FQDN_*`, and `SERVICE_NAME_*`, but marks them for review because they may need to change after Dokploy routes exist.

Source-control handling is metadata-only. Coolify GitHub App connections, deploy keys, and webhook secrets are platform credentials, not portable app config. Bort records repository/branch/provider/auth hints for reports and actions, deploys the current raw compose/image snapshot without requiring those credentials, and tells you to connect or reuse a Dokploy source later only if future Git deploys are wanted.

Cleanup is also dry-run-first:

```sh
bin/bort cleanup --run demo-run
bin/bort cleanup --apply --run demo-run
```

The dry run lists safe Dokploy metadata candidates, source containers, source volumes and bind mounts, source Docker networks, source-control credentials, and target artifacts. `cleanup --apply` only touches Dokploy metadata records named `coolify-proxy`, `proxy`, or `source` when they still have zero attached domains; it backs up the Dokploy database first and rechecks the zero-domain condition in the database before deleting. It does not remove Coolify containers, volumes, networks, source-control credentials, or Dokploy target apps.

For the current safe audit loop, run only read-only/local commands:

```sh
bin/bort scan --source coolify --output coolify-manifest.json
bin/bort plan --manifest coolify-manifest.json --target dokploy
bin/bort export --manifest coolify-manifest.json --output-dir bort-bundle
bin/bort                                               # linear app-first status
bin/bort env api API_TOKEN=secret DB_URL=postgres://...  # record env values
bin/bort data api postgres --migrate                   # record data strategy
bin/bort                                               # rerun; resolved issues drop out
bin/bort validate --bundle bort-bundle
bin/bort prepare --bundle bort-bundle --target dokploy
bin/bort sync --bundle bort-bundle --target dokploy
bin/bort cutover --bundle bort-bundle --target dokploy
bin/bort rollback --bundle bort-bundle --target dokploy
bin/bort commit --bundle bort-bundle --target dokploy
bin/bort migrate --bundle bort-bundle --target dokploy --run audit
bin/bort status --run audit
bin/bort next --run audit
bin/bort cleanup --run audit                            # inventory only
```

For same-VPS execution after the audit is green:

```sh
bin/bort init-target dokploy --dokploy-url http://127.0.0.1:3030
bin/bort migrate --live --run audit
bin/bort commit --apply --run audit      # stop source app containers after acceptance
bin/bort cleanup --run audit             # inventory leftovers
bin/bort cleanup --apply --run audit     # metadata-only cleanup after DB backup
```

## Direction

The primary user surface is eight verbs:

```text
bort             # scan and show app-first migration status with fix hints
bort env         # record env values for an app in .bort/state.json
bort data        # record a data store strategy in .bort/state.json
bort migrate     # create/update a run; --live applies after gates are clear
bort rollback    # plan a rollback to the source
bort commit      # plan final target acceptance; --apply stops source app containers
bort cleanup     # inventory leftovers; --apply removes safe stale Dokploy metadata only
bort init-target # bootstrap Dokploy credentials for live execution
```

The power-user pipeline behind it is:

```text
bort scan      # extract source platform state
bort plan      # classify apps as green/yellow/red
bort export    # write an inspectable local migration bundle
bort validate  # validate exported compose, env, routes, and storage
bort prepare   # plan target resources privately before creating anything
bort sync      # plan state copy or replication work
bort cutover   # plan traffic movement through the migration gateway
bort status    # summarize a persisted local run
bort next      # show the next safe local action for a run
bort cleanup   # inventory and safely apply metadata-only cleanup
```

## Name

`bort` is named after Bortianor, a coastal town in Accra. The project is about safe passage: moving apps from one harbor to another without dropping traffic into the sea.
