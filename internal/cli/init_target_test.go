package cli

import (
	"bytes"
	"context"
	"encoding/json"
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
			{Email: "kelvin@vela.partners", Name: "Kelvin", PasswordHash: bcryptCoolifyHash(t, "correct-horse")},
		}},
		newClient: func(string) *dokploy.Client { return defaultDokployClient(server.URL) },
		statePath: statePath,
	}

	stdin := strings.NewReader("")
	stdout := &bytes.Buffer{}
	stderr := &bytes.Buffer{}
	args := []string{
		"--coolify-email", "kelvin@vela.partners",
		"--coolify-password", "wrong-password",
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
			{Email: "kelvin@vela.partners", Name: "Kelvin Amoab", PasswordHash: bcryptCoolifyHash(t, "right-password")},
		}},
		newClient: func(string) *dokploy.Client { return defaultDokployClient(server.URL) },
		statePath: statePath,
	}

	stdout := &bytes.Buffer{}
	stderr := &bytes.Buffer{}
	args := []string{
		"--coolify-email", "kelvin@vela.partners",
		"--coolify-password", "right-password",
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
	if !strings.Contains(stdout.String(), "API key stored in .bort/state.json") {
		t.Fatalf("expected stored notice on stdout, got %q", stdout.String())
	}

	state, err := readBortState(statePath)
	if err != nil {
		t.Fatalf("read state: %v", err)
	}
	creds, ok := state.Targets["dokploy"]
	if !ok {
		t.Fatalf("expected dokploy creds in state, got %+v", state.Targets)
	}
	if creds.Token != "K-secret" || creds.URL != server.URL || creds.AdminEmail != "kelvin@vela.partners" {
		t.Fatalf("unexpected creds: %+v", creds)
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
	out := []byte("eng@vela.partners\teng\t$2y$10$abc\nkelvin@vela.partners\tKelvin Amoab\t$2y$10$def\n")
	admins, err := parsePsqlAdminRows(out)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if len(admins) != 2 {
		t.Fatalf("expected 2 admins, got %d: %#v", len(admins), admins)
	}
	if admins[0].Email != "eng@vela.partners" || admins[0].Name != "eng" || admins[0].PasswordHash != "$2y$10$abc" {
		t.Fatalf("unexpected first admin: %#v", admins[0])
	}
	if admins[1].Email != "kelvin@vela.partners" || admins[1].Name != "Kelvin Amoab" || admins[1].PasswordHash != "$2y$10$def" {
		t.Fatalf("unexpected second admin: %#v", admins[1])
	}
}

func TestCoolifyDBListerInvokesDockerExec(t *testing.T) {
	dir := t.TempDir()
	envPath := filepath.Join(dir, ".env")
	const testPass = "test-secret-value"
	if err := os.WriteFile(envPath, []byte("DB_PASSWORD="+testPass+"\n"), 0o600); err != nil {
		t.Fatalf("write env: %v", err)
	}
	captured := struct {
		name string
		args []string
	}{}
	lister := &coolifyDBLister{
		envPath:   envPath,
		container: "coolify-db",
		runCommand: func(ctx context.Context, name string, args ...string) ([]byte, error) {
			captured.name = name
			captured.args = append([]string{}, args...)
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
	if captured.name != "docker" {
		t.Fatalf("expected docker command, got %q", captured.name)
	}
	want := []string{"exec", "-e", "PGPASSWORD=" + testPass, "coolify-db", "psql", "-U", "coolify", "-d", "coolify"}
	for i, expected := range want {
		if i >= len(captured.args) || captured.args[i] != expected {
			t.Fatalf("docker exec args[:%d] = %v, want prefix %v", len(captured.args), captured.args, want)
		}
	}
}

func TestInitTargetIdempotentRerunWhenUserExists(t *testing.T) {
	stub := &dokployStub{signupExists: true}
	server := newDokployStub(t, stub)
	defer server.Close()

	statePath := filepath.Join(t.TempDir(), "state.json")
	deps := initTargetDeps{
		lister: &fakeAdminLister{admins: []coolifyAdmin{
			{Email: "kelvin@vela.partners", Name: "Kelvin", PasswordHash: bcryptCoolifyHash(t, "right-password")},
		}},
		newClient: func(string) *dokploy.Client { return defaultDokployClient(server.URL) },
		statePath: statePath,
	}

	args := []string{
		"--coolify-email", "kelvin@vela.partners",
		"--coolify-password", "right-password",
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
