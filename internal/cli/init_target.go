package cli

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"strings"
	"time"

	"github.com/aikins01/bort/internal/target/dokploy"
	"golang.org/x/crypto/bcrypt"
	"golang.org/x/term"
)

const (
	defaultCoolifyEnvPath     = "/data/coolify/source/.env"
	defaultCoolifyDBContainer = "coolify-db"
	defaultDokployAPIName     = "bort-cli"
	envCoolifyAdminPwd        = "BORT_COOLIFY_ADMIN_PASSWORD"
	installProgressPrefix     = "BORT_INSTALL_STEP\t"
	psqlAdminFieldSeparator   = "\t"
)

type coolifyAdmin struct {
	Email        string
	Name         string
	PasswordHash string
}

type coolifyAdminLister interface {
	listAdmins(ctx context.Context) ([]coolifyAdmin, error)
}

type initTargetDeps struct {
	lister    coolifyAdminLister
	installer dokployInstaller
	newClient func(baseURL string) *dokploy.Client
	statePath string
}

type dokployInstallOptions struct {
	HostPort  string
	AddrPool  string
	Version   string
	ACMEEmail string
}

type dokployInstaller interface {
	InstallDokploy(ctx context.Context, opts dokployInstallOptions, stdout, stderr io.Writer) error
}

type shellDokployInstaller struct{}

func runInitTarget(ctx context.Context, args []string, stdin io.Reader, stdout, stderr io.Writer) error {
	return runInitTargetWith(ctx, args, stdin, stdout, stderr, initTargetDeps{
		lister:    &coolifyDBLister{envPath: defaultCoolifyEnvPath, container: defaultCoolifyDBContainer},
		installer: shellDokployInstaller{},
		newClient: defaultDokployClient,
		statePath: defaultStatePath(),
	})
}

func defaultDokployClient(baseURL string) *dokploy.Client {
	return &dokploy.Client{
		BaseURL:    strings.TrimRight(baseURL, "/"),
		HTTPClient: &http.Client{Timeout: 30 * time.Second},
	}
}

func runInitTargetWith(ctx context.Context, args []string, stdin io.Reader, stdout, stderr io.Writer, deps initTargetDeps) error {
	fs := flag.NewFlagSet("init-target", flag.ContinueOnError)
	fs.SetOutput(stderr)

	var (
		target       string
		email        string
		password     string
		dokployURL   string
		nameOverride string
		apiKeyName   string
		install      bool
		installPort  string
		addrPool     string
		version      string
	)
	fs.StringVar(&target, "target", "dokploy", "target platform to bootstrap (only dokploy is supported)")
	fs.StringVar(&email, "coolify-email", "", "coolify admin email to reuse (prompted if absent and multiple admins exist)")
	fs.StringVar(&password, "coolify-password", "", "coolify admin plaintext password (env BORT_COOLIFY_ADMIN_PASSWORD also accepted)")
	fs.StringVar(&dokployURL, "dokploy-url", "", "dokploy base url (defaults to BORT_DOKPLOY_URL)")
	fs.StringVar(&nameOverride, "name", "", "display name for the dokploy admin (defaults to coolify user name)")
	fs.StringVar(&apiKeyName, "api-key-name", defaultDokployAPIName, "label for the dokploy api key")
	fs.BoolVar(&install, "install", false, "install dokploy on this VPS before bootstrapping credentials")
	fs.StringVar(&installPort, "install-port", "3030", "host port for dokploy UI/API when --install is used")
	fs.StringVar(&addrPool, "swarm-addr-pool", "auto", "docker swarm default address pool for --install (auto selects an unused private /16)")
	fs.StringVar(&version, "dokploy-version", "latest", "dokploy image tag for --install")

	if len(args) > 0 && !strings.HasPrefix(args[0], "-") {
		target = args[0]
		args = args[1:]
	}
	if err := fs.Parse(args); err != nil {
		return err
	}
	if target != "dokploy" {
		return fmt.Errorf("init-target only supports --target=dokploy, got %q", target)
	}
	if dokployURL == "" {
		dokployURL = strings.TrimSpace(os.Getenv(dokploy.EnvBaseURL))
	}
	if dokployURL == "" && install {
		dokployURL = "http://127.0.0.1:" + strings.TrimSpace(installPort)
	}
	if dokployURL == "" {
		return fmt.Errorf("--dokploy-url is required (or set %s)", dokploy.EnvBaseURL)
	}

	admins, err := deps.lister.listAdmins(ctx)
	if err != nil {
		return fmt.Errorf("read coolify admins: %w", err)
	}
	if len(admins) == 0 {
		return errors.New("no coolify Root Team admin/owner users found")
	}

	admin, err := selectCoolifyAdmin(admins, email, stdin, stdout)
	if err != nil {
		return err
	}

	if password == "" {
		password = strings.TrimSpace(os.Getenv(envCoolifyAdminPwd))
	}
	if password == "" {
		password, err = promptPassword(stdin, stdout, fmt.Sprintf("Password for %s: ", admin.Email))
		if err != nil {
			return err
		}
	}
	if password == "" {
		return errors.New("password is required")
	}

	if err := bcrypt.CompareHashAndPassword([]byte(coolifyBcryptHash(admin.PasswordHash)), []byte(password)); err != nil {
		return fmt.Errorf("coolify password verification failed for %s: %w", admin.Email, err)
	}

	displayName := strings.TrimSpace(nameOverride)
	if displayName == "" {
		displayName = strings.TrimSpace(admin.Name)
	}
	if displayName == "" {
		displayName = admin.Email
	}

	if install {
		installer := deps.installer
		if installer == nil {
			installer = shellDokployInstaller{}
		}
		fmt.Fprintf(stdout, "Installing Dokploy in same-VPS shadow mode at %s\n", dokployURL)
		if err := installer.InstallDokploy(ctx, dokployInstallOptions{HostPort: installPort, AddrPool: addrPool, Version: version, ACMEEmail: admin.Email}, stdout, stderr); err != nil {
			return fmt.Errorf("install dokploy: %w", err)
		}
		fmt.Fprintf(stdout, "Waiting for Dokploy to answer at %s\n", dokployURL)
		if err := waitForDokployHTTP(ctx, dokployURL, 2*time.Minute); err != nil {
			return fmt.Errorf("wait for dokploy at %s: %w", dokployURL, err)
		}
	}

	client := deps.newClient(dokployURL)
	if err := client.SignUpAdmin(ctx, displayName, admin.Email, password); err != nil {
		return fmt.Errorf("dokploy sign-up: %w", err)
	}
	cookie, err := client.SignIn(ctx, admin.Email, password)
	if err != nil {
		return fmt.Errorf("dokploy sign-in: %w", err)
	}
	orgID, err := client.GetCurrentUserOrg(ctx, cookie)
	if err != nil {
		return fmt.Errorf("dokploy user.get: %w", err)
	}
	apiKey, err := client.CreateAPIKey(ctx, cookie, apiKeyName, orgID)
	if err != nil {
		return fmt.Errorf("dokploy createApiKey: %w", err)
	}

	state, err := readBortState(deps.statePath)
	if err != nil {
		return err
	}
	state = setTargetCredentials(state, target, targetCredentials{
		URL:            client.BaseURL,
		Token:          apiKey,
		AdminEmail:     admin.Email,
		BootstrappedAt: time.Now().UTC(),
		APIKeyName:     apiKeyName,
	})
	if err := writeBortState(deps.statePath, state); err != nil {
		return err
	}

	fmt.Fprintf(stdout, "Dokploy setup complete for %s at %s\n", admin.Email, client.BaseURL)
	fmt.Fprintln(stdout, "Bort can now continue with this migration.")
	return nil
}

func (shellDokployInstaller) InstallDokploy(ctx context.Context, opts dokployInstallOptions, stdout, stderr io.Writer) error {
	cmd := exec.CommandContext(ctx, "bash", "-s")
	cmd.Stdin = strings.NewReader(dokployShadowInstallScript)
	progress := newInstallProgressWriter(stdout)
	cmd.Stdout = progress
	cmd.Stderr = stderr
	env := os.Environ()
	if strings.TrimSpace(opts.HostPort) != "" {
		env = append(env, "HOST_PORT="+strings.TrimSpace(opts.HostPort))
	}
	if strings.TrimSpace(opts.AddrPool) != "" {
		env = append(env, "ADDR_POOL="+strings.TrimSpace(opts.AddrPool))
	}
	if strings.TrimSpace(opts.Version) != "" {
		env = append(env, "DOKPLOY_VERSION="+strings.TrimSpace(opts.Version))
	}
	if strings.TrimSpace(opts.ACMEEmail) != "" {
		env = append(env, "ACME_EMAIL="+strings.TrimSpace(opts.ACMEEmail))
	}
	cmd.Env = env
	err := cmd.Run()
	if flushErr := progress.Flush(); flushErr != nil && err == nil {
		err = flushErr
	}
	return err
}

type installProgressWriter struct {
	out io.Writer
	st  *styler
	buf strings.Builder
}

func newInstallProgressWriter(out io.Writer) *installProgressWriter {
	return &installProgressWriter{out: out, st: newStyler(out)}
}

func (w *installProgressWriter) Write(p []byte) (int, error) {
	written := len(p)
	for len(p) > 0 {
		idx := bytes.IndexByte(p, '\n')
		if idx < 0 {
			_, _ = w.buf.Write(p)
			return written, nil
		}
		_, _ = w.buf.Write(p[:idx])
		if err := w.writeLine(w.buf.String()); err != nil {
			return 0, err
		}
		w.buf.Reset()
		p = p[idx+1:]
	}
	return written, nil
}

func (w *installProgressWriter) Flush() error {
	if w.buf.Len() == 0 {
		return nil
	}
	err := w.writeLine(w.buf.String())
	w.buf.Reset()
	return err
}

func (w *installProgressWriter) writeLine(line string) error {
	line = strings.TrimRight(line, "\r")
	if step, ok := strings.CutPrefix(line, installProgressPrefix); ok {
		_, err := fmt.Fprintf(w.out, "%s %s\n", w.st.glyph("~", sevDim), strings.TrimSpace(step))
		return err
	}
	_, err := fmt.Fprintln(w.out, line)
	return err
}

func waitForDokployHTTP(ctx context.Context, baseURL string, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	client := &http.Client{Timeout: 2 * time.Second}
	var lastErr error
	for {
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, strings.TrimRight(baseURL, "/"), nil)
		if err != nil {
			return err
		}
		resp, err := client.Do(req)
		if err == nil {
			_ = resp.Body.Close()
			return nil
		}
		lastErr = err
		if time.Now().After(deadline) {
			return lastErr
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(2 * time.Second):
		}
	}
}

// coolify hashes are written with the php $2y$ prefix; bcrypt in go accepts $2a$/$2b$.
func coolifyBcryptHash(hash string) string {
	if strings.HasPrefix(hash, "$2y$") {
		return "$2a$" + strings.TrimPrefix(hash, "$2y$")
	}
	return hash
}

func selectCoolifyAdmin(admins []coolifyAdmin, email string, stdin io.Reader, stdout io.Writer) (coolifyAdmin, error) {
	if email != "" {
		for _, admin := range admins {
			if strings.EqualFold(admin.Email, email) {
				return admin, nil
			}
		}
		return coolifyAdmin{}, fmt.Errorf("coolify admin %q not found", email)
	}
	if len(admins) == 1 {
		return admins[0], nil
	}
	fmt.Fprintln(stdout, "Multiple coolify admins found:")
	for i, admin := range admins {
		fmt.Fprintf(stdout, "  [%d] %s (%s)\n", i+1, admin.Email, admin.Name)
	}
	fmt.Fprint(stdout, "Pick one by email or index: ")
	reader := bufio.NewReader(stdin)
	line, err := reader.ReadString('\n')
	if err != nil && !errors.Is(err, io.EOF) {
		return coolifyAdmin{}, err
	}
	choice := strings.TrimSpace(line)
	if choice == "" {
		return coolifyAdmin{}, errors.New("no admin selected")
	}
	for i, admin := range admins {
		if strings.EqualFold(admin.Email, choice) || choice == fmt.Sprintf("%d", i+1) {
			return admin, nil
		}
	}
	return coolifyAdmin{}, fmt.Errorf("coolify admin %q not in the list", choice)
}

func promptPassword(stdin io.Reader, stdout io.Writer, prompt string) (string, error) {
	fmt.Fprint(stdout, prompt)
	if file, ok := stdin.(*os.File); ok && term.IsTerminal(int(file.Fd())) {
		bytes, err := term.ReadPassword(int(file.Fd()))
		fmt.Fprintln(stdout)
		if err != nil {
			return "", err
		}
		return strings.TrimSpace(string(bytes)), nil
	}
	reader := bufio.NewReader(stdin)
	line, err := reader.ReadString('\n')
	if err != nil && !errors.Is(err, io.EOF) {
		return "", err
	}
	return strings.TrimRight(line, "\r\n"), nil
}

// coolify-db is a postgres container with no host port mapping, so we
// query it via `docker exec` rather than a tcp client. this also avoids
// taking a database driver as a direct dependency for one bootstrap call.
type coolifyDBLister struct {
	envPath    string
	container  string
	runCommand func(ctx context.Context, name string, args ...string) ([]byte, error)
}

type coolifyDBCredentials struct {
	User     string
	Password string
	Database string
}

func (l *coolifyDBLister) listAdmins(ctx context.Context) ([]coolifyAdmin, error) {
	envValues, err := readDotEnvFile(l.envPath)
	if err != nil {
		return nil, err
	}
	creds, err := readCoolifyDBCredentials(envValues, l.envPath)
	if err != nil {
		return nil, err
	}
	container := l.container
	if container == "" {
		container = defaultCoolifyDBContainer
	}
	runCommand := l.runCommand
	if runCommand == nil {
		runCommand = runDockerCommand
	}
	args := []string{
		"exec",
		"-e", "PGPASSWORD=" + creds.Password,
		container,
		"psql", "-U", creds.User, "-d", creds.Database,
		"-A", "-F", psqlAdminFieldSeparator, "-t", "-X", "-q",
		"-c", `SELECT u.email, u.name, u.password FROM "users" u JOIN team_user tu ON tu.user_id = u.id WHERE tu.team_id = 0 AND tu.role IN ('owner', 'admin') ORDER BY u.id`,
	}
	out, err := runCommand(ctx, "docker", args...)
	if err != nil {
		return nil, fmt.Errorf("docker exec psql in %s: %w", container, err)
	}
	return parsePsqlAdminRows(out)
}

func readCoolifyDBCredentials(env map[string]string, envPath string) (coolifyDBCredentials, error) {
	// coolify's docker-compose defaults DB_USERNAME and DB_DATABASE to "coolify",
	// so .env files that only set DB_PASSWORD are valid. mirror that default here.
	user := firstNonEmpty(env, "POSTGRES_USER", "DB_USERNAME")
	if user == "" {
		user = "coolify"
	}
	pass := firstNonEmpty(env, "POSTGRES_PASSWORD", "DB_PASSWORD")
	db := firstNonEmpty(env, "POSTGRES_DB", "DB_DATABASE")
	if db == "" {
		db = "coolify"
	}
	if pass == "" {
		return coolifyDBCredentials{}, fmt.Errorf("coolify env at %s missing POSTGRES_PASSWORD/DB_PASSWORD", envPath)
	}
	return coolifyDBCredentials{User: user, Password: pass, Database: db}, nil
}

func runDockerCommand(ctx context.Context, name string, args ...string) ([]byte, error) {
	cmd := exec.CommandContext(ctx, name, args...)
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		stderrText := strings.TrimSpace(stderr.String())
		if stderrText != "" {
			return nil, fmt.Errorf("%w: %s", err, stderrText)
		}
		return nil, err
	}
	return stdout.Bytes(), nil
}

func parsePsqlAdminRows(out []byte) ([]coolifyAdmin, error) {
	admins := []coolifyAdmin{}
	for _, raw := range strings.Split(string(out), "\n") {
		line := strings.TrimRight(raw, "\r")
		if line == "" {
			continue
		}
		fields := strings.SplitN(line, psqlAdminFieldSeparator, 3)
		if len(fields) != 3 {
			return nil, fmt.Errorf("unexpected psql row %q", line)
		}
		admins = append(admins, coolifyAdmin{
			Email:        fields[0],
			Name:         fields[1],
			PasswordHash: fields[2],
		})
	}
	return admins, nil
}

func firstNonEmpty(env map[string]string, keys ...string) string {
	for _, key := range keys {
		if value := strings.TrimSpace(env[key]); value != "" {
			return value
		}
	}
	return ""
}

func readDotEnvFile(path string) (map[string]string, error) {
	contents, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read %s: %w", path, err)
	}
	values := map[string]string{}
	for _, raw := range strings.Split(string(contents), "\n") {
		line := strings.TrimSpace(raw)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		key, value, ok := strings.Cut(line, "=")
		if !ok {
			continue
		}
		key = strings.TrimSpace(key)
		value = strings.TrimSpace(value)
		if len(value) >= 2 && (value[0] == '"' || value[0] == '\'') && value[len(value)-1] == value[0] {
			value = value[1 : len(value)-1]
		}
		values[key] = value
	}
	return values, nil
}

const dokployShadowInstallScript = `#!/usr/bin/env bash
set -euo pipefail

HOST_PORT="${HOST_PORT:-3030}"
ADDR_POOL="${ADDR_POOL:-auto}"
VERSION_TAG="${DOKPLOY_VERSION:-latest}"
ACME_EMAIL="${ACME_EMAIL:-admin@dokploy.local}"
TRAEFIK_IMAGE="${DOKPLOY_TRAEFIK_IMAGE:-traefik:v3.6.7}"

log() {
    printf 'BORT_INSTALL_STEP\t%s\n' "$*"
}

if [ "$(id -u)" != "0" ]; then
    echo "must run as root" >&2
    exit 1
fi

log "Checking Docker"
if ! command -v docker >/dev/null 2>&1; then
    echo "docker is required before installing Dokploy" >&2
    exit 1
fi

private_ip() {
    ip -o -4 addr show scope global 2>/dev/null | awk 'first == "" { split($4, a, "/"); first = a[1] } END { print first }'
}

log "Finding this server's Docker address"
ADVERTISE_ADDR="${ADVERTISE_ADDR:-$(private_ip)}"
if [ -z "$ADVERTISE_ADDR" ]; then
    ADVERTISE_ADDR="$(curl -4s --connect-timeout 5 https://ifconfig.io || true)"
fi
if [ -z "$ADVERTISE_ADDR" ]; then
    echo "could not determine an address for docker swarm; set ADVERTISE_ADDR and rerun" >&2
    exit 1
fi

docker_info="$(docker info 2>/dev/null || true)"
if grep -q 'Live Restore Enabled: true' <<<"$docker_info"; then
    log "Adjusting Docker live-restore for swarm mode"
    mkdir -p /etc/docker
    [ -s /etc/docker/daemon.json ] || echo '{}' > /etc/docker/daemon.json
    cp /etc/docker/daemon.json "/etc/docker/daemon.json.bak.$(date +%s)"
    python3 - <<'PY'
import json
path = "/etc/docker/daemon.json"
with open(path) as f:
    cfg = json.load(f)
cfg["live-restore"] = False
with open(path, "w") as f:
    json.dump(cfg, f, indent=2)
PY
    systemctl reload docker
    sleep 2
    docker_info="$(docker info 2>/dev/null || true)"
    if grep -q 'Live Restore Enabled: true' <<<"$docker_info"; then
        echo "docker live-restore is still enabled after reload; aborting" >&2
        exit 1
    fi
fi

existing_subnets() {
    docker network ls -q | xargs -r -I{} docker network inspect {} --format '{{range .IPAM.Config}}{{.Subnet}}{{"\n"}}{{end}}' 2>/dev/null || true
}

choose_addr_pool() {
    EXISTING_SUBNETS="$(existing_subnets)" python3 - "$ADDR_POOL" <<'PY'
import ipaddress
import os
import sys

requested = sys.argv[1].strip().lower()
existing = []
for raw in os.environ.get("EXISTING_SUBNETS", "").splitlines():
    raw = raw.strip()
    if not raw:
        continue
    try:
        existing.append(ipaddress.ip_network(raw, strict=False))
    except ValueError:
        pass

auto = requested in ("", "auto")
candidates = [
    "172.28.0.0/16",
    "172.29.0.0/16",
    "172.30.0.0/16",
    "172.31.0.0/16",
    "172.27.0.0/16",
    "172.26.0.0/16",
    "10.88.0.0/16",
    "10.89.0.0/16",
    "10.90.0.0/16",
    "10.91.0.0/16",
]
if not auto:
    candidates = [requested]

for candidate in candidates:
    try:
        network = ipaddress.ip_network(candidate, strict=False)
    except ValueError:
        print(f"invalid Docker swarm address pool {candidate}", file=sys.stderr)
        sys.exit(1)
    if any(network.overlaps(other) for other in existing):
        continue
    print(network)
    sys.exit(0)

if auto:
    print("could not find an unused private Docker swarm address pool", file=sys.stderr)
else:
    print(f"Docker swarm address pool {requested} overlaps an existing Docker network", file=sys.stderr)
sys.exit(1)
PY
}

docker_info="$(docker info 2>/dev/null || true)"
if grep -q 'Swarm: active' <<<"$docker_info"; then
    log "Docker swarm is already active; reusing it"
else
    ADDR_POOL="$(choose_addr_pool)"
    log "Initializing Docker swarm with address pool $ADDR_POOL"
    docker swarm init --advertise-addr "$ADVERTISE_ADDR" --default-addr-pool "$ADDR_POOL" >/dev/null
fi

log "Creating Dokploy overlay network"
docker network inspect dokploy-network >/dev/null 2>&1 || \
    docker network create --driver overlay --attachable dokploy-network >/dev/null

log "Preparing Dokploy config files"
mkdir -p /etc/dokploy/traefik/dynamic
chmod 777 /etc/dokploy
touch /etc/dokploy/traefik/dynamic/acme.json
chmod 600 /etc/dokploy/traefik/dynamic/acme.json

if [ ! -s /etc/dokploy/traefik/traefik.yml ]; then
    cat >/etc/dokploy/traefik/traefik.yml <<YAML
api:
  dashboard: true
entryPoints:
  web:
    address: ":80"
  websecure:
    address: ":443"
providers:
  docker:
    exposedByDefault: false
    network: dokploy-network
  file:
    directory: /etc/dokploy/traefik/dynamic
    watch: true
certificatesResolvers:
  letsencrypt:
    acme:
      email: ${ACME_EMAIL}
      storage: /etc/dokploy/traefik/dynamic/acme.json
      httpChallenge:
        entryPoint: web
YAML
fi

python3 - <<'PY'
import pathlib
import re

static_path = pathlib.Path("/etc/dokploy/traefik/traefik.yml")
dynamic_path = pathlib.Path("/etc/dokploy/traefik/dynamic/dokploy.yml")

static = static_path.read_text()
updated = static
has_dokploy_names = re.search(r"(?m)^  web:\s*$", static) and re.search(r"(?m)^  websecure:\s*$", static)
has_legacy_names = re.search(r"(?m)^  http:\s*\n    address: [\"']?:80[\"']?\s*$", static) and re.search(r"(?m)^  https:\s*\n    address: [\"']?:443[\"']?\s*$", static)
if not has_dokploy_names and has_legacy_names:
    updated = re.sub(r"(?m)^  http:\s*\n(    address: [\"']?:80[\"']?\s*)$", r"  web:\n\1", updated)
    updated = re.sub(r"(?m)^  https:\s*\n(    address: [\"']?:443[\"']?\s*)$", r"  websecure:\n\1", updated)
updated = re.sub(r"(?m)^(\s*entryPoint:\s*)http(\s*)$", r"\1web\2", updated)
if updated != static:
    backup = static_path.with_name(static_path.name + ".bak.bort-entrypoints")
    if not backup.exists():
        backup.write_text(static)
    static_path.write_text(updated)

if dynamic_path.exists():
    dynamic = dynamic_path.read_text()
    updated_dynamic = re.sub(r"(?m)^(\s*-\s*)http(\s*)$", r"\1web\2", dynamic)
    updated_dynamic = re.sub(r"(?m)^(\s*-\s*)https(\s*)$", r"\1websecure\2", updated_dynamic)
    if updated_dynamic != dynamic:
        backup = dynamic_path.with_name(dynamic_path.name + ".bak.bort-entrypoints")
        if not backup.exists():
            backup.write_text(dynamic)
        dynamic_path.write_text(updated_dynamic)
PY

log "Creating Dokploy database secret"
if ! docker secret inspect dokploy_postgres_password >/dev/null 2>&1; then
    POSTGRES_PASSWORD="$(openssl rand -base64 32 | tr -d '=+/' | cut -c1-32)"
    echo "$POSTGRES_PASSWORD" | docker secret create dokploy_postgres_password - >/dev/null
fi

log "Starting Dokploy Postgres"
if ! docker service inspect dokploy-postgres >/dev/null 2>&1; then
    docker service create \
        --name dokploy-postgres \
        --detach=true \
        --constraint 'node.role==manager' \
        --network dokploy-network \
        --env POSTGRES_USER=dokploy \
        --env POSTGRES_DB=dokploy \
        --secret source=dokploy_postgres_password,target=/run/secrets/postgres_password \
        --env POSTGRES_PASSWORD_FILE=/run/secrets/postgres_password \
        --mount type=volume,source=dokploy-postgres,target=/var/lib/postgresql/data \
		postgres:16 >/dev/null
fi

log "Starting Dokploy Redis"
if ! docker service inspect dokploy-redis >/dev/null 2>&1; then
	docker service create \
        --name dokploy-redis \
        --detach=true \
        --constraint 'node.role==manager' \
        --network dokploy-network \
		--mount type=volume,source=dokploy-redis,target=/data \
		redis:7 >/dev/null
fi

DOCKER_IMAGE="dokploy/dokploy:${VERSION_TAG}"
release_tag_env=""
if [ "$VERSION_TAG" != "latest" ]; then
    release_tag_env="-e RELEASE_TAG=${VERSION_TAG}"
fi

log "Starting Dokploy UI/API on port ${HOST_PORT}"
if ! docker service inspect dokploy >/dev/null 2>&1; then
    docker service create \
        --name dokploy \
        --detach=true \
        --replicas 1 \
        --network dokploy-network \
		--mount type=bind,source=/var/run/docker.sock,target=/var/run/docker.sock \
        --mount type=bind,source=/etc/dokploy,target=/etc/dokploy \
        --mount type=volume,source=dokploy,target=/root/.docker \
        --secret source=dokploy_postgres_password,target=/run/secrets/postgres_password \
        --publish published="$HOST_PORT",target=3000,mode=host \
        --update-parallelism 1 \
        --update-order stop-first \
        --constraint 'node.role == manager' \
        $release_tag_env \
        -e ADVERTISE_ADDR="$ADVERTISE_ADDR" \
        -e POSTGRES_PASSWORD_FILE=/run/secrets/postgres_password \
        "$DOCKER_IMAGE" >/dev/null
fi

log "Preparing Dokploy edge proxy for cutover"
if ! docker inspect dokploy-traefik >/dev/null 2>&1; then
    docker pull --quiet "$TRAEFIK_IMAGE" >/dev/null
    docker create \
        --name dokploy-traefik \
        --restart always \
        --network dokploy-network \
        -v /etc/dokploy/traefik/traefik.yml:/etc/traefik/traefik.yml \
        -v /etc/dokploy/traefik/dynamic:/etc/dokploy/traefik/dynamic \
        -v /var/run/docker.sock:/var/run/docker.sock:ro \
        -p 80:80/tcp \
        -p 443:443/tcp \
        -p 443:443/udp \
        "$TRAEFIK_IMAGE" >/dev/null
fi

echo
echo "Dokploy installed in same-VPS shadow mode."
echo "UI/API: http://127.0.0.1:${HOST_PORT}"
echo "Coolify still owns :80/:443. Dokploy Traefik is prepared and will start during cutover."
`
