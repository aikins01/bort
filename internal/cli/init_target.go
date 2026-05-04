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
	defaultCoolifyEnvPath      = "/data/coolify/source/.env"
	defaultCoolifyDBContainer  = "coolify-db"
	defaultDokployAPIName      = "bort-cli"
	envCoolifyAdminPwd         = "BORT_COOLIFY_ADMIN_PASSWORD"
	psqlAdminFieldSeparator    = "\t"
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
	newClient func(baseURL string) *dokploy.Client
	statePath string
}

func runInitTarget(ctx context.Context, args []string, stdin io.Reader, stdout, stderr io.Writer) error {
	return runInitTargetWith(ctx, args, stdin, stdout, stderr, initTargetDeps{
		lister:    &coolifyDBLister{envPath: defaultCoolifyEnvPath, container: defaultCoolifyDBContainer},
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
		target      string
		email       string
		password    string
		dokployURL  string
		nameOverride string
		apiKeyName  string
	)
	fs.StringVar(&target, "target", "dokploy", "target platform to bootstrap (only dokploy is supported)")
	fs.StringVar(&email, "coolify-email", "", "coolify admin email to reuse (prompted if absent and multiple admins exist)")
	fs.StringVar(&password, "coolify-password", "", "coolify admin plaintext password (env BORT_COOLIFY_ADMIN_PASSWORD also accepted)")
	fs.StringVar(&dokployURL, "dokploy-url", "", "dokploy base url (defaults to BORT_DOKPLOY_URL)")
	fs.StringVar(&nameOverride, "name", "", "display name for the dokploy admin (defaults to coolify user name)")
	fs.StringVar(&apiKeyName, "api-key-name", defaultDokployAPIName, "label for the dokploy api key")

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
	if dokployURL == "" {
		return fmt.Errorf("--dokploy-url is required (or set %s)", dokploy.EnvBaseURL)
	}

	admins, err := deps.lister.listAdmins(ctx)
	if err != nil {
		return fmt.Errorf("read coolify admins: %w", err)
	}
	if len(admins) == 0 {
		return errors.New("no coolify admins found in users table")
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

	fmt.Fprintf(stdout, "Dokploy bootstrapped for %s at %s\n", admin.Email, client.BaseURL)
	fmt.Fprintln(stdout, "API key stored in .bort/state.json (target=dokploy)")
	return nil
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
		"-c", `SELECT email, name, password FROM "users" ORDER BY id`,
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
