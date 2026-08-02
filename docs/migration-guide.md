# Migration guide

This guide covers Bort's current end-to-end product path: moving applications
from Coolify to Dokploy on the same Linux VPS.

The live path still needs a recorded disposable-host acceptance run before it
should be treated as production-proven. Use a fresh VM or a restorable snapshot
for evaluation, and keep an independent host backup for any migration involving
data you need to retain.

## Prerequisites

Before starting, confirm that:

- Coolify is running on the source Linux VPS and its applications are healthy;
- Docker is available to the identity that will run Bort;
- `bash`, `python3`, `openssl`, `curl`, and the standard Linux networking tools
  used by the guided shadow-target installer are available; if Docker
  live-restore is enabled, the host must use systemd and provide a working
  `systemctl` so the installer can reload Docker for Swarm mode;
- the host has enough free space for migration copies, database dumps, and
  backups;
- you have a terminal on the source VPS and can keep using the same working
  directory and OS identity throughout the migration;
- Dokploy will run on this same VPS and is either already prepared in Bort's
  shadow-target layout or will be installed through the guided same-VPS setup
  when credentials are needed;
- the VPS has a current provider snapshot or another independently tested
  recovery path.

Bort publishes binaries for multiple operating systems, but the complete live
same-VPS path depends on Linux host state, Docker, and privileged filesystem
access. macOS and Windows builds should currently be treated as inspection and
development surfaces, not as validated production live-migration hosts.

## Keep one workspace and one identity

Bort stores its current run, target credentials, decisions, plans, and apply
ledger under `.bort` in the current working directory. Running Bort from another
directory creates or reads a different workspace.

Create a dedicated directory and return to it for every lifecycle command:

```sh
mkdir -p ~/bort-migration
cd ~/bort-migration
sudo bort
```

Use one privilege model for the whole lifecycle. Same-VPS discovery, state copy,
proxy cutover, source retirement, and purge need host-level Docker and
filesystem access, so this guide consistently uses `sudo`. The guided
shadow-target installer requires root. An unprivileged lifecycle is only
appropriate when the shadow target is already prepared and the regular account
has every required Docker and source-path permission. Choose before the first
command and do not alternate identities in one workspace.

The `.bort` directory contains credentials and application configuration. Bort
uses private file permissions, but you should still keep the directory out of
source control, support tickets, and shared archives.

## Start or resume

The normal product surface is the guided cockpit:

```sh
sudo bort
```

It starts setup when the workspace has no current run and resumes the selected
run otherwise. For a noninteractive Coolify discovery start:

```sh
sudo bort migrate --source coolify-local
sudo bort
```

For an existing manifest:

```sh
sudo bort migrate --manifest manifest.json
```

Each source or manifest start creates a self-contained run under
`.bort/runs/<name>`. Use `--run <name>` only when intentionally selecting a run
other than the workspace default.

## Review and fix

The cockpit shows each application's readiness, blocking issues, and the next
safe action. Follow the generated `fix:` commands and rerun the cockpit until it
shows `READY`.

Use the interactive cockpit for secret values so they are not placed in command
arguments, shell history, or process and sudo audit records. The noninteractive
form is appropriate for non-secret values and data strategy decisions:

```sh
sudo bort env demo-app APP_MODE=production
sudo bort data demo-app postgres --migrate
sudo bort
```

Review application routes, environment values, data-store strategies, named
volumes, bind mounts, and source-control metadata. A ready run is a reviewed
plan, not a host mutation.

## Apply the reviewed run

Live apply is a separate explicit command:

```sh
sudo bort migrate --live
```

Live mode applies only the selected existing run. It does not create or refresh
a plan. Once live execution begins, that run's reviewed plan is immutable; if
the plan itself must change, create a new run.

An interrupted live apply records progress in its apply ledger. Rerun the same
command to resume:

```sh
sudo bort migrate --live
```

If another invocation is already applying the same run, a second
`migrate --live` invocation attaches to its progress. Read-only inspection can
continue separately:

```sh
sudo bort status
```

Do not remove lock files to work around contention. Wait for the active mutation
or use the cockpit and status output to determine whether an apply should be
resumed.

## Validate and accept the target

After live apply, validate the target application before accepting it:

- exercise every migrated domain and health endpoint;
- verify expected environment-dependent behavior;
- read and write representative database data;
- verify uploaded or persistent files;
- inspect Dokploy deployment and proxy health;
- keep observing for at least the planned rollback window.

You can inspect the stored rollback plan with:

```sh
sudo bort rollback
```

Bort does not currently execute rollback. The command prints the stored plan;
any recovery action remains manual. Do not accept the target unless that manual
recovery limitation is understood.

Accepting the target retires source application containers:

```sh
sudo bort commit --apply
```

This is a lifecycle boundary, not another validation step. Run it only after the
target has been checked and the rollback window has passed.

## Audit and clean up

After acceptance, inventory remaining metadata and source resources:

```sh
sudo bort cleanup
```

Ordinary cleanup and destructive source purge are separate operations. See the
[cleanup and purge guide](cleanup-and-purge.md) before applying either one.

## Recovery guide

| Situation | Safe next action |
| --- | --- |
| The cockpit cannot find the expected run | Return to the original working directory and original OS identity. Do not create a replacement workspace accidentally. |
| Live apply was interrupted | Run `sudo bort status`, then rerun `sudo bort migrate --live` to resume from the recorded ledger. |
| Another mutation is active | Keep read-only status open if useful and wait. A second live command attaches to an active live apply; other mutations remain serialized. |
| The reviewed plan needs to change after live execution began | Preserve the existing run as evidence and create a new named run. Live plans are intentionally immutable. |
| Target validation fails before acceptance | Keep source resources available, inspect `sudo bort rollback`, and perform the required recovery manually. Automated rollback is not implemented. |
| Cleanup or purge stops partway through | Stop and inspect the command output and private backup before retrying. Follow the recovery guidance in the cleanup and purge guide. |
| `.bort` cannot be read | Do not change ownership or permissions blindly. Confirm the original working directory and identity first, then restore the workspace from backup if necessary. |

## Current workload coverage

| Surface | Current behavior |
| --- | --- |
| Discovery | Local Coolify Docker state, local Docker, Coolify API, or an existing manifest. |
| Application definitions | Exports Compose/image-shaped application state and prepares Dokploy projects and compose applications. Unsupported constructs remain review items. |
| Environment | Captures application values, strips source-platform values that should not be replayed, and keeps generated artifacts private. |
| Routes | Plans supported routes with health checks, observation, and rollback information. Missing or unsupported proxy metadata blocks or requires review. |
| Databases and persistent state | Supports the current local logical dump/restore and stopped-copy strategies. Continuous delta sync and broader replication adapters are not implemented. |
| Named volumes and bind mounts | Inventories and plans them, with explicit strategy or manual review where required. Source purge never automatically deletes named volumes or host paths. |
| Source control | Records repository metadata and deploy-key hints, but does not copy, revoke, or recreate GitHub Apps, deploy keys, webhooks, or equivalent credentials. |

For release-level validation, use the [acceptance guide](acceptance.md).
