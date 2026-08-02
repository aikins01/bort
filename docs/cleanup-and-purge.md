# Cleanup and purge

The normal lifecycle enters cleanup after a successful live apply and explicit
target acceptance. Cleanup has two deliberately separate surfaces:

- `cleanup` audits leftovers and can remove narrowly eligible Dokploy metadata;
- `cleanup purge` handles destructive source-resource removal after additional
  scope and confirmation gates.

Neither surface removes target Dokploy applications or source-control
credentials.

## Accept the target first

Validate the target and wait through the planned rollback window before running:

```sh
sudo bort commit --apply
```

This accepts target ownership and retires source application containers. Bort
does not currently provide executable rollback, so do not cross this boundary
until the target has been independently validated.

## Ordinary cleanup

Inventory leftovers with:

```sh
sudo bort cleanup
```

This is a dry run. It reports source containers, volumes, networks,
source-control metadata, target artifacts, and stale Dokploy platform metadata.

Apply only the safe metadata cleanup with:

```sh
sudo bort cleanup --apply
```

Before changing Dokploy metadata, Bort backs up the Dokploy database. The apply
path only removes eligible empty, zero-domain Dokploy platform records named
`coolify-proxy`, `proxy`, or `source`. It does not remove source containers,
volumes, networks, paths, credentials, Docker images, or target applications.

## Plan a source purge

Start with another dry run:

```sh
sudo bort cleanup purge
```

Select the intended destructive scope before applying:

```sh
sudo bort cleanup purge --app demo-app
sudo bort cleanup purge --project demo-project
sudo bort cleanup purge --all-apps
```

`--all-apps` covers non-platform applications by default. Platform-role
leftovers such as Coolify source or proxy resources additionally require
`--include-platform`.

Review every listed resource and use the exact apply command and confirmation
phrase emitted by the dry run. A named example is:

```sh
run="bort-acceptance-20260802-120000"
sudo bort cleanup purge --run "$run" --all-apps
sudo bort cleanup purge --apply --run "$run" --all-apps --confirm "purge $run"
```

Purge apply refuses to run unless:

- an explicit `--app`, `--project`, or `--all-apps` scope is present;
- the run has a successful live-apply ledger;
- the target was accepted with `commit --apply`;
- the exact `purge <run-name>` confirmation is supplied.

Bort writes a private purge-plan backup under `.bort/backups` before destructive
work begins.

## Automatic and verification-only purge modes

There are two purge modes based on the selected resources.

### Eligible containers and networks

When the scope has no absence-only prerequisite, Bort can remove eligible source
containers and networks by the exact Docker identities captured immediately
before apply. It rechecks identity rather than deleting a replacement resource
that happens to reuse a name.

### Named volumes or host paths

Bort never automatically deletes named volumes or host source paths. If the
selected scope contains either one, the dry run requires manual removal of every
listed source container, volume, network, and path.

After those resources have been removed manually, `cleanup purge --apply` only
verifies that the complete scope remains absent. It does not combine manual
prerequisites with automatic deletion.

Verification-only purge does not set the run's `PurgedAt` timestamp because Bort
cannot prevent an external tool from recreating a resource after the absence
check.

## Lifecycle completion

A successful automatic purge covering all run applications can mark the
migration complete. App- or project-scoped purge does not claim completion for
the whole run. A verification-only purge also leaves the run accepted rather
than recording durable lifecycle completion.

Check the resulting state with:

```sh
sudo bort status
```

## What purge preserves

Source purge does not remove:

- target Dokploy projects, compose applications, domains, or volumes;
- GitHub Apps, deploy keys, webhooks, or other source-control credentials;
- Docker images, because image layers and tags can be shared with target
  workloads;
- named volumes or host paths automatically.

## Recover from a partial purge

A destructive operation can stop after some earlier resources have already been
removed. If purge reports an incomplete result:

1. Stop issuing cleanup commands.
2. Save the complete output without publishing secrets.
3. Inspect the purge results and the private backup path printed by Bort.
4. Verify the target still serves the application.
5. Compare every selected source resource with the recorded result before
   retrying the generated scoped command.

Do not recreate a source resource under the same name and assume it is the same
object. Bort's identity checks intentionally refuse replacements where stable
identity matters.

Use the destructive scenarios in the [acceptance guide](acceptance.md) only on a
fresh VM or restorable snapshot.
