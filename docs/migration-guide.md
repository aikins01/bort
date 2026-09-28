# Migration guide

This guide covers Bort's current supported migration: moving applications from
Coolify to Dokploy on the same Linux VPS.

Start with a fresh VM or a restorable snapshot and keep an independent backup of
any data you need to retain.

## Prerequisites

Before starting, confirm that:

- Coolify is running on the source Linux VPS and its applications are healthy;
- Docker is available to the OS user that will run Bort through the system
  socket at `/var/run/docker.sock`; unset `DOCKER_CONTEXT`,
  `DOCKER_TLS_VERIFY`, and `DOCKER_CERT_PATH`, and leave `DOCKER_HOST` unset or
  set it exactly to `unix:///var/run/docker.sock`; rootless, SSH, TCP, and other
  Docker targets are refused so discovery and mutation cannot address different
  daemons;
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

### Dokploy version and authentication secret

Bort pins Dokploy v0.30.7 and its reviewed multi-platform image digest by
default. If you set `--dokploy-version`, use a release at v0.29.3 or newer and
include its independently reviewed registry digest as
`vX.Y.Z@sha256:<64 lowercase hex characters>`. Bare nondefault tags, floating
`latest` and `canary` tags, and unresolved or changed digests are refused
because a retry cannot prove which image receives the Docker socket and
secrets. Older releases cannot load the authentication key from the Docker
secret that Bort creates.

Before a fresh install, pass `--auth-secret-backup` with an absolute path on
encrypted or off-host storage, or run
`sudo BORT_DOKPLOY_AUTH_SECRET_BACKUP=/path bort ...` (sudo drops exported
variables by default). Put it
inside a mode-0700 directory owned by root or the invoking sudo user. Every
ancestor must be a real directory owned by root or that user and must not be
writable by group or others. Bort creates and fsyncs the authentication key,
opens it without following links, and supplies that validated file descriptor
to Docker. It then persists a private creation intent and labels the new Docker
secret with the matching nonce. After creation it records the exact Docker
secret ID and escrow digest in `/var/lib/bort/dokploy-auth-secret.id`. A retry
can finish the marker transition after interruption only when the pending nonce,
Docker label, and retained escrow all match; otherwise it refuses the orphaned
secret.
Retain both files. Bort mounts the key into Dokploy without putting it in the
service environment or migration workspace. Do not delete or rotate it
casually: Dokploy uses it for sessions and encrypted settings. If an existing
Dokploy service has no configured
`BETTER_AUTH_SECRET` or `BETTER_AUTH_SECRET_FILE`, Bort refuses to generate a
replacement implicitly. Recover it with the procedure below. Bort does not
print or store the secret in its workspace. Bort also refuses to create a key
when the Dokploy service is absent but an existing Dokploy database service or
volume remains. Restore the prior Dokploy service, original Bort-managed
`dokploy_auth_secret`, and `/var/lib/bort/dokploy-auth-secret.id` provenance
marker together from a consistent host backup before retrying. Bort rejects a
manually recreated secret even when its key bytes match because its Docker ID
and creation provenance differ. If no consistent backup exists, recover the
service manually outside Bort instead of rerunning the installer. Bort accepts
an existing explicit inline key only when it is exactly 64 lowercase
hexadecimal characters. Bort cannot prove how an existing key was generated.
Use the recovery procedure below for the published legacy value, and rotate any
other key whose provenance or entropy is unknown with a Dokploy-supported
procedure before retrying.

The installer has a 20-minute overall deadline. A timeout terminates its shell
process group and leaves a durable installation-recovery marker after releasing
Bort's host-wide operation lock. That marker blocks live apply, commit,
rollback, cleanup mutations, and ordinary target initialization. Inspect the
durable install state, then rerun the same `init-target --install` command under
the recovery lock to reconcile or resume it. The private marker binds the
target URL, port, address pool, digest-qualified Dokploy version, ACME email,
and escrow path, and retains the exact shell-quoted recovery command. The
cockpit and `bort next` show that command; a retry with different installation
inputs is refused. The bound inputs include the admin display name and API-key
name used by post-install bootstrap. Bort removes the marker only after the
local Dokploy service answers and passes same-host verification.

#### Recover an existing Dokploy service using the legacy secret

Dokploy releases that have no explicit authentication secret use the legacy
value `better-auth-secret-123456789`. Dokploy also derives the key for encrypted
configuration from that value. Rotating the authentication secret without
preserving the derived key can leave stored configuration unreadable.

Schedule a maintenance window first. Before taking the backup, deny every
inbound path to the Dokploy UI and API at the firewall or external load
balancer. Inspect the existing service with
`docker service inspect dokploy --format '{{json .Endpoint.Ports}}'`, then block
every published host port whose target port is 3000. Bort uses host port 3030
by default; a custom `--install-port` uses a different host port. Block any
dashboard routes on ports 80/443 as well. Keep the local Dokploy task running
until the command below performs its first stop-first forced update. Verify from
every client network that both the UI and API are unreachable, and keep this
write fence in place through all service restarts. The command replaces the
legacy-secret task, waits for active database work to settle, and only then
captures the two-factor snapshot.

With the write fence active, take recoverable disaster-recovery backups of the
complete Dokploy PostgreSQL database, `/etc/dokploy`, Docker service
configuration, and Docker secrets. Do not use the full database backup as the
routine recovery for an interrupted authentication-secret migration: Dokploy
workers can write unrelated rows while inbound HTTP is fenced, and restoring
the whole database can silently discard those writes. The procedure below
captures and compare-and-swap restores only `public.two_factor.secret` and
`public.two_factor.backup_codes`, the fields that Dokploy's migration changes.
Then preserve the legacy-derived encryption key:

```sh
(
set -eu
if ! legacy_hmac="$(printf '%s' 'dokploy:db-encryption:v1' |
  openssl dgst -sha256 -hmac 'better-auth-secret-123456789')"; then
  echo 'failed to derive the legacy Dokploy encryption key' >&2
  exit 1
fi
legacy_key="$(printf '%s\n' "$legacy_hmac" | awk '{print $NF}')"
test "${#legacy_key}" -eq 64
printf '%s\n' "$legacy_key" | grep -Eq '^[0-9a-f]{64}$'
sudo install -d -m 700 /etc/dokploy
printf '%s\n' "$legacy_key" | sudo sh -eu -c '
IFS= read -r key
[ "${#key}" -eq 64 ]
case "$key" in
  *[!0-9a-f]*) echo "derived encryption key is malformed" >&2; exit 1 ;;
esac
file=/etc/dokploy/encryption.key
tmp=$(mktemp /etc/dokploy/.encryption.key.XXXXXX)
cleanup_key_tmp() { rm -f -- "${tmp:-}"; }
trap cleanup_key_tmp EXIT
trap "exit 129" HUP
trap "exit 130" INT
trap "exit 143" TERM
if [ -e "$file" ]; then
  if [ -L "$file" ] || [ ! -f "$file" ]; then
    echo "$file is not a regular file" >&2
    exit 1
  fi
  if grep -Ev "^[0-9a-f]{64}$" "$file" | grep -q "^"; then
    echo "$file contains an invalid key line" >&2
    exit 1
  fi
  mode=$(stat -c "%a" "$file")
  owner=$(stat -c "%u:%g" "$file")
  while IFS= read -r line || [ -n "$line" ]; do
    printf "%s\n" "$line"
  done < "$file" > "$tmp"
  chmod "$mode" "$tmp"
  chown "$owner" "$tmp"
else
  chmod 600 "$tmp"
  chown root:root "$tmp"
fi
grep -Fxq "$key" "$tmp" || printf "%s\n" "$key" >> "$tmp"
sync -f "$tmp"
mv -f "$tmp" "$file"
sync -f "$(dirname "$file")"
tmp=
trap - HUP INT TERM
trap - EXIT
'
unset legacy_key
unset legacy_hmac
)
```

Each line in `/etc/dokploy/encryption.key` is a 32-byte key encoded as 64
lowercase hexadecimal characters, not a raw authentication secret. The command
validates and preserves the existing key ring, then atomically adds the
legacy-derived key only if it is absent, and syncs the replacement before the
authentication-secret migration continues. Back up this file separately.
Dokploy v0.29.12 and newer web-server backups include the key ring by default
while **Include encryption key** remains enabled; confirm that option and keep
the independent copy as well. On v0.29.5 through v0.29.11, rely on the separate
key-file backup because that backup option is unavailable.

Upgrade Dokploy to v0.29.5 or newer before running the migration. The migration
command does not exist before v0.29.3, and v0.29.5 fixes the migration process
not exiting when there are no two-factor records. Follow Dokploy's upgrade
procedure for your installed release, then inspect the running service and
confirm its image uses an explicit supported release tag:

```sh
sudo docker service inspect dokploy \
  --format '{{.Spec.TaskTemplate.ContainerSpec.Image}}'
```

Do not use the hosted `0.29.3.sh` wrapper for this recovery. It treats a
120-second migration timeout as success and can rotate the service secret while
the database transaction is still uncommitted. Run the migration command
without that timeout, and update the service authentication setting only after
the command exits successfully. Before running it, set
`DOKPLOY_AUTH_SECRET_BACKUP` to a new absolute path on mounted encrypted or
off-host storage that is backed up independently of this host. The command
creates or reuses that mode-0600 escrow, syncs it, and verifies it byte for byte
before creating the Docker secret or changing the database. Also set
`DOKPLOY_TWO_FACTOR_BEFORE` to a new absolute mode-0600 snapshot path on the
same class of protected storage. Keep both files for as long as this Dokploy
installation uses the secret.

The automated command below supports only Dokploy's default internal
`dokploy-postgres` service. It inspects the Dokploy service and refuses to run
when `DATABASE_URL` is configured or when `POSTGRES_HOST`, `POSTGRES_PORT`,
`POSTGRES_USER`, or `POSTGRES_DB` differs from the default internal connection.
For an external or otherwise custom PostgreSQL deployment, have the database
operator capture the same three-column snapshots and perform the same
compare-and-swap transaction against that exact database using its approved
connection and backup procedures. Do not substitute the local
`dokploy-postgres` container commands below.

```sh
(
  set -eu
  if sudo docker secret inspect dokploy-auth-secret >/dev/null 2>&1; then
    echo 'refusing to run migration while Docker secret dokploy-auth-secret already exists' >&2
    echo 'if an earlier attempt was interrupted before service rotation, keep the write fence active and use the two-factor compare-and-swap recovery below before removing the unused secret' >&2
    exit 1
  fi
  : "${DOKPLOY_AUTH_SECRET_BACKUP:?set DOKPLOY_AUTH_SECRET_BACKUP to an absolute path on separately backed-up encrypted or off-host storage}"
  case "$DOKPLOY_AUTH_SECRET_BACKUP" in
    /*) ;;
    *) echo 'DOKPLOY_AUTH_SECRET_BACKUP must be an absolute path' >&2; exit 1 ;;
  esac
  auth_secret_backup_dir="$(dirname -- "$DOKPLOY_AUTH_SECRET_BACKUP")"
  test -d "$auth_secret_backup_dir"
  test ! -L "$DOKPLOY_AUTH_SECRET_BACKUP"
  : "${DOKPLOY_TWO_FACTOR_BEFORE:?set DOKPLOY_TWO_FACTOR_BEFORE to a new absolute path on separately backed-up encrypted or off-host storage}"
  case "$DOKPLOY_TWO_FACTOR_BEFORE" in
    /*) ;;
    *) echo 'DOKPLOY_TWO_FACTOR_BEFORE must be an absolute path' >&2; exit 1 ;;
  esac
  two_factor_backup_dir="$(dirname -- "$DOKPLOY_TWO_FACTOR_BEFORE")"
  test -d "$two_factor_backup_dir"
  test ! -L "$DOKPLOY_TWO_FACTOR_BEFORE"
  command -v shred >/dev/null 2>&1
  umask 077
  auth_secret_tmp="$(mktemp "${TMPDIR:-/tmp}/dokploy-auth-secret.XXXXXX")"
  two_factor_tmp=
  cleanup_migration_tmp() {
    [ -z "${auth_secret_tmp:-}" ] || shred -u -- "$auth_secret_tmp"
    [ -z "${two_factor_tmp:-}" ] || rm -f -- "$two_factor_tmp"
  }
  trap cleanup_migration_tmp EXIT
  trap 'exit 129' HUP
  trap 'exit 130' INT
  trap 'exit 143' TERM
  if ! DOKPLOY_MIGRATION_ENV="$(sudo docker service inspect dokploy --format '{{range .Spec.TaskTemplate.ContainerSpec.Env}}{{println .}}{{end}}' 2>/dev/null)"; then
    echo 'could not inspect the Dokploy service database configuration; refusing authentication-secret migration' >&2
    exit 1
  fi
  require_default_database_selector() {
    selector="$1"
    expected="$2"
    count="$(printf '%s\n' "$DOKPLOY_MIGRATION_ENV" | grep -Ec "^${selector}=" || true)"
    if [ "$count" -gt 1 ]; then
      echo "Dokploy service has duplicate ${selector} settings; refusing authentication-secret migration" >&2
      exit 1
    fi
    if [ "$count" -eq 1 ]; then
      actual="$(printf '%s\n' "$DOKPLOY_MIGRATION_ENV" | sed -n "s/^${selector}=//p")"
      if [ "$actual" != "$expected" ]; then
        echo "this automated authentication-secret migration supports only Dokploy's default internal database; ${selector}=${actual} does not match ${expected}, so use a database-operator procedure that snapshots and compare-and-swap restores the same public.two_factor columns on the configured database" >&2
        exit 1
      fi
    fi
  }
  if printf '%s\n' "$DOKPLOY_MIGRATION_ENV" | grep -Eq '^DATABASE_URL='; then
    echo 'this automated authentication-secret migration supports only Dokploy with its default internal database; DATABASE_URL is configured, so use a database-operator procedure that snapshots and compare-and-swap restores the same public.two_factor columns on that external database' >&2
    exit 1
  fi
  require_default_database_selector POSTGRES_HOST dokploy-postgres
  require_default_database_selector POSTGRES_PORT 5432
  require_default_database_selector POSTGRES_USER dokploy
  require_default_database_selector POSTGRES_DB dokploy
  if [ -e "$DOKPLOY_AUTH_SECRET_BACKUP" ]; then
    test -f "$DOKPLOY_AUTH_SECRET_BACKUP"
    test "$(stat -c '%a' "$DOKPLOY_AUTH_SECRET_BACKUP")" = 600
    cat -- "$DOKPLOY_AUTH_SECRET_BACKUP" > "$auth_secret_tmp"
  else
    openssl rand -hex 32 > "$auth_secret_tmp"
    (set -C; cat -- "$auth_secret_tmp" > "$DOKPLOY_AUTH_SECRET_BACKUP")
  fi
  chmod 600 "$auth_secret_tmp"
  test "$(wc -c < "$auth_secret_tmp")" -eq 65
  grep -Eq '^[0-9a-f]{64}$' "$auth_secret_tmp"
  sync -f "$DOKPLOY_AUTH_SECRET_BACKUP"
  cmp -s -- "$auth_secret_tmp" "$DOKPLOY_AUTH_SECRET_BACKUP"
  sync -f "$auth_secret_backup_dir"
  sudo docker service update --force --update-order stop-first --detach=false dokploy
  set -- $(sudo docker ps \
    --filter label=com.docker.swarm.service.name=dokploy \
    --format '{{.ID}}')
  test "$#" -eq 1
  set -- $(sudo docker ps \
    --filter label=com.docker.swarm.service.name=dokploy-postgres \
    --format '{{.ID}}')
  test "$#" -eq 1
  dokploy_postgres_container="$1"
  database_settled=false
  settle_attempt=0
  while [ "$settle_attempt" -lt 30 ]; do
    active_sessions="$(sudo docker exec "$dokploy_postgres_container" \
      psql -U dokploy -d dokploy -X -v ON_ERROR_STOP=1 -Atc \
      "SELECT count(*) FROM pg_stat_activity WHERE datname = current_database() AND pid <> pg_backend_pid() AND state <> 'idle'")"
    if [ "$active_sessions" = 0 ]; then
      database_settled=true
      break
    fi
    settle_attempt=$((settle_attempt + 1))
    sleep 1
  done
  if [ "$database_settled" != true ]; then
    echo 'Dokploy database sessions did not settle after the stop-first task replacement; refusing to snapshot two-factor state' >&2
    exit 1
  fi
  two_factor_tmp="$(mktemp "$two_factor_backup_dir/.dokploy-two-factor-before.XXXXXX")"
  sudo docker exec "$dokploy_postgres_container" \
    psql -U dokploy -d dokploy -X -v ON_ERROR_STOP=1 \
    -c "COPY (SELECT id, secret, backup_codes FROM public.two_factor ORDER BY id) TO STDOUT WITH (FORMAT csv, HEADER true, NULL '\N', FORCE_QUOTE *)" \
    > "$two_factor_tmp"
  chmod 600 "$two_factor_tmp"
  sync -f "$two_factor_tmp"
  if [ -e "$DOKPLOY_TWO_FACTOR_BEFORE" ]; then
    test -f "$DOKPLOY_TWO_FACTOR_BEFORE"
    test "$(stat -c '%a' "$DOKPLOY_TWO_FACTOR_BEFORE")" = 600
    cmp -s -- "$two_factor_tmp" "$DOKPLOY_TWO_FACTOR_BEFORE"
    rm -f -- "$two_factor_tmp"
  else
    ln -- "$two_factor_tmp" "$DOKPLOY_TWO_FACTOR_BEFORE"
    sync -f "$two_factor_backup_dir"
    rm -f -- "$two_factor_tmp"
  fi
  two_factor_tmp=
  sudo docker secret create dokploy-auth-secret - < "$auth_secret_tmp"
  sudo docker service update \
    --secret-add source=dokploy-auth-secret,target=/run/secrets/dokploy-auth-secret \
    --update-order stop-first \
    dokploy
  set -- $(sudo docker ps \
    --filter label=com.docker.swarm.service.name=dokploy \
    --format '{{.ID}}')
  test "$#" -eq 1
  dokploy_container="$1"
  sudo docker exec \
    -e OLD_SECRET=better-auth-secret-123456789 \
    "$dokploy_container" \
    sh -c 'cd /app && NEW_SECRET="$(cat /run/secrets/dokploy-auth-secret)" pnpm run migrate-auth-secret'
  sudo docker service update \
    --env-add BETTER_AUTH_SECRET_FILE=/run/secrets/dokploy-auth-secret \
    --env-rm BETTER_AUTH_SECRET \
    --update-order stop-first \
    dokploy
  shred -u -- "$auth_secret_tmp"
  auth_secret_tmp=
  trap - HUP INT TERM
  trap - EXIT
  sudo docker service update --force --update-order stop-first dokploy
  sudo docker service ps dokploy --no-trunc
  sudo docker service logs --since 10m dokploy
)
```

Remove the maintenance fence only after the updated Dokploy task is healthy and
the authentication and stored-value checks below pass.

Keep the write fence active after any interruption. If neither
`DOKPLOY_TWO_FACTOR_BEFORE` nor the Docker secret exists, the migration did not
reach its post-quiescence snapshot; rerun the exact command under the fence. If
the snapshot exists but the Docker secret does not, rerunning first replaces
and settles the legacy task again and requires the current snapshot to match.
Once the Docker secret exists, do not rerun. Use `docker top` to confirm that no
`migrate-auth-secret` process remains. If the Dokploy service already sets
`BETTER_AUTH_SECRET_FILE`, do not remove the secret or rewrite the database;
verify login, two-factor authentication, and the stored-value checks below.

If the service still uses the legacy fallback, capture the immediate
post-interruption values to a new `DOKPLOY_TWO_FACTOR_AFTER` path. Then restore
only the two changed columns with a transaction that first proves the current
rows still equal that post-interruption snapshot. This compare-and-swap check
holds a writer-conflicting table lock from validation through restore and
refuses recovery if any two-factor row changed after the snapshot. Keep the
full database backup for disaster recovery; do not restore it for this
interruption.

```sh
(
set -eu
: "${DOKPLOY_TWO_FACTOR_BEFORE:?retain the exact pre-migration snapshot}"
: "${DOKPLOY_TWO_FACTOR_AFTER:?set a new absolute path on encrypted or off-host storage}"
case "$DOKPLOY_TWO_FACTOR_AFTER" in
  /*) ;;
  *) echo 'DOKPLOY_TWO_FACTOR_AFTER must be an absolute path' >&2; exit 1 ;;
esac
test -f "$DOKPLOY_TWO_FACTOR_BEFORE"
test "$(stat -c '%a' "$DOKPLOY_TWO_FACTOR_BEFORE")" = 600
test ! -e "$DOKPLOY_TWO_FACTOR_AFTER"
set -- $(sudo docker ps \
  --filter label=com.docker.swarm.service.name=dokploy-postgres \
  --format '{{.ID}}')
test "$#" -eq 1
dokploy_postgres_container="$1"
umask 077
after_tmp="$(mktemp "$(dirname -- "$DOKPLOY_TWO_FACTOR_AFTER")/.dokploy-two-factor-after.XXXXXX")"
cleanup_after_tmp() { rm -f -- "${after_tmp:-}"; }
trap cleanup_after_tmp EXIT
trap 'exit 129' HUP
trap 'exit 130' INT
trap 'exit 143' TERM
sudo docker exec "$dokploy_postgres_container" \
  psql -U dokploy -d dokploy -X -v ON_ERROR_STOP=1 \
  -c "COPY (SELECT id, secret, backup_codes FROM public.two_factor ORDER BY id) TO STDOUT WITH (FORMAT csv, HEADER true, NULL '\N', FORCE_QUOTE *)" \
  > "$after_tmp"
chmod 600 "$after_tmp"
sync -f "$after_tmp"
ln -- "$after_tmp" "$DOKPLOY_TWO_FACTOR_AFTER"
sync -f "$(dirname -- "$DOKPLOY_TWO_FACTOR_AFTER")"
rm -f -- "$after_tmp"
after_tmp=
trap - HUP INT TERM
trap - EXIT

(
  set -eu
  cleanup_two_factor_stage() {
    status=$?
    trap - EXIT
    sudo docker exec "$dokploy_postgres_container" \
      rm -f /tmp/bort-two-factor-before.csv /tmp/bort-two-factor-after.csv || true
    exit "$status"
  }
  trap cleanup_two_factor_stage EXIT
  sudo docker exec "$dokploy_postgres_container" \
    sh -eu -c 'test ! -e /tmp/bort-two-factor-before.csv; test ! -e /tmp/bort-two-factor-after.csv'
  sudo docker cp "$DOKPLOY_TWO_FACTOR_BEFORE" \
    "$dokploy_postgres_container:/tmp/bort-two-factor-before.csv"
  sudo docker cp "$DOKPLOY_TWO_FACTOR_AFTER" \
    "$dokploy_postgres_container:/tmp/bort-two-factor-after.csv"
  sudo docker exec -i "$dokploy_postgres_container" \
    psql -U dokploy -d dokploy -X -v ON_ERROR_STOP=1 <<'SQL'
BEGIN;
LOCK TABLE public.two_factor IN SHARE ROW EXCLUSIVE MODE;
CREATE TEMP TABLE bort_two_factor_before AS
  SELECT id, secret, backup_codes FROM public.two_factor WITH NO DATA;
CREATE TEMP TABLE bort_two_factor_after AS
  SELECT id, secret, backup_codes FROM public.two_factor WITH NO DATA;
\copy bort_two_factor_before FROM '/tmp/bort-two-factor-before.csv' WITH (FORMAT csv, HEADER true, NULL '\N')
\copy bort_two_factor_after FROM '/tmp/bort-two-factor-after.csv' WITH (FORMAT csv, HEADER true, NULL '\N')
DO $$
BEGIN
  IF EXISTS (
    SELECT 1 FROM (
      (SELECT id FROM bort_two_factor_before EXCEPT SELECT id FROM bort_two_factor_after)
      UNION ALL
      (SELECT id FROM bort_two_factor_after EXCEPT SELECT id FROM bort_two_factor_before)
    ) AS id_drift
  ) THEN
    RAISE EXCEPTION 'two-factor row identities differ between before and after snapshots';
  END IF;
  IF EXISTS (
    SELECT 1 FROM (
      (SELECT id, secret, backup_codes FROM public.two_factor
       EXCEPT SELECT id, secret, backup_codes FROM bort_two_factor_after)
      UNION ALL
      (SELECT id, secret, backup_codes FROM bort_two_factor_after
       EXCEPT SELECT id, secret, backup_codes FROM public.two_factor)
    ) AS current_drift
  ) THEN
    RAISE EXCEPTION 'current two-factor values changed after the interruption snapshot';
  END IF;
END
$$;
UPDATE public.two_factor AS current
SET secret = original.secret,
    backup_codes = original.backup_codes
FROM bort_two_factor_before AS original
WHERE current.id = original.id;
COMMIT;
SQL
)
)
```

Capture the three columns again and compare the result byte-for-byte with
`DOKPLOY_TWO_FACTOR_BEFORE`. Only after they match, remove the
`dokploy-auth-secret` mount from the still-legacy service, remove that Docker
secret, and rerun the full migration with both retained pre-migration files.
Never delete or overwrite either retained file during recovery.

On Dokploy v0.29.12 and newer, the official migration re-encrypts only Better
Auth's two-factor secret and backup-code fields. It does not re-encrypt
application, Compose, project, environment, or database environment values, nor
application build arguments and build secrets. The preserved key remains
necessary to read those existing encrypted values. Before saving or deploying
anything, inspect representative values of each kind in Dokploy and confirm they
render as plaintext rather than `enc:v1:` ciphertext. Also verify login,
two-factor authentication if enabled, service health, and logs. If any check
fails, keep the write fence active and retain the backups unchanged while you
investigate. Do not restore the full PostgreSQL backup in place: unrelated
writes may have committed after it was taken. For selective recovery, restore
the backup to an isolated database, identify the affected rows, and use a
reviewed transaction that verifies current values before updating them. Reserve
an in-place full restore for disaster recovery after accounting for all
post-backup writes. Dokploy v0.29.5 through v0.29.11 keep these configuration
fields as plaintext.

On v0.29.12 and newer, Dokploy supports `ENCRYPTION_KEY` and
`ENCRYPTION_KEY_FILE` for separating authentication and configuration
encryption. Configure one while the old secret is still active and retain the
old derived key in `/etc/dokploy/encryption.key` during the transition. Do not
use the derived hex key itself as `ENCRYPTION_KEY`; Dokploy derives another key
from that setting. Upgrade v0.29.5 through v0.29.11 before using either setting.

### Keep the Dokploy API on the local Docker host

Use the direct loopback HTTP endpoint, such as `http://127.0.0.1:3030`, for
standalone `bort init-target` and every live migration command. Include the
published service port and do not add a proxy path. Bort refuses HTTPS and
non-loopback URLs. Swarm membership cannot prove that an API on one manager
uses the local manager's Docker daemon, so a public or private network address
is not an acceptable same-host identity. This prevents a remote Dokploy API
from being combined with source-container or proxy changes on the local VPS.

### Kernels without IPVS

Dokploy's control-plane services use Docker Swarm VIP networking by default.
If the kernel explicitly reports that IPVS is disabled, Bort stops before
installing anything. On these hosts, use Dokploy's documented DNS round-robin
mode when starting Bort, for example:

```sh
sudo ENDPOINT_MODE=dnsrr bort migrate --live
sudo bort init-target --install --endpoint-mode dnsrr --auth-secret-backup /absolute/path/to/encrypted-or-off-host/dokploy-auth-secret
```

This option applies to the three control-plane services that Bort creates, not
to existing services. Bort refuses a conflicting existing endpoint mode; review
and update those services before retrying. DNSRR requires host-mode published
ports, which Bort's shadow installer already uses. Bort migrates workloads as
Docker Compose containers, not Swarm services. Other applications deployed as
Swarm services can still require IPVS or direct task addressing; see
[Dokploy's networking guide](https://docs.dokploy.com/docs/core/troubleshooting/networking).

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
required Docker and source-path permission. Before the first non-root command,
provision Bort's host-wide operation state for that account:

```sh
sudo install -d -m 700 -o "$(id -u)" -g "$(id -g)" /var/lib/bort
```

Choose before the first command and do not switch users in one workspace or for
the `/var/lib/bort` owner.

The `.bort` directory contains credentials and application configuration. Bort
uses private file permissions, but you should still keep the directory out of
source control, support tickets, and shared archives. `/var/lib/bort` contains a
transient operation lock, the fresh-install authentication-secret provenance
marker, and `dokploy-traffic-owner.json`, the durable record
that gives one migration run host-wide authority over Dokploy changes. Bort
claims it before the first live target mutation, including project or service
creation in a plan with no routes, and retains it until the run is committed or
rolled back. The record binds a one-way identifier for the organization-scoped
Dokploy API key; it never stores the key itself. Do not rotate or replace that
key during a migration or before metadata `cleanup --apply`. Preserve the owner
file with `.bort`, at the same path and under the same OS identity, through any
metadata cleanup you intend to apply. Do not restore one location without the
other.
The status screen reports `HOST OWNED`, the owning run, and its recorded
authority when a different run holds the host. New owner records also show the
absolute run path; older records may have only the run name. Open that run path
or return to its workspace and finish its commit or rollback; do not delete or
rewrite the owner file to bypass it.

A local scan records the Docker Engine ID and the exact source container IDs and
names in the reviewed run. Live apply, interrupted source cleanup, rollback,
and commit re-read the system Docker socket and refuse mutation unless all of
those identities still match. Those checks catch a replaced daemon or container;
they do not re-verify every detail of the reviewed configuration. Unstarted runs
from imported manifests, imported bundles (`--bundle`, including bundles produced
by `bort export`), remote scans, or older Bort versions remain available for
inspection and planning only; replace them with a new local scan on the source
host before live work.
A run that already performed live work is immutable and cannot be refreshed.
If such a run lacks source attestation, Bort cannot safely choose containers to
restart, stop, retire, or purge: verify Docker Engine and container identities
manually, recover traffic and writers or retire the reviewed sources outside
Bort, and preserve the run and any host-owner record as audit evidence. macOS
and Windows inspection preserves the stored lifecycle phase and shows the
state-specific Linux handoff, but cannot perform live recovery or mutation.

If a current run reports `AUTHORITY UNKNOWN` while its matching owner record is
still present, do not abandon the run or delete the owner. Inspect both sides,
manually establish source or target authority, then run the exact
`recover-authority` command shown by `bort status`. Bort binds the decision to
the owning run under both operation locks. A later retry finishes any
interrupted metadata or owner transition.

If `dokploy-traffic-owner.json` is lost after any target mutation, current runs
fail closed: Bort cannot prove that another workspace did not change the target
or shared proxy. Preserve both source and target data, inspect target projects,
environments, Compose resources, the Coolify and Dokploy proxies, and routes,
establish writer and traffic authority manually, then start a fresh migration
run. Do not recreate the owner file by hand.

Completed routed runs from before Bort persisted the immutable Dokploy origin
also appear as `AUTHORITY UNKNOWN`. Current credentials do not prove that they
address the original target, so automatic retry, commit, and rollback remain
disabled even when the old ledger says the run completed. Do not rebind the run
to the currently configured URL. Keep both copies of the data, identify the
original target from independent operator records, inspect its workloads and
routes alongside the source, and establish writer and traffic authority
manually. Retire or restore resources only after that inspection, then create a
fresh migration run for any remaining automated work.

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
until it shows `READY` or reports `MANUAL STATE`. `READY` means the inputs that
block live apply are resolved; it does not clear non-blocking review items.
`MANUAL STATE` means a historical stateful run was applied with an older plan
version that copied state into an already deployed target; Bort cannot continue
that run. Follow the displayed manual recovery guidance or create a new run.
`PLAN BLOCKED` means live apply would refuse the planned state transfer before
touching the target (for example a bind mount, a named volume shared by two
apps, or a compose file that cannot be staged). When the blocked volume belongs
to a data store, choose `bort data <app> <store> --recreate` or `--managed`,
then re-plan with `bort migrate --run <run>`. Otherwise change the source
compose and scan a new run. Bort checks this before binding the run to a
Dokploy target, so the run stays editable.

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
resume when the saved boundary is safe:

```sh
sudo bort migrate --live
```

For an app with named volumes or a Postgres database, live apply moves the
state before the Dokploy compose is deployed. Dokploy v0.30.7 cannot durably
prevent a queued or future deployment from restarting a target writer, so Bort
never copies data into a deployed target. Instead it:

1. Creates the Dokploy project, compose service, and environment, but does not
   deploy.
2. Stops the reviewed source app containers. Logical-dump databases keep
   running for `pg_dump`; volume-strategy stores stop with the app.
3. Copies each named volume into a Docker volume that Bort creates and labels
   with the run name (`bort-<run>-<app>-<service>-<hash>`). It refuses a volume
   with that name that another run owns.
4. Restores each Postgres dump by starting only the database service under a
   temporary compose project (`bort-stage-<app>-<service>-<hash>`) with the
   staging volume mounted, running `pg_restore`, and stopping it again. The
   project files live under `.bort/runs/<run>/stage/`.
5. Re-inspects the source containers after each volume copy and refuses the
   transfer if any started or ran during it. After every copy or restore it
   also refuses if any container is still attached to the staging volume.
6. Deploys the compose with each transferred volume declared as
   `external: true` pointing at the staging volume, then verifies that the
   running target mounts those exact volumes and stops the target if it does
   not.

Routed apps stay stopped from step 2 until the proxy handoff moves traffic;
plan for that downtime. Unrouted apps restart the source after step 5, so the
source and target then run with diverging copies.

A stateful app whose plan needs a bind-mount copy is refused. Same-host bind
mounts that keep their existing paths need no transfer. Do not remove real
storage from an app's manifest merely to bypass a refusal.

A named volume mounted by more than one compose service cannot be staged,
because each service mount would become a separate staging volume. Bort refuses
it before changing Dokploy or stopping the source. When the volume belongs to a
data store, choose `bort data <app> <store> --recreate` or `--managed` and
re-plan the run with `bort migrate --run <run>`. Otherwise change the source
compose so one service mounts the volume, then scan a new run.

The temporary compose project runs with only `PATH` and `HOME`, as Dokploy
deploys do, and writes its `.env` in the format the running Dokploy release
uses: raw values before v0.30.0, every `$` escaped through v0.30.2, and
`${VAR}` references kept from v0.30.3. Bort reads the version from
`settings.getDokployVersion` before pausing the source and refuses staged
restores when it is not a `vMAJOR.MINOR.PATCH` release. Because the staged
service runs under a Bort project name, a compose file or `.env` that
interpolates `COMPOSE_PROJECT_NAME` for a staged data store is refused before
anything changes; reference the app name another way and re-plan the run. A
Dokploy compose app that has `.env` creation disabled or a custom deploy
command is refused before the source pauses, because its deploy would not
interpolate the compose file the way the staged restore did. The check runs
again right before each deploy of that app, so changing those settings in
Dokploy between a completed restore and a resumed deploy also refuses; restore
them and resume.

The staged service mounts only its staging volumes plus any bind mounts under
`/docker-entrypoint-initdb.d`, read-only, so the restore runs the same init
scripts the deploy will. Those mounts must use absolute host paths; a relative
one is refused before the source pauses. Scripts that write into that
directory, or that read other bind mounts such as a `postgresql.conf`, do not
run the same way in staging; choose `--recreate` or `--managed` for such
stores. The restore waits for the final server to accept connections on
`127.0.0.1` inside the container, so the entrypoint's socket-only init server,
which runs those scripts, never receives the dump. A custom `listen_addresses`
must include that address; the official image's `*` default already does.

Runs whose plan was created by an older Bort version copied state into an
already deployed target. Live apply refuses those runs (`MANUAL STATE`). If
such a run already started a volume copy or target database restore, it remains
fail-closed: Bort cannot prove whether the target later accepted unique writes,
so it refuses automatic retry or rollback rather than overwrite either side.
Follow the authority-recovery guidance below.

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

The text and JSON inspection output state whether automatic rollback is
available and give the exact blocker when it is not. Treat unavailable as a
recovery boundary, not as a prompt to force the command.

After a successful stateless live apply, if the target does not check out,
return traffic to the source:

```sh
sudo bort rollback --live
```

The command is available only when the run transferred no database or volume
state. In that case the source applications remained running, and rollback
stops the Dokploy proxy and starts the Coolify proxy. An interrupted stateless
rollback can be re-run.

An interrupted stateful run with the current plan version resumes from its
saved progress like a stateless run. For a run applied with an older plan
version that stopped a source app, if its ledger proves that no database dump,
restore, volume copy, or traffic handoff may have started, re-running
`migrate --live` only restarts the source and records that cleanup; the run
itself remains refused.

For a historical stateful run that already completed, `rollback --live` also
refuses before changing progress, Docker, or Dokploy. Dokploy v0.30.7 has no
durable application fence:
`compose.stop` does not cancel a queued deployment or block a future manual
deployment. Preserve both sides and establish writer and traffic authority
manually. Do not infer authority from a short period with no running target
containers.

Recovery from an interrupted run whose writer authority is ambiguous is also
manual. In current runs this means a Dokploy target mutation (project or
service creation, env upload, compose deploy, gateway install, or route
activation) was interrupted or returned an ambiguous result. In ledgers written
by older Bort versions it also covers an in-place volume copy, database restore,
or handoff whose boundary cannot be proven. Bort refuses retry without mutating
either side. Staged copies and restores in current runs are not affected: rerun
`migrate --live` and the transfer restarts.

When the durable Dokploy owner still identifies the same run, finish manual
recovery through that run:

- To return to the source, first stop and fence every target writer, reverse
  target traffic, and verify that the source is healthy and authoritative. Then
  run:

  ```sh
  sudo bort recover-authority --run <run-name> --authority source \
    --confirm 'recover <run-name> as source'
  ```

  Bort records the resolution and rollback lifecycle, changes the owner to
  source authority, and releases host ownership. Target data is not copied back.

- To retain the target, first stop and fence every source writer and verify the
  target data and traffic. Then run:

  ```sh
  sudo bort recover-authority --run <run-name> --authority target \
    --confirm 'recover <run-name> as target'
  ```

  When source attestation still passes, Bort records target authority. For a
  source without an external orchestrator, `commit --apply` can then retire the
  exact reviewed containers. For Coolify, disable future deployments for the
  reviewed apps and manually retire every reviewed source app (and the source
  proxy when the cutover moved routes) instead; `commit --apply` refuses
  because Bort cannot durably fence Coolify. If the
  reviewed source daemon or containers are no longer attestable, the recovery
  command also refuses without recording the authority decision. In either
  manual-retirement case, run the second command Bort prints with
  `--source-retired` only after verifying the source remains retired. Its
  stronger confirmation marks the run committed and releases host ownership
  without mutating source resources. Automatic rollback remains unavailable
  because Bort cannot verify the manual boundary.

`recover-authority` records work that you already completed and verified. It
does not stop containers, move data, or switch traffic. It refuses a missing or
different owner and a mismatched confirmation phrase. The plain
`--authority source` and `--authority target` forms also refuse a run that has
no authority ambiguity, no unavailable automatic rollback, and no unreleased
host ownership after a definite live-apply failure. `--authority target
--source-retired` records permanent target acceptance after any successful live
apply, including stateless runs and runs whose plan never needed host
ownership. If a plan needed host ownership and the owner record is missing or
belongs to another run, keep both sides and complete recovery manually; start a
fresh run only after the old resources and traffic are safe.

Stateless rollback does not enforce the observation window displayed by
`bort rollback`; continue monitoring the source for that reviewed period. Once
rollback starts, commit and live apply remain blocked, even if rollback fails.
Use `bort rollback --live` again to finish a stateless rollback.

Stateless rollback requires confirming the recorded rollback trigger first. If
you skipped that guided review before applying, inspect
`bort rollback --run <run-name>`, then use
`bort rollback --live --run <run-name> --confirm 'rollback <run-name>'` with the
actual run name. The command records that confirmation before changing traffic.

Stateless rollback never deletes resources: the Dokploy target resources remain
on the server for later cleanup. To migrate again, create a new named run with
`bort migrate --run <new-name>` and current `--source` or `--bundle` inputs.

For a source without an external orchestrator, accepting the target retires the
exact reviewed source application containers:

```sh
sudo bort commit --apply
```

This command stops the source application containers. Run it only after checking
the target and waiting through the planned rollback window. Once source
retirement starts, automated rollback is no longer available, even if commit is
interrupted; rerun `commit --apply` to finish acceptance.

For a Coolify source, `commit --apply` refuses before source mutation because a
queued or future deployment could replace a stopped container. Disable future
deployments in Coolify, manually retire every reviewed source app and the source
proxy, and verify they remain retired. Then run the exact
`recover-authority --authority target --source-retired --confirm ...` command
Bort prints. That command records permanent target acceptance without mutating
the source.

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
| Stateless live apply was interrupted at a safe completed boundary | Run `sudo bort status`, then rerun `sudo bort migrate --live` to resume from the saved progress. |
| Live apply reports `MANUAL STATE` for a stateful run | The run was applied with an older plan version that copied state into a deployed target. The blocked run cannot continue. Complete recovery with the guidance shown by `bort status`, then create a new run. |
| A staged state transfer fails (source restarted, foreign-owned volume, attached staging volume) | That app's target was not deployed. Fix the reported cause and rerun `sudo bort migrate --live`; the transfer restarts from the beginning for that volume or database. |
| A stateful plan needs a bind-mount copy | Bort refuses before pausing the source. Re-plan with `bort migrate --run <run>`; current plans keep same-host bind mounts at their existing paths, and the run stays editable because nothing live has started. |
| A named volume is mounted by more than one compose service | Bort refuses before changing Dokploy or pausing the source. For a data store volume, choose `bort data <app> <store> --recreate` or `--managed` and re-plan with `bort migrate --run <run>`; otherwise change the source compose so one service mounts the volume and scan a new run. |
| A Dokploy target mutation (project or service creation, env upload, compose deploy, gateway install, route activation) was interrupted or ambiguous, or an older-plan ledger cannot prove writer authority after an in-place copy, restore, or handoff | Bort refuses automatic retry and rollback because Dokploy cannot durably fence the target. Inspect and preserve both sides. If the matching owner remains, establish authority manually and run the exact `recover-authority` command shown by `bort status`; otherwise complete recovery manually before starting a fresh run. |
| A deployed target fails its migrated-volume check and Bort cannot stop its containers | Bort leaves the paused source stopped and refuses automatic retry, because target containers may still be writing to the transferred state. Stop the target containers yourself, then run the exact `recover-authority` command shown by `bort status`. |
| Another change is running | Keep `status` open if useful and wait. A second live command joins an active live apply; other commands that make changes must wait. |
| The plan needs to change after live execution began | Keep the existing run as a record and create a new named run. A plan cannot change after live work starts. |
| A stateless target fails validation after successful live apply | Run `sudo bort rollback --live` to return traffic to the source. Target resources remain for cleanup. |
| A stateful target fails validation after successful live apply | Bort refuses automatic rollback because Dokploy cannot durably fence target writers. Preserve both sides, manually restore and verify source authority, then use the `recover-authority --authority source` command shown by `bort status`. |
| Stateless rollback stops partway through | Inspect `sudo bort status`, then rerun `sudo bort rollback --live`. Commit and live apply are blocked because traffic may already have returned to the source. |
| Cleanup or purge stops partway through | Stop and inspect the command output and private backup before retrying. Follow the recovery guidance in the cleanup and purge guide. |
| `.bort` cannot be read | Do not change ownership or permissions blindly. Confirm the original working directory and OS user first, then restore the workspace from backup if necessary. |

## Current workload coverage

| Surface | Current behavior |
| --- | --- |
| Discovery | Local Coolify Docker state, local Docker, Coolify API, or an existing manifest. |
| App setup | Exports Compose or image-based app details and prepares Dokploy projects and compose applications. Unsupported Compose features remain review items. |
| Environment | Captures application values, removes Coolify-specific values that should not be copied, and keeps generated files private. |
| Routes | Plans supported routes with health checks, a waiting period, and rollback information. Missing or unsupported proxy details block the migration or require review. |
| Databases and persistent data | Dumps Postgres databases and copies named volumes into Bort-owned staging volumes before the target deploys, then hands those volumes to the Dokploy compose. Bind-mount copies are not implemented. |
| Named volumes and bind mounts | Lists them and asks how they should be handled when manual review is required. Source purge never automatically deletes named volumes or host paths. |
| Source control | Records repository details and deploy-key hints, but does not copy, revoke, or recreate GitHub Apps, deploy keys, webhooks, or equivalent credentials. |
