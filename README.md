# bort

`bort` is a migration orchestrator for self-hosted PaaS platforms. The first target is moving apps from Coolify to Dokploy with live traffic cutover, data sync, health checks, and rollback.

The project is intentionally starting as a small Go CLI. The core idea is to extract a source platform into a portable manifest, recreate the app on the target platform, sync state, validate privately, then flip traffic through a migration gateway.

## Current Status

This repository is at the foundation stage. It can scan local Docker resources or the Coolify API and produce a portable manifest that later target adapters, sync strategies, and gateway cutovers will use.

Implemented now:

- `bort scan --source docker` discovers local Docker containers, routes, mounts, volumes, and networks.
- `bort scan --source coolify` exports Coolify applications, services, databases, env vars, storages, compose config, git metadata, and domains through the Coolify API.
- `bort plan` reads a manifest and prints a first migration readiness summary.
- `bort export` writes a local migration bundle with compose, env, routes, storages, and a report per app.
- `bort validate` checks exported bundles for compose validity, portability risks, missing routes, and secret handling.
- Source/target/gateway/sync/state packages define the shape for the live migration engine.

Not implemented yet:

- Dokploy resource creation.
- Migration gateway installation.
- Database replication adapters.
- Volume delta sync and final cutover.

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

Review a migration plan:

```sh
bin/bort plan --manifest manifest.json --target dokploy
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

By default, environment variable values are redacted. If you need a full migration manifest for a trusted local workflow, opt in explicitly:

```sh
bin/bort scan --include-env-values --output manifest.json
```

## Direction

The intended migration flow is:

```text
bort scan      # extract source platform state
bort plan      # classify apps as green/yellow/red
bort export    # write an inspectable local migration bundle
bort validate  # validate exported compose, env, routes, and storage
bort prepare   # create target resources privately
bort sync      # copy or replicate state
bort cutover   # flip traffic through the migration gateway
bort rollback  # route traffic back to the source app
bort commit    # hand ownership to the target platform
```

## Name

`bort` is named after Bortianor, a coastal town in Accra. The project is about safe passage: moving apps from one harbor to another without dropping traffic into the sea.
