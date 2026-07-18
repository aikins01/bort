package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/aikins01/bort/internal/target/dokploy"
	"golang.org/x/crypto/bcrypt"
)

type fakeAdminLister struct {
	admins []coolifyAdmin
}

func (f *fakeAdminLister) listAdmins(_ context.Context) ([]coolifyAdmin, error) {
	return f.admins, nil
}

type fakeDokployInstaller struct {
	calls int
	opts  dokployInstallOptions
}

func (f *fakeDokployInstaller) InstallDokploy(_ context.Context, opts dokployInstallOptions, _, _ io.Writer) error {
	f.calls++
	f.opts = opts
	return nil
}

func bcryptCoolifyHash(t *testing.T, password string) string {
	t.Helper()
	hash, err := bcrypt.GenerateFromPassword([]byte(password), bcrypt.MinCost)
	if err != nil {
		t.Fatalf("hash: %v", err)
	}
	return "$2y$" + strings.TrimPrefix(string(hash), "$2a$")
}

type dokployStub struct {
	signupCalls    int
	signupExists   bool
	signinCalls    int
	createKeyCalls int
	lastKeyName    string
	lastOrgID      string
}

func newDokployStub(t *testing.T, stub *dokployStub) *httptest.Server {
	t.Helper()
	mux := http.NewServeMux()
	mux.HandleFunc("/api/auth/sign-up/email", func(w http.ResponseWriter, r *http.Request) {
		stub.signupCalls++
		if stub.signupExists {
			http.Error(w, `{"code":"USER_ALREADY_EXISTS","message":"User with this email already exists"}`, http.StatusUnprocessableEntity)
			return
		}
		w.WriteHeader(http.StatusOK)
		_ = json.NewEncoder(w).Encode(map[string]string{"id": "u1"})
	})
	mux.HandleFunc("/api/auth/sign-in/email", func(w http.ResponseWriter, r *http.Request) {
		stub.signinCalls++
		http.SetCookie(w, &http.Cookie{Name: "better-auth.session_token", Value: "session-xyz", Path: "/", HttpOnly: true})
		w.WriteHeader(http.StatusOK)
		_ = json.NewEncoder(w).Encode(map[string]string{"id": "u1"})
	})
	mux.HandleFunc("/api/trpc/user.get", func(w http.ResponseWriter, r *http.Request) {
		if !strings.Contains(r.Header.Get("Cookie"), "better-auth.session_token=session-xyz") {
			http.Error(w, "no cookie", http.StatusUnauthorized)
			return
		}
		_, _ = w.Write([]byte(`[{"result":{"data":{"json":{"organizationId":"org-1","role":"owner"}}}}]`))
	})
	mux.HandleFunc("/api/trpc/user.createApiKey", func(w http.ResponseWriter, r *http.Request) {
		stub.createKeyCalls++
		if !strings.Contains(r.Header.Get("Cookie"), "better-auth.session_token=session-xyz") {
			http.Error(w, "no cookie", http.StatusUnauthorized)
			return
		}
		var payload map[string]map[string]map[string]any
		if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		inner := payload["0"]["json"]
		stub.lastKeyName, _ = inner["name"].(string)
		if md, ok := inner["metadata"].(map[string]any); ok {
			stub.lastOrgID, _ = md["organizationId"].(string)
		}
		_, _ = w.Write([]byte(`[{"result":{"data":{"json":{"key":"K-secret"}}}}]`))
	})
	return httptest.NewServer(mux)
}

func TestInitTargetBcryptMismatchAbortsBeforeDokploy(t *testing.T) {
	stub := &dokployStub{}
	server := newDokployStub(t, stub)
	defer server.Close()

	statePath := filepath.Join(t.TempDir(), "state.json")
	deps := initTargetDeps{
		lister: &fakeAdminLister{admins: []coolifyAdmin{
			{Email: "admin@example.com", Name: "Admin User", PasswordHash: bcryptCoolifyHash(t, "correct-horse")},
		}},
		newClient: func(string) *dokploy.Client { return defaultDokployClient(server.URL) },
		statePath: statePath,
	}

	stdin := strings.NewReader("")
	stdout := &bytes.Buffer{}
	stderr := &bytes.Buffer{}
	t.Setenv(envCoolifyAdminPwd, "wrong-password")
	args := []string{
		"--coolify-email", "admin@example.com",
		"--dokploy-url", server.URL,
	}
	err := runInitTargetWith(context.Background(), args, stdin, stdout, stderr, deps)
	if err == nil || !strings.Contains(err.Error(), "verification failed") {
		t.Fatalf("expected verification failure, got %v", err)
	}
	if stub.signupCalls+stub.signinCalls+stub.createKeyCalls != 0 {
		t.Fatalf("dokploy was contacted before bcrypt verify: %+v", stub)
	}
}

func TestInitTargetSuccessfulBootstrapPersistsState(t *testing.T) {
	stub := &dokployStub{}
	server := newDokployStub(t, stub)
	defer server.Close()

	tempDir := t.TempDir()
	statePath := filepath.Join(tempDir, "state.json")
	deps := initTargetDeps{
		lister: &fakeAdminLister{admins: []coolifyAdmin{
			{Email: "admin@example.com", Name: "Admin User", PasswordHash: bcryptCoolifyHash(t, "right-password")},
		}},
		newClient: func(string) *dokploy.Client { return defaultDokployClient(server.URL) },
		statePath: statePath,
	}

	stdout := &bytes.Buffer{}
	stderr := &bytes.Buffer{}
	t.Setenv(envCoolifyAdminPwd, "right-password")
	args := []string{
		"--coolify-email", "admin@example.com",
		"--dokploy-url", server.URL,
	}
	if err := runInitTargetWith(context.Background(), args, strings.NewReader(""), stdout, stderr, deps); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if stub.signupCalls != 1 || stub.signinCalls != 1 || stub.createKeyCalls != 1 {
		t.Fatalf("unexpected call counts: %+v", stub)
	}
	if stub.lastOrgID != "org-1" || stub.lastKeyName != defaultDokployAPIName {
		t.Fatalf("unexpected createApiKey payload: %+v", stub)
	}
	if strings.Contains(stdout.String(), "K-secret") {
		t.Fatalf("api key leaked to stdout: %q", stdout.String())
	}
	if strings.Contains(strings.ToLower(stdout.String()), "api key") {
		t.Fatalf("api key implementation detail leaked to stdout: %q", stdout.String())
	}
	if !strings.Contains(stdout.String(), "Dokploy setup complete for admin@example.com") || !strings.Contains(stdout.String(), "Bort can now continue with this migration.") {
		t.Fatalf("expected setup-complete notice on stdout, got %q", stdout.String())
	}

	state, err := readBortState(statePath)
	if err != nil {
		t.Fatalf("read state: %v", err)
	}
	creds, ok := state.Targets["dokploy"]
	if !ok {
		t.Fatalf("expected dokploy creds in state, got %+v", state.Targets)
	}
	if creds.Token != "K-secret" || creds.URL != server.URL || creds.AdminEmail != "admin@example.com" {
		t.Fatalf("unexpected creds: %+v", creds)
	}
}

func TestInitTargetInstallRunsInstallerBeforeBootstrap(t *testing.T) {
	stub := &dokployStub{}
	server := newDokployStub(t, stub)
	defer server.Close()
	installer := &fakeDokployInstaller{}

	statePath := filepath.Join(t.TempDir(), "state.json")
	deps := initTargetDeps{
		lister: &fakeAdminLister{admins: []coolifyAdmin{
			{Email: "admin@example.com", Name: "Admin User", PasswordHash: bcryptCoolifyHash(t, "right-password")},
		}},
		installer: installer,
		newClient: func(string) *dokploy.Client { return defaultDokployClient(server.URL) },
		statePath: statePath,
	}

	t.Setenv(envCoolifyAdminPwd, "right-password")
	args := []string{
		"--install",
		"--install-port", "3031",
		"--swarm-addr-pool", "172.29.0.0/16",
		"--dokploy-version", "v0.26.6",
		"--coolify-email", "admin@example.com",
		"--dokploy-url", server.URL,
	}
	if err := runInitTargetWith(context.Background(), args, strings.NewReader(""), &bytes.Buffer{}, &bytes.Buffer{}, deps); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if installer.calls != 1 {
		t.Fatalf("expected installer to run once, got %d", installer.calls)
	}
	if installer.opts.HostPort != "3031" || installer.opts.AddrPool != "172.29.0.0/16" || installer.opts.Version != "v0.26.6" || installer.opts.ACMEEmail != "admin@example.com" {
		t.Fatalf("unexpected install opts: %#v", installer.opts)
	}
	if stub.signupCalls != 1 || stub.signinCalls != 1 || stub.createKeyCalls != 1 {
		t.Fatalf("expected bootstrap after install, got %+v", stub)
	}
}

func TestInitTargetInstallDefaultsSwarmAddrPoolToAuto(t *testing.T) {
	stub := &dokployStub{}
	server := newDokployStub(t, stub)
	defer server.Close()
	installer := &fakeDokployInstaller{}

	statePath := filepath.Join(t.TempDir(), "state.json")
	deps := initTargetDeps{
		lister: &fakeAdminLister{admins: []coolifyAdmin{
			{Email: "admin@example.com", Name: "Admin User", PasswordHash: bcryptCoolifyHash(t, "right-password")},
		}},
		installer: installer,
		newClient: func(string) *dokploy.Client { return defaultDokployClient(server.URL) },
		statePath: statePath,
	}

	t.Setenv(envCoolifyAdminPwd, "right-password")
	args := []string{
		"--install",
		"--coolify-email", "admin@example.com",
		"--dokploy-url", server.URL,
	}
	if err := runInitTargetWith(context.Background(), args, strings.NewReader(""), &bytes.Buffer{}, &bytes.Buffer{}, deps); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if installer.opts.AddrPool != "auto" {
		t.Fatalf("expected default addr pool auto, got %#v", installer.opts)
	}
}

func TestInitTargetInstallPromptsAndVerifiesPasswordBeforeInstaller(t *testing.T) {
	stub := &dokployStub{}
	server := newDokployStub(t, stub)
	defer server.Close()
	installer := &fakeDokployInstaller{}

	statePath := filepath.Join(t.TempDir(), "state.json")
	deps := initTargetDeps{
		lister: &fakeAdminLister{admins: []coolifyAdmin{
			{Email: "admin@example.com", Name: "Admin User", PasswordHash: bcryptCoolifyHash(t, "right-password")},
		}},
		installer: installer,
		newClient: func(string) *dokploy.Client { return defaultDokployClient(server.URL) },
		statePath: statePath,
	}

	stdout := &bytes.Buffer{}
	t.Setenv(envCoolifyAdminPwd, "")
	args := []string{
		"--install",
		"--coolify-email", "admin@example.com",
		"--dokploy-url", server.URL,
	}
	if err := runInitTargetWith(context.Background(), args, strings.NewReader("right-password\n"), stdout, &bytes.Buffer{}, deps); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	promptIndex := strings.Index(stdout.String(), "Password for admin@example.com:")
	installIndex := strings.Index(stdout.String(), "Installing Dokploy")
	if promptIndex < 0 || installIndex < 0 || promptIndex > installIndex {
		t.Fatalf("expected password prompt before install, got stdout %q", stdout.String())
	}
	if installer.calls != 1 {
		t.Fatalf("expected installer to run after password verification, got %d", installer.calls)
	}
}

func TestInitTargetInstallPasswordMismatchSkipsInstaller(t *testing.T) {
	stub := &dokployStub{}
	server := newDokployStub(t, stub)
	defer server.Close()
	installer := &fakeDokployInstaller{}

	deps := initTargetDeps{
		lister: &fakeAdminLister{admins: []coolifyAdmin{
			{Email: "admin@example.com", Name: "Admin User", PasswordHash: bcryptCoolifyHash(t, "right-password")},
		}},
		installer: installer,
		newClient: func(string) *dokploy.Client { return defaultDokployClient(server.URL) },
		statePath: filepath.Join(t.TempDir(), "state.json"),
	}

	t.Setenv(envCoolifyAdminPwd, "wrong-password")
	args := []string{
		"--install",
		"--coolify-email", "admin@example.com",
		"--dokploy-url", server.URL,
	}
	err := runInitTargetWith(context.Background(), args, strings.NewReader(""), &bytes.Buffer{}, &bytes.Buffer{}, deps)
	if err == nil || !strings.Contains(err.Error(), "verification failed") {
		t.Fatalf("expected verification failure, got %v", err)
	}
	if installer.calls != 0 {
		t.Fatalf("installer ran before password verification")
	}
	if stub.signupCalls+stub.signinCalls+stub.createKeyCalls != 0 {
		t.Fatalf("dokploy was contacted before password verification: %+v", stub)
	}
}

func TestInstallProgressWriterUsesBortStatusGlyphs(t *testing.T) {
	var out bytes.Buffer
	writer := newInstallProgressWriter(&out)
	if _, err := writer.Write([]byte(installProgressPrefix + "Checking Docker\nraw docker output\n" + installProgressPrefix + "Starting Dokploy")); err != nil {
		t.Fatalf("write progress: %v", err)
	}
	if err := writer.Flush(); err != nil {
		t.Fatalf("flush progress: %v", err)
	}
	want := "~ Checking Docker\nraw docker output\n~ Starting Dokploy\n"
	if out.String() != want {
		t.Fatalf("unexpected progress output:\ngot  %q\nwant %q", out.String(), want)
	}
}

func TestReadCoolifyDBCredentialsDefaultsToCoolify(t *testing.T) {
	creds, err := readCoolifyDBCredentials(map[string]string{
		"DB_PASSWORD": "secret",
	}, "/tmp/.env")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if creds.User != "coolify" || creds.Database != "coolify" || creds.Password != "secret" {
		t.Fatalf("unexpected creds: %+v", creds)
	}
}

func TestReadCoolifyDBCredentialsRequiresPassword(t *testing.T) {
	if _, err := readCoolifyDBCredentials(map[string]string{}, "/tmp/.env"); err == nil {
		t.Fatalf("expected error when password is missing")
	}
}

func TestReadCoolifyDBCredentialsRespectsExplicitOverrides(t *testing.T) {
	creds, err := readCoolifyDBCredentials(map[string]string{
		"POSTGRES_USER":     "alice",
		"POSTGRES_PASSWORD": "p4ss",
		"POSTGRES_DB":       "appdb",
	}, "/tmp/.env")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if creds.User != "alice" || creds.Database != "appdb" || creds.Password != "p4ss" {
		t.Fatalf("unexpected creds: %+v", creds)
	}
}

func TestParsePsqlAdminRowsHandlesTabSeparated(t *testing.T) {
	out := []byte("operator@example.com\tOperator\t$2y$10$abc\nadmin@example.com\tAdmin User\t$2y$10$def\n")
	admins, err := parsePsqlAdminRows(out)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if len(admins) != 2 {
		t.Fatalf("expected 2 admins, got %d: %#v", len(admins), admins)
	}
	if admins[0].Email != "operator@example.com" || admins[0].Name != "Operator" || admins[0].PasswordHash != "$2y$10$abc" {
		t.Fatalf("unexpected first admin: %#v", admins[0])
	}
	if admins[1].Email != "admin@example.com" || admins[1].Name != "Admin User" || admins[1].PasswordHash != "$2y$10$def" {
		t.Fatalf("unexpected second admin: %#v", admins[1])
	}
}

func TestCoolifyDBListerStagesPgpassInsteadOfArgvPassword(t *testing.T) {
	dir := t.TempDir()
	envPath := filepath.Join(dir, ".env")
	const testPass = "test-secret-value"
	if err := os.WriteFile(envPath, []byte("DB_PASSWORD="+testPass+"\n"), 0o600); err != nil {
		t.Fatalf("write env: %v", err)
	}
	type invocation struct {
		name string
		args []string
	}
	var calls []invocation
	lister := &coolifyDBLister{
		envPath:   envPath,
		container: "coolify-db",
		runCommand: func(ctx context.Context, name string, args ...string) ([]byte, error) {
			calls = append(calls, invocation{name: name, args: append([]string{}, args...)})
			return []byte("alice@example.com\tAlice\t$2y$10$xxx\n"), nil
		},
	}
	admins, err := lister.listAdmins(context.Background())
	if err != nil {
		t.Fatalf("listAdmins: %v", err)
	}
	if len(admins) != 1 || admins[0].Email != "alice@example.com" {
		t.Fatalf("unexpected admins: %#v", admins)
	}
	for _, call := range calls {
		if call.name != "docker" {
			t.Fatalf("expected docker command, got %q", call.name)
		}
		joined := strings.Join(call.args, " ")
		if strings.Contains(joined, testPass) || strings.Contains(joined, "PGPASSWORD=") {
			t.Fatalf("db password exposed in argv: %v", call.args)
		}
	}
	if len(calls) != 3 {
		t.Fatalf("expected cp + psql + cleanup calls, got %d: %#v", len(calls), calls)
	}
	cp := calls[0]
	if cp.args[0] != "cp" || !strings.HasPrefix(cp.args[2], "coolify-db:/tmp/bort-pgpass-") {
		t.Fatalf("unexpected pgpass staging call: %#v", cp.args)
	}
	hostPgpass := cp.args[1]
	execCall := calls[1]
	if execCall.args[0] != "exec" || execCall.args[1] != "-e" || !strings.HasPrefix(execCall.args[2], "PGPASSFILE=/tmp/bort-pgpass-") {
		t.Fatalf("psql call must reference PGPASSFILE, got %#v", execCall.args)
	}
	if !strings.HasSuffix(hostPgpass, strings.TrimPrefix(execCall.args[2], "PGPASSFILE=/tmp/")) {
		t.Fatalf("host pgpass %q does not match container path %q", hostPgpass, execCall.args[2])
	}
	query := strings.Join(execCall.args, " ")
	for _, want := range []string{"JOIN team_user", "tu.team_id = 0", "tu.role IN ('owner', 'admin')"} {
		if !strings.Contains(query, want) {
			t.Fatalf("expected admin query to contain %q, got %q", want, query)
		}
	}
	cleanup := calls[2]
	if cleanup.args[0] != "exec" || cleanup.args[1] != "coolify-db" || cleanup.args[2] != "rm" || cleanup.args[3] != "-f" || !strings.HasPrefix(cleanup.args[4], "/tmp/bort-pgpass-") {
		t.Fatalf("unexpected container pgpass cleanup call: %#v", cleanup.args)
	}
	if _, err := os.Stat(hostPgpass); !os.IsNotExist(err) {
		t.Fatalf("host pgpass file %q was not removed", hostPgpass)
	}
}

func TestCoolifyPgpassFileContentEscapesColonAndBackslash(t *testing.T) {
	got := coolifyPgpassFileContent(`pa:ss\wo:rd`)
	want := `*:*:*:*:pa\:ss\\wo\:rd` + "\n"
	if got != want {
		t.Fatalf("pgpass content = %q, want %q", got, want)
	}
}

func TestInitTargetIdempotentRerunWhenUserExists(t *testing.T) {
	stub := &dokployStub{signupExists: true}
	server := newDokployStub(t, stub)
	defer server.Close()

	statePath := filepath.Join(t.TempDir(), "state.json")
	deps := initTargetDeps{
		lister: &fakeAdminLister{admins: []coolifyAdmin{
			{Email: "admin@example.com", Name: "Admin User", PasswordHash: bcryptCoolifyHash(t, "right-password")},
		}},
		newClient: func(string) *dokploy.Client { return defaultDokployClient(server.URL) },
		statePath: statePath,
	}

	t.Setenv(envCoolifyAdminPwd, "right-password")
	args := []string{
		"--coolify-email", "admin@example.com",
		"--dokploy-url", server.URL,
	}
	if err := runInitTargetWith(context.Background(), args, strings.NewReader(""), &bytes.Buffer{}, &bytes.Buffer{}, deps); err != nil {
		t.Fatalf("unexpected error on idempotent run: %v", err)
	}
	if stub.signupCalls != 1 || stub.signinCalls != 1 || stub.createKeyCalls != 1 {
		t.Fatalf("expected sign-in + create-key to proceed past sign-up: %+v", stub)
	}
	state, err := readBortState(statePath)
	if err != nil {
		t.Fatalf("read state: %v", err)
	}
	if state.Targets["dokploy"].Token != "K-secret" {
		t.Fatalf("expected token persisted on idempotent run, got %+v", state.Targets)
	}
}

func TestInitTargetRejectsCoolifyPasswordFlag(t *testing.T) {
	const secretValue = "sup3r-secret-flag-value"
	deps := initTargetDeps{
		lister:    &fakeAdminLister{},
		statePath: filepath.Join(t.TempDir(), "state.json"),
	}
	stderr := &bytes.Buffer{}
	args := []string{
		"--coolify-email", "admin@example.com",
		"--coolify-password", secretValue,
		"--dokploy-url", "http://127.0.0.1:3030",
	}
	err := runInitTargetWith(context.Background(), args, strings.NewReader(""), &bytes.Buffer{}, stderr, deps)
	if err == nil {
		t.Fatalf("expected --coolify-password to be rejected")
	}
	if strings.Contains(err.Error(), secretValue) || strings.Contains(stderr.String(), secretValue) {
		t.Fatalf("rejected flag leaked its value: err=%v stderr=%q", err, stderr.String())
	}
}

func TestValidateInstallPort(t *testing.T) {
	valid := []string{"1", "3030", "65535", " 8080 "}
	for _, port := range valid {
		if err := validateInstallPort(port); err != nil {
			t.Fatalf("expected port %q to be valid: %v", port, err)
		}
	}
	invalid := []string{"", "abc", "0", "-1", "+3030", "0303", "65536", "30.5", "3030; reboot"}
	for _, port := range invalid {
		if err := validateInstallPort(port); err == nil {
			t.Fatalf("expected port %q to be rejected", port)
		}
	}
}

func TestValidateDokployVersionTag(t *testing.T) {
	valid := []string{"latest", "v0.26.6", "0.26.6-beta_1", "_rc2", strings.Repeat("a", 128)}
	for _, tag := range valid {
		if err := validateDokployVersionTag(tag); err != nil {
			t.Fatalf("expected tag %q to be valid: %v", tag, err)
		}
	}
	invalid := []string{
		"",
		"with space",
		"v0.26.6; rm -rf /",
		"$(whoami)",
		"v1`id`",
		".leading-dot",
		"-leading-dash",
		"tag\nnext",
		strings.Repeat("a", 129),
	}
	for _, tag := range invalid {
		if err := validateDokployVersionTag(tag); err == nil {
			t.Fatalf("expected tag %q to be rejected", tag)
		}
	}
}

func TestValidateACMEEmail(t *testing.T) {
	valid := []string{"admin@example.com", "a.b+tag@sub.example.co"}
	for _, email := range valid {
		if err := validateACMEEmail(email); err != nil {
			t.Fatalf("expected email %q to be valid: %v", email, err)
		}
	}
	invalid := []string{
		"",
		"not-an-email",
		"Admin <admin@example.com>",
		"admin@example.com\nemail: injected",
		"admin@example.com\r\nx: y",
		"a#b@example.com",
		"admin@localhost",
		" spaced@example.com",
	}
	for _, email := range invalid {
		if err := validateACMEEmail(email); err == nil {
			t.Fatalf("expected email %q to be rejected", email)
		}
	}
}

func TestInitTargetInstallRejectsInvalidPortBeforeBootstrap(t *testing.T) {
	installer := &fakeDokployInstaller{}
	deps := initTargetDeps{
		lister:    &fakeAdminLister{},
		installer: installer,
		statePath: filepath.Join(t.TempDir(), "state.json"),
	}
	t.Setenv(dokploy.EnvBaseURL, "")
	args := []string{"--install", "--install-port", "0"}
	err := runInitTargetWith(context.Background(), args, strings.NewReader(""), &bytes.Buffer{}, &bytes.Buffer{}, deps)
	if err == nil || !strings.Contains(err.Error(), "install-port") {
		t.Fatalf("expected install-port validation error, got %v", err)
	}
	if installer.calls != 0 {
		t.Fatalf("installer ran despite invalid port")
	}
}

func TestInitTargetInstallRejectsInvalidVersionTag(t *testing.T) {
	installer := &fakeDokployInstaller{}
	deps := initTargetDeps{
		lister:    &fakeAdminLister{},
		installer: installer,
		statePath: filepath.Join(t.TempDir(), "state.json"),
	}
	args := []string{"--install", "--dokploy-version", "v1; rm -rf /", "--dokploy-url", "http://127.0.0.1:3030"}
	err := runInitTargetWith(context.Background(), args, strings.NewReader(""), &bytes.Buffer{}, &bytes.Buffer{}, deps)
	if err == nil || !strings.Contains(err.Error(), "dokploy-version") {
		t.Fatalf("expected dokploy-version validation error, got %v", err)
	}
	if installer.calls != 0 {
		t.Fatalf("installer ran despite invalid version tag")
	}
}

func TestInitTargetInstallRejectsUnsafeACMEEmail(t *testing.T) {
	stub := &dokployStub{}
	server := newDokployStub(t, stub)
	defer server.Close()
	installer := &fakeDokployInstaller{}
	deps := initTargetDeps{
		lister: &fakeAdminLister{admins: []coolifyAdmin{
			{Email: "a#b@example.com", Name: "Admin User", PasswordHash: "unused"},
		}},
		installer: installer,
		newClient: func(string) *dokploy.Client { return defaultDokployClient(server.URL) },
		statePath: filepath.Join(t.TempDir(), "state.json"),
	}
	args := []string{"--install", "--coolify-email", "a#b@example.com", "--dokploy-url", server.URL}
	err := runInitTargetWith(context.Background(), args, strings.NewReader(""), &bytes.Buffer{}, &bytes.Buffer{}, deps)
	if err == nil || !strings.Contains(err.Error(), "ACME") {
		t.Fatalf("expected ACME email validation error, got %v", err)
	}
	if installer.calls != 0 {
		t.Fatalf("installer ran despite unsafe ACME email")
	}
	if stub.signupCalls+stub.signinCalls+stub.createKeyCalls != 0 {
		t.Fatalf("dokploy was contacted despite unsafe ACME email: %+v", stub)
	}
}

func TestDokployShadowInstallScriptIsHardened(t *testing.T) {
	for _, banned := range []string{"chmod 777", "release_tag_env"} {
		if strings.Contains(dokployShadowInstallScript, banned) {
			t.Fatalf("install script still contains unsafe %q", banned)
		}
	}
	for _, want := range []string{
		`chown root:root /etc/dokploy /etc/dokploy/traefik /etc/dokploy/traefik/dynamic`,
		`chmod 755 /etc/dokploy /etc/dokploy/traefik /etc/dokploy/traefik/dynamic`,
		`chmod 600 /etc/dokploy/traefik/dynamic/acme.json`,
		`release_tag_args=(-e "RELEASE_TAG=$VERSION_TAG")`,
		`"${release_tag_args[@]}"`,
		`ipaddress.ip_address`,
		`python3 is required before installing Dokploy`,
		`python3 is required before installing Dokploy`,
	} {
		if !strings.Contains(dokployShadowInstallScript, want) {
			t.Fatalf("install script missing hardened fragment %q", want)
		}
	}
}

func TestValidateDokployBootstrapURLPolicy(t *testing.T) {
	t.Run("accepts http localhost case-insensitively", func(t *testing.T) {
		if err := validateDokployBootstrapURL("http://LOCALHOST:3030"); err != nil {
			t.Fatalf("expected LOCALHOST to be accepted: %v", err)
		}
	})

	t.Run("accepts remote https", func(t *testing.T) {
		if err := validateDokployBootstrapURL("https://dokploy.example.com"); err != nil {
			t.Fatalf("expected https URL to be accepted: %v", err)
		}
	})

	t.Run("rejects embedded credentials without echoing them", func(t *testing.T) {
		const secret = "s3cr3t-in-userinfo"
		err := validateDokployBootstrapURL("https://admin:" + secret + "@dokploy.example.com")
		if err == nil {
			t.Fatal("expected userinfo URL to be rejected")
		}
		if strings.Contains(err.Error(), secret) {
			t.Fatalf("error leaked embedded credential: %v", err)
		}
	})

	t.Run("rejects malformed credentials without echoing them", func(t *testing.T) {
		const secret = "s3cr3t-in-userinfo"
		err := validateDokployBootstrapURL("https://admin:" + secret + "@dokploy exam ple.com")
		if err == nil {
			t.Fatal("expected malformed URL to be rejected")
		}
		if strings.Contains(err.Error(), secret) {
			t.Fatalf("error leaked embedded credential: %v", err)
		}
	})

	t.Run("rejects empty-host userinfo without echoing credentials", func(t *testing.T) {
		const secret = "s3cr3t-in-userinfo"
		err := validateDokployBootstrapURL("http://admin:" + secret + "@")
		if err == nil {
			t.Fatal("expected empty-host URL to be rejected")
		}
		if strings.Contains(err.Error(), secret) {
			t.Fatalf("error leaked embedded credential: %v", err)
		}
	})

	t.Run("rejects malformed credentials without echoing them", func(t *testing.T) {
		const secret = "s3cr3t-in-userinfo"
		err := validateDokployBootstrapURL("https://admin:" + secret + "@dokploy exam ple.com")
		if err == nil {
			t.Fatal("expected malformed URL to be rejected")
		}
		if strings.Contains(err.Error(), secret) {
			t.Fatalf("error leaked embedded credential: %v", err)
		}
	})

	t.Run("rejects empty-host userinfo without echoing credentials", func(t *testing.T) {
		const secret = "s3cr3t-in-userinfo"
		err := validateDokployBootstrapURL("http://admin:" + secret + "@")
		if err == nil {
			t.Fatal("expected empty-host URL to be rejected")
		}
		if strings.Contains(err.Error(), secret) {
			t.Fatalf("error leaked embedded credential: %v", err)
		}
	})

	t.Run("rejects unsupported scheme", func(t *testing.T) {
		if err := validateDokployBootstrapURL("ftp://dokploy.example.com"); err == nil {
			t.Fatal("expected ftp scheme to be rejected")
		}
	})
}

func TestDefaultDokployClientRedirectPolicy(t *testing.T) {
	newVia := func(raw string) []*http.Request {
		return []*http.Request{httptest.NewRequest(http.MethodGet, raw, nil)}
	}

	t.Run("rejects https to http downgrade", func(t *testing.T) {
		client := defaultDokployClient("https://dokploy.example.com").HTTPClient
		req := httptest.NewRequest(http.MethodGet, "http://dokploy.example.com/api/auth/sign-in/email", nil)
		if err := client.CheckRedirect(req, newVia("https://dokploy.example.com/api/auth/sign-in/email")); err == nil || !strings.Contains(err.Error(), "refusing Dokploy bootstrap redirect") {
			t.Fatalf("expected redirect refusal, got %v", err)
		}
	})

	t.Run("rejects cross origin subdomain", func(t *testing.T) {
		client := defaultDokployClient("https://dokploy.example.com").HTTPClient
		req := httptest.NewRequest(http.MethodGet, "https://api.dokploy.example.com/api/auth/sign-in/email", nil)
		if err := client.CheckRedirect(req, newVia("https://dokploy.example.com/api/auth/sign-in/email")); err == nil || !strings.Contains(err.Error(), "refusing Dokploy bootstrap redirect") {
			t.Fatalf("expected redirect refusal, got %v", err)
		}
	})

	t.Run("allows same origin with explicit default port", func(t *testing.T) {
		client := defaultDokployClient("https://dokploy.example.com").HTTPClient
		req := httptest.NewRequest(http.MethodGet, "https://dokploy.example.com:443/api/settings/get-redirect-url", nil)
		if err := client.CheckRedirect(req, newVia("https://dokploy.example.com/api/auth/sign-in/email")); err != nil {
			t.Fatalf("expected redirect to be allowed, got %v", err)
		}
	})

	t.Run("allows same origin with explicit non-default port", func(t *testing.T) {
		client := defaultDokployClient("https://dokploy.example.com:3000").HTTPClient
		req := httptest.NewRequest(http.MethodGet, "https://dokploy.example.com:3000/api/settings/get-redirect-url", nil)
		if err := client.CheckRedirect(req, newVia("https://dokploy.example.com:3000/api/auth/sign-in/email")); err != nil {
			t.Fatalf("expected redirect to be allowed, got %v", err)
		}
	})

	t.Run("rejects ipv6 port-colliding origin", func(t *testing.T) {
		client := defaultDokployClient("https://[2001:db8::1]:8443").HTTPClient
		req := httptest.NewRequest(http.MethodGet, "https://[2001:db8::1:8443]/api/auth/sign-in/email", nil)
		if err := client.CheckRedirect(req, newVia("https://[2001:db8::1]:8443/api/auth/sign-in/email")); err == nil || !strings.Contains(err.Error(), "refusing Dokploy bootstrap redirect") {
			t.Fatalf("expected redirect refusal, got %v", err)
		}
	})
}

func TestCoolifyDBListerWarnsWhenPgpassCleanupFails(t *testing.T) {
	dir := t.TempDir()
	envPath := filepath.Join(dir, ".env")
	const testPass = "cleanup-secret-value"
	if err := os.WriteFile(envPath, []byte("DB_PASSWORD="+testPass+"\n"), 0o600); err != nil {
		t.Fatalf("write env: %v", err)
	}
	var stderr bytes.Buffer
	lister := &coolifyDBLister{
		envPath:   envPath,
		container: "coolify-db",
		stderr:    &stderr,
		runCommand: func(ctx context.Context, name string, args ...string) ([]byte, error) {
			if name == "docker" && len(args) > 2 && args[0] == "exec" && args[2] == "rm" {
				return nil, errors.New("container rm exploded")
			}
			return []byte("alice@example.com\tAlice\t$2y$10$xxx\n"), nil
		},
	}
	admins, err := lister.listAdmins(context.Background())
	if err != nil {
		t.Fatalf("listAdmins should survive cleanup failure: %v", err)
	}
	if len(admins) != 1 || admins[0].Email != "alice@example.com" {
		t.Fatalf("unexpected admins: %#v", admins)
	}
	warning := stderr.String()
	if !strings.Contains(warning, "warning") || !strings.Contains(warning, "remove pgpass file") {
		t.Fatalf("expected cleanup warning, got %q", warning)
	}
	if !strings.Contains(warning, "remove it manually") {
		t.Fatalf("expected manual removal guidance, got %q", warning)
	}
	if strings.Contains(warning, testPass) {
		t.Fatalf("warning leaked db password: %q", warning)
	}
}

func TestStageCoolifyPgpassRejectsNewlinePassword(t *testing.T) {
	secret := "line-one\nline-two"
	called := false
	_, _, err := stageCoolifyPgpass(t.Context(), func(context.Context, string, ...string) ([]byte, error) {
		called = true
		return nil, nil
	}, "coolify-db", secret)
	if err == nil {
		t.Fatal("expected newline-containing password to be rejected")
	}
	if strings.Contains(err.Error(), secret) {
		t.Fatalf("error leaked password: %v", err)
	}
	if called {
		t.Fatal("runCommand must not run for a rejected password")
	}
}

func TestStageCoolifyPgpassCpFailureCleansContainer(t *testing.T) {
	var commands []string
	_, _, err := stageCoolifyPgpass(t.Context(), func(_ context.Context, name string, args ...string) ([]byte, error) {
		commands = append(commands, name+" "+strings.Join(args, " "))
		if len(args) > 0 && args[0] == "cp" {
			return nil, errors.New("cp exploded")
		}
		return nil, nil
	}, "coolify-db", "cp-fail-secret")
	if err == nil {
		t.Fatal("expected staging error")
	}
	if strings.Contains(err.Error(), "cp-fail-secret") {
		t.Fatalf("error leaked password: %v", err)
	}
	sawCleanup := false
	for _, cmd := range commands {
		if strings.Contains(cmd, "docker exec coolify-db rm -f /tmp/bort-pgpass-") {
			sawCleanup = true
		}
		if strings.Contains(cmd, "cp-fail-secret") {
			t.Fatalf("command leaked password: %q", cmd)
		}
	}
	if !sawCleanup {
		t.Fatalf("expected container rm -f after failed cp, got %v", commands)
	}
}
