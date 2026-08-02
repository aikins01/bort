# Migration guide

This guide covers Bort's current supported migration: moving applications from
Coolify to Dokploy on the same Linux VPS.

Start with a fresh VM or a restorable snapshot and keep an independent backup of
any data you need to retain.

## Prerequisites

Before starting, confirm that:

- Coolify is running on the source Linux VPS and its applications are healthy;
- Docker is available to the OS user that will run Bort;
- `bash`, `python3`, `openssl`, `curl`, and the standard Linux networking tools
  used by Bort's side-by-side Dokploy installer are available; if Docker
  live-restore is enabled, the host must use systemd and provide a working
  `systemctl` so the installer can reload Docker for Swarm mode;
- the host has enough free space for migration copies, database dumps, and
  backups;
- you have a terminal on the source VPS and can keep using the same working
  directory and OS user throughout the migration;
- Dokploy will run on this same VPS and use Bort's side-by-side layout, which the
  CLI calls `same-VPS shadow mode`; either it is already prepared in that layout
  or the `migrate --live` preflight will offer to install it. This layout
  prevents Dokploy's proxy from taking ports 80/443 before Bort switches traffic;
- the VPS has a current provider snapshot or another independently tested
  recovery path.

Bort publishes binaries for multiple operating systems, but a complete same-VPS
move depends on Linux, Docker, and access to protected host files. Use the macOS
and Windows builds to inspect migration files and help develop Bort, not to run a
production migration.

## Keep one workspace and one identity

Bort stores the current run, Dokploy credentials, your answers, plans, and live
progress under `.bort` in the current working directory. Running Bort from
another directory creates or reads a different workspace.

Create a dedicated directory and return to it for every migration command:

```sh
mkdir -p ~/bort-migration
cd ~/bort-migration
sudo bort
```

Use the same OS user for the whole migration. Finding apps, copying data,
switching web traffic, stopping source containers, and removing leftovers need
Docker and protected-file access, so this guide consistently uses `sudo`. Bort's
side-by-side Dokploy installer also requires root. Running without `sudo` is only
appropriate when Dokploy is already prepared and the regular account has every
required Docker and source-path permission. Choose before the first command and
do not switch users in one workspace.

The `.bort` directory contains credentials and application configuration. Bort
uses private file permissions, but you should still keep the directory out of
source control, support tickets, and shared archives.

## Start or resume

The main command opens the guided status screen:

```sh
sudo bort
```

It starts setup when the workspace has no current run and resumes the selected
run otherwise. To start Coolify discovery directly without the setup questions:

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

The guided screen shows whether each application is ready, what must be fixed,
and the next safe action. Follow the generated `fix:` commands and rerun Bort
until it shows `READY`. This means the inputs that block live apply are resolved;
it does not clear non-blocking review items.

Use the guided screen for secret values so they are not placed in command
arguments, shell history, or process and sudo audit records. The direct command
is appropriate for non-secret values and database or storage choices:

```sh
sudo bort env demo-app APP_MODE=production
sudo bort data demo-app postgres --migrate
sudo bort
```

Before continuing, review application routes, environment values, database and
storage choices, named volumes, bind mounts, source-control details, and any
remaining `next:` notes. The server has not been changed yet.

## Apply the selected run

Live apply is a separate explicit command:

```sh
sudo bort migrate --live
```

Live mode applies only the selected existing run. It does not create or refresh
a plan. Once live execution begins, that plan cannot be changed. Create a new
run if the plan itself must change.

If live apply is interrupted, Bort saves its progress. Rerun the same command to
resume:

```sh
sudo bort migrate --live
```

If another process is already applying the same run, a second `migrate --live`
command joins it and shows its progress. You can check status separately:

```sh
sudo bort status
```

Do not remove lock files when another change is running. Wait for it to finish,
or use the guided screen and `status` to decide whether live apply should be
resumed.

## Validate and accept the target

After live apply, validate the target application before accepting it:

- exercise every migrated domain and health URL;
- verify expected environment-dependent behavior;
- read and write representative database data;
- verify uploaded or persistent files;
- inspect Dokploy deployment and proxy health;
- keep observing for at least the `rollback window` recorded in the plan.

The rollback window is guidance for the operator, not an enforced timer. Bort
does not measure how long you wait or block `commit --apply` when the window has
not passed.

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

This command stops the source application containers. Run it only after checking
the target and waiting through the planned rollback window.

## Audit and clean up

After acceptance, list the remaining Dokploy records and source resources:

```sh
sudo bort cleanup
```

Ordinary cleanup and destructive source purge are separate operations. See the
[cleanup and purge guide](cleanup-and-purge.md) before applying either one.

## Recovery guide

| Situation | Safe next action |
| --- | --- |
| Bort cannot find the expected run | Return to the original working directory and original OS user. Do not create a replacement workspace accidentally. |
| Live apply was interrupted | Run `sudo bort status`, then rerun `sudo bort migrate --live` to resume from the saved progress. |
| Another change is running | Keep `status` open if useful and wait. A second live command joins an active live apply; other commands that make changes must wait. |
| The plan needs to change after live execution began | Keep the existing run as a record and create a new named run. A plan cannot change after live work starts. |
| Target validation fails before acceptance | Keep source resources available, inspect `sudo bort rollback`, and perform the required recovery manually. Automated rollback is not implemented. |
| Cleanup or purge stops partway through | Stop and inspect the command output and private backup before retrying. Follow the recovery guidance in the cleanup and purge guide. |
| `.bort` cannot be read | Do not change ownership or permissions blindly. Confirm the original working directory and OS user first, then restore the workspace from backup if necessary. |

## Current workload coverage

| Surface | Current behavior |
| --- | --- |
| Discovery | Local Coolify Docker state, local Docker, Coolify API, or an existing manifest. |
| App setup | Exports Compose or image-based app details and prepares Dokploy projects and compose applications. Unsupported Compose features remain review items. |
| Environment | Captures application values, removes Coolify-specific values that should not be copied, and keeps generated files private. |
| Routes | Plans supported routes with health checks, a waiting period, and rollback information. Missing or unsupported proxy details block the migration or require review. |
| Databases and persistent data | Supports local database dump/restore and copying while a service is stopped. Continuous syncing and other replication methods are not implemented. |
| Named volumes and bind mounts | Lists them and asks how they should be handled when manual review is required. Source purge never automatically deletes named volumes or host paths. |
| Source control | Records repository details and deploy-key hints, but does not copy, revoke, or recreate GitHub Apps, deploy keys, webhooks, or equivalent credentials. |
