# Lifecycle acceptance

This guide is for maintainers validating Bort releases. It includes an automated
trace with fakes and a destructive disposable-host checklist. The fake trace is
not evidence that real Docker, proxy, volume, or filesystem behavior passed.

No production-host acceptance should be inferred until a redacted result from
the disposable-host checklist has been recorded for the release candidate.

## Automated lifecycle trace

The automated trace uses a disposable workspace, fake Dokploy HTTP API, and fake
Docker executable. It creates a self-contained run, performs live apply, verifies
`TARGET LIVE`, commits, verifies `COMMITTED`, inventories cleanup, applies a
confirmed all-app purge, and verifies `COMPLETE`:

```sh
go test ./internal/cli -run '^TestLifecycleAcceptanceTraceWithFakeDokploy$' -v
```

Run the complete repository checks separately:

```sh
make test
make vet
make build
```

## Disposable-host matrix

Use a fresh Linux VM or a restorable snapshot containing no data you intend to
keep. Exercise at least:

| Scenario | Required evidence |
| --- | --- |
| Stateless app | Domain remains healthy after live apply, acceptance, and source purge. |
| Database | Representative data survives the selected migration strategy and remains readable and writable on the target. |
| Named volume | Dry run requires manual completion; apply verifies absence without deleting resources automatically or recording completion. |
| Bind mount | Host path receives the same absence-only treatment and protected paths are refused. |
| Interrupted live apply | Rerun resumes from the apply ledger without replaying completed work blindly. |
| Concurrent live invocation | The second process attaches to progress while status remains readable. |
| Automatic purge | Exact source container and network identities are removed while target resources and credentials remain. |
| Replacement identity | A resource recreated under a reused name is not mistaken for the originally reviewed object. |

## Disposable-host procedure

1. Install Coolify, then deploy one stateless disposable Coolify app with a
   route, source network, and non-secret test environment value. Do not perform a
   normal Dokploy installation alongside the active Coolify proxy. During Bort's
   guided setup, use its same-VPS shadow installation so Dokploy's edge proxy is
   prepared without taking ports 80/443 before cutover.
2. Record the source container, network, and target project names before
   migration.
3. In a dedicated working directory, run the complete lifecycle under one
   identity:

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

4. Review the purge plan and verify the target still serves the disposable
   workload before applying it:

   ```sh
   sudo bort cleanup purge --apply --run "$run" --all-apps --confirm "purge $run"
   sudo bort status --run "$run"
   ```

5. Confirm that the target application remains healthy, `run.json` records live
   apply and acceptance, the selected non-platform source container and network
   are absent, and target resources plus source-control credentials remain. A
   real `coolify-local` run includes platform-role applications that `--all-apps`
   excludes unless `--include-platform` is supplied, so this scoped purge must
   not record `PurgedAt` or claim whole-run completion.
6. Restore the clean snapshot and repeat with a disposable app containing a
   named volume and a bind mount under a disposable directory. After the purge
   dry run, manually remove every listed source resource. Confirm that `--apply`
   performs only absence verification, leaves the run accepted rather than
   complete, and does not delete a replacement resource introduced before
   verification.
7. Repeat once with an interrupted live apply and once with two concurrent live
   invocations. Confirm that resume, attachment, and read-only status behave as
   documented.
8. Destroy the VM or restore its snapshot after retaining only the redacted
   evidence needed for the release.

## Evidence handling

Retain:

- Bort version and release candidate commit;
- host OS, architecture, Docker version, Coolify version, and Dokploy version;
- redacted command output and lifecycle status;
- the scenario matrix result;
- confirmation that the VM was destroyed or restored.

Do not retain or publish:

- `.bort/state.json`;
- exported environment values;
- Coolify or Dokploy tokens;
- database dumps or application data;
- unredacted manifests, bundles, apply ledgers, or purge backups.
