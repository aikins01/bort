package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/aikins01/bort/internal/target/dokploy"
	"golang.org/x/crypto/bcrypt"
)

func serverPort(t *testing.T, server *httptest.Server) string {
	t.Helper()
	parsed, err := url.Parse(server.URL)
	if err != nil {
		t.Fatal(err)
	}
	return parsed.Port()
}

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

type blockingDokployInstaller struct {
	calls int
}

func (i *blockingDokployInstaller) InstallDokploy(ctx context.Context, _ dokployInstallOptions, _, _ io.Writer) error {
	i.calls++
	<-ctx.Done()
	return ctx.Err()
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
	userGetCalls   int
	createKeyCalls int
	deleteKeyCalls int
	lastKeyName    string
	lastKeyPrefix  string
	lastOrgID      string
	deletedKeyIDs  []string
	apiKeys        []dokploy.APIKey
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
		stub.userGetCalls++
		if !strings.Contains(r.Header.Get("Cookie"), "better-auth.session_token=session-xyz") {
			http.Error(w, "no cookie", http.StatusUnauthorized)
			return
		}
		_ = json.NewEncoder(w).Encode([]any{map[string]any{
			"result": map[string]any{"data": map[string]any{"json": map[string]any{
				"organizationId": "org-1",
				"role":           "owner",
				"user":           map[string]any{"apiKeys": stub.apiKeys},
			}}},
		}})
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
		stub.lastKeyPrefix, _ = inner["prefix"].(string)
		if md, ok := inner["metadata"].(map[string]any); ok {
			stub.lastOrgID, _ = md["organizationId"].(string)
		}
		keyID := fmt.Sprintf("key-%d", stub.createKeyCalls)
		stub.apiKeys = append(stub.apiKeys, dokploy.APIKey{ID: keyID, Name: stub.lastKeyName, Prefix: stub.lastKeyPrefix})
		key := "K-secret"
		if stub.createKeyCalls > 1 {
			key = fmt.Sprintf("K-secret-%d", stub.createKeyCalls)
		}
		_ = json.NewEncoder(w).Encode([]any{map[string]any{
			"result": map[string]any{"data": map[string]any{"json": map[string]string{"id": keyID, "key": key}}},
		}})
	})
	mux.HandleFunc("/api/trpc/user.deleteApiKey", func(w http.ResponseWriter, r *http.Request) {
		stub.deleteKeyCalls++
		if !strings.Contains(r.Header.Get("Cookie"), "better-auth.session_token=session-xyz") {
			http.Error(w, "no cookie", http.StatusUnauthorized)
			return
		}
		var payload map[string]map[string]map[string]string
		if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		apiKeyID := payload["0"]["json"]["apiKeyId"]
		stub.deletedKeyIDs = append(stub.deletedKeyIDs, apiKeyID)
		for index, key := range stub.apiKeys {
			if key.ID == apiKeyID {
				stub.apiKeys = append(stub.apiKeys[:index], stub.apiKeys[index+1:]...)
				break
			}
		}
		_, _ = w.Write([]byte(`[{"result":{"data":{"json":true}}}]`))
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

func TestInitTargetVerifiesLocalServiceBeforeSendingPassword(t *testing.T) {
	stub := &dokployStub{}
	server := newDokployStub(t, stub)
	defer server.Close()
	deps := initTargetDeps{
		lister: &fakeAdminLister{admins: []coolifyAdmin{
			{Email: "admin@example.com", Name: "Admin User", PasswordHash: bcryptCoolifyHash(t, "right-password")},
		}},
		newClient: func(string) *dokploy.Client { return defaultDokployClient(server.URL) },
		verifyLocalService: func(context.Context, *dokploy.Client) error {
			return errors.New("no running local Dokploy task")
		},
		statePath: filepath.Join(t.TempDir(), "state.json"),
	}
	t.Setenv(envCoolifyAdminPwd, "right-password")
	err := runInitTargetWith(context.Background(), []string{
		"--coolify-email", "admin@example.com",
		"--dokploy-url", server.URL,
	}, strings.NewReader(""), io.Discard, io.Discard, deps)
	if err == nil || !strings.Contains(err.Error(), "verify local Dokploy service before bootstrap") {
		t.Fatalf("expected local service verification refusal, got %v", err)
	}
	if stub.signupCalls+stub.signinCalls+stub.createKeyCalls != 0 {
		t.Fatalf("Dokploy credentials were sent before local service verification: %+v", stub)
	}
}

func TestInitTargetInstallHonorsHostSharedLiveOperationLock(t *testing.T) {
	t.Setenv(envDokployAuthBackup, filepath.Join(t.TempDir(), "dokploy-auth-secret"))
	targetLock, err := acquireDokployLiveOperationLock()
	if err != nil {
		t.Fatal(err)
	}
	defer targetLock.Release()
	installer := &fakeDokployInstaller{}
	deps := initTargetDeps{
		lister: &fakeAdminLister{admins: []coolifyAdmin{
			{Email: "admin@example.com", Name: "Admin User", PasswordHash: bcryptCoolifyHash(t, "right-password")},
		}},
		installer: installer,
		newClient: func(string) *dokploy.Client {
			t.Fatal("Dokploy client was created while another live operation held the host lock")
			return nil
		},
		statePath: filepath.Join(t.TempDir(), "state.json"),
	}
	t.Setenv(envCoolifyAdminPwd, "right-password")
	err = runInitTargetWith(context.Background(), []string{
		"--install",
		"--dokploy-url", "http://127.0.0.1:3030",
		"--coolify-email", "admin@example.com",
	}, strings.NewReader(""), io.Discard, io.Discard, deps)
	if !errors.Is(err, errDokployLiveOperationActive) {
		t.Fatalf("expected host-shared live-operation lock contention, got %v", err)
	}
	if installer.calls != 0 {
		t.Fatalf("blocked init-target mutated Dokploy installation %d time(s)", installer.calls)
	}
	requireNoDokployInstallationRecovery(t)
}

func TestInitTargetCollectsPasswordBeforeWaitingForHostLock(t *testing.T) {
	t.Setenv(envDokployAuthBackup, filepath.Join(t.TempDir(), "dokploy-auth-secret"))
	targetLock, err := acquireDokployLiveOperationLock()
	if err != nil {
		t.Fatal(err)
	}
	defer targetLock.Release()
	installer := &fakeDokployInstaller{}
	deps := initTargetDeps{
		lister: &fakeAdminLister{admins: []coolifyAdmin{
			{Email: "admin@example.com", Name: "Admin User", PasswordHash: bcryptCoolifyHash(t, "right-password")},
		}},
		installer: installer,
		newClient: func(string) *dokploy.Client {
			t.Fatal("Dokploy client was created while another live operation held the host lock")
			return nil
		},
		statePath: filepath.Join(t.TempDir(), "state.json"),
	}
	t.Setenv(envCoolifyAdminPwd, "")
	var stdout bytes.Buffer
	err = runInitTargetWith(context.Background(), []string{
		"--install",
		"--dokploy-url", "http://127.0.0.1:3030",
		"--coolify-email", "admin@example.com",
	}, strings.NewReader("right-password\n"), &stdout, io.Discard, deps)
	if !errors.Is(err, errDokployLiveOperationActive) {
		t.Fatalf("expected host-shared live-operation lock contention, got %v", err)
	}
	if !strings.Contains(stdout.String(), "Password for admin@example.com:") {
		t.Fatalf("password was not collected before lock acquisition: %q", stdout.String())
	}
	if installer.calls != 0 {
		t.Fatalf("blocked init-target mutated Dokploy installation %d time(s)", installer.calls)
	}
	requireNoDokployInstallationRecovery(t)
}

func TestStandaloneInitTargetRefusesDurableHostOwnerBeforeMutation(t *testing.T) {
	t.Setenv(envDokployAuthBackup, filepath.Join(t.TempDir(), "dokploy-auth-secret"))
	resetDokployTrafficOwner(t)
	run := migrationRun{
		Name:         "owned-migration",
		RunDir:       filepath.Join(t.TempDir(), "owned-migration"),
		CreatedAt:    time.Now().UTC(),
		BundleDigest: "digest",
	}
	if err := claimDokployHostOwnership(run, "http://127.0.0.1:3030", dokployCredentialID("migration-token")); err != nil {
		t.Fatal(err)
	}
	installer := &fakeDokployInstaller{}
	deps := initTargetDeps{
		lister: &fakeAdminLister{admins: []coolifyAdmin{
			{Email: "admin@example.com", Name: "Admin User", PasswordHash: bcryptCoolifyHash(t, "right-password")},
		}},
		installer: installer,
		newClient: func(string) *dokploy.Client {
			t.Fatal("standalone init-target created a client while the host had a durable owner")
			return nil
		},
		statePath: filepath.Join(t.TempDir(), "state.json"),
	}
	t.Setenv(envCoolifyAdminPwd, "right-password")
	err := runInitTargetWith(context.Background(), []string{
		"--install",
		"--dokploy-url", "http://127.0.0.1:3030",
		"--coolify-email", "admin@example.com",
	}, strings.NewReader(""), io.Discard, io.Discard, deps)
	if err == nil || !strings.Contains(err.Error(), fmt.Sprintf("owned by migration run %q at %q", run.Name, run.RunDir)) {
		t.Fatalf("expected durable owner refusal, got %v", err)
	}
	if installer.calls != 0 {
		t.Fatalf("blocked init-target mutated Dokploy installation %d time(s)", installer.calls)
	}
	requireNoDokployInstallationRecovery(t)
}

func TestInitTargetCanRetainAcquiredHostLockForLiveApply(t *testing.T) {
	t.Setenv(envDokployAuthBackup, filepath.Join(t.TempDir(), "dokploy-auth-secret"))
	stub := &dokployStub{}
	server := newDokployStub(t, stub)
	defer server.Close()
	installer := &fakeDokployInstaller{}
	var retained *applyLock
	revalidated := false
	deps := initTargetDeps{
		lister: &fakeAdminLister{admins: []coolifyAdmin{
			{Email: "admin@example.com", Name: "Admin User", PasswordHash: bcryptCoolifyHash(t, "right-password")},
		}},
		installer:               installer,
		newClient:               func(string) *dokploy.Client { return defaultDokployClient(server.URL) },
		statePath:               filepath.Join(t.TempDir(), "state.json"),
		retainLiveOperationLock: &retained,
		revalidateAfterTargetLock: func() error {
			revalidated = true
			second, err := acquireDokployLiveOperationLock()
			if second != nil {
				second.Release()
			}
			if !errors.Is(err, errDokployLiveOperationActive) {
				return fmt.Errorf("expected revalidation to run under host lock, got %v", err)
			}
			return nil
		},
	}
	t.Setenv(envCoolifyAdminPwd, "right-password")
	err := runInitTargetWith(context.Background(), []string{
		"--install",
		"--dokploy-url", server.URL,
		"--install-port", serverPort(t, server),
		"--coolify-email", "admin@example.com",
	}, strings.NewReader(""), io.Discard, io.Discard, deps)
	if err != nil {
		t.Fatalf("init-target with retained lock failed: %v", err)
	}
	if !revalidated || retained == nil {
		t.Fatalf("expected revalidation and retained lock, revalidated=%t retained=%v", revalidated, retained)
	}
	second, err := acquireDokployLiveOperationLock()
	if second != nil {
		second.Release()
	}
	if !errors.Is(err, errDokployLiveOperationActive) {
		t.Fatalf("setup released the retained host lock before live apply: %v", err)
	}
	retained.Release()
	retained = nil
	reacquired, err := acquireDokployLiveOperationLock()
	if err != nil {
		t.Fatalf("host lock was not released by the caller: %v", err)
	}
	reacquired.Release()
}

func TestInitTargetReusesHeldLiveOperationLock(t *testing.T) {
	t.Setenv(envDokployAuthBackup, filepath.Join(t.TempDir(), "dokploy-auth-secret"))
	targetLock, err := acquireDokployLiveOperationLock()
	if err != nil {
		t.Fatal(err)
	}
	defer targetLock.Release()
	stub := &dokployStub{}
	server := newDokployStub(t, stub)
	defer server.Close()
	installer := &fakeDokployInstaller{}
	deps := initTargetDeps{
		lister: &fakeAdminLister{admins: []coolifyAdmin{
			{Email: "admin@example.com", Name: "Admin User", PasswordHash: bcryptCoolifyHash(t, "right-password")},
		}},
		installer:         installer,
		newClient:         func(string) *dokploy.Client { return defaultDokployClient(server.URL) },
		statePath:         filepath.Join(t.TempDir(), "state.json"),
		liveOperationLock: targetLock,
	}
	t.Setenv(envCoolifyAdminPwd, "right-password")
	err = runInitTargetWith(context.Background(), []string{
		"--install",
		"--dokploy-url", server.URL,
		"--install-port", serverPort(t, server),
		"--coolify-email", "admin@example.com",
	}, strings.NewReader(""), io.Discard, io.Discard, deps)
	if err != nil {
		t.Fatalf("init-target under held live-operation lock failed: %v", err)
	}
	if installer.calls != 1 || stub.createKeyCalls != 1 {
		t.Fatalf("expected setup to complete under inherited lock, installer=%d api_keys=%d", installer.calls, stub.createKeyCalls)
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
	if stub.signupCalls != 1 || stub.signinCalls != 1 || stub.userGetCalls != 1 || stub.createKeyCalls != 1 {
		t.Fatalf("unexpected call counts: %+v", stub)
	}
	if stub.lastOrgID != "org-1" || stub.lastKeyName != defaultDokployAPIName || stub.lastKeyPrefix != "" {
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
	authBackup := filepath.Join(t.TempDir(), "dokploy-auth-secret")
	t.Setenv(envDokployAuthBackup, authBackup)
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
		"--install-port", serverPort(t, server),
		"--swarm-addr-pool", "172.29.0.0/16",
		"--dokploy-version", "v0.30.7",
		"--endpoint-mode", "dnsrr",
		"--coolify-email", "admin@example.com",
		"--dokploy-url", server.URL,
		"--name", "Explicit Admin",
		"--api-key-name", "migration key",
	}
	if err := runInitTargetWith(context.Background(), args, strings.NewReader(""), &bytes.Buffer{}, &bytes.Buffer{}, deps); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if installer.calls != 1 {
		t.Fatalf("expected installer to run once, got %d", installer.calls)
	}
	wantVersion := defaultDokployVersion + "@" + defaultDokployDigest
	if installer.opts.HostPort != serverPort(t, server) || installer.opts.AddrPool != "172.29.0.0/16" || installer.opts.Version != wantVersion || installer.opts.EndpointMode != "dnsrr" || installer.opts.ACMEEmail != "admin@example.com" || installer.opts.AuthSecretBackup != authBackup || installer.opts.AdminName != "Explicit Admin" || installer.opts.APIKeyName != "migration key" {
		t.Fatalf("unexpected install opts: %#v", installer.opts)
	}
	if stub.signupCalls != 1 || stub.signinCalls != 1 || stub.userGetCalls != 1 || stub.createKeyCalls != 1 {
		t.Fatalf("expected bootstrap after install, got %+v", stub)
	}
}

type foreignServiceDokployInstaller struct {
	calls int
}

func (i *foreignServiceDokployInstaller) InstallDokploy(_ context.Context, _ dokployInstallOptions, _, stderr io.Writer) error {
	i.calls++
	fmt.Fprintln(stderr, "existing Dokploy service was not installed by Bort, so --install cannot adopt or repair it; it answers at http://127.0.0.1:3000")
	return exec.Command("sh", "-c", "exit 75").Run()
}

func TestInitTargetInstallForeignServiceRefusalClearsRecoveryMarker(t *testing.T) {
	t.Cleanup(func() { _ = clearDokployInstallationRecoveryRequired() })
	t.Setenv(envDokployAuthBackup, filepath.Join(t.TempDir(), "dokploy-auth-secret"))
	stub := &dokployStub{}
	server := newDokployStub(t, stub)
	defer server.Close()
	installer := &foreignServiceDokployInstaller{}
	deps := initTargetDeps{
		lister: &fakeAdminLister{admins: []coolifyAdmin{
			{Email: "admin@example.com", Name: "Admin User", PasswordHash: bcryptCoolifyHash(t, "right-password")},
		}},
		installer: installer,
		newClient: func(string) *dokploy.Client { return defaultDokployClient(server.URL) },
		statePath: filepath.Join(t.TempDir(), "state.json"),
	}
	t.Setenv(envCoolifyAdminPwd, "right-password")
	t.Setenv("SUDO_UID", "1000")
	err := runInitTargetWith(context.Background(), []string{
		"--install",
		"--coolify-email", "admin@example.com",
		"--dokploy-url", server.URL,
		"--install-port", serverPort(t, server),
	}, strings.NewReader(""), io.Discard, io.Discard, deps)
	if err == nil || !strings.Contains(err.Error(), "not installed by Bort") || !strings.Contains(err.Error(), "sudo bort init-target --dokploy-url") || strings.Contains(err.Error(), "blocks other host mutations") {
		t.Fatalf("expected foreign-service refusal with the recorded command prefix, got %v", err)
	}
	if installer.calls != 1 || stub.signupCalls != 0 {
		t.Fatalf("unexpected calls after refusal: installer=%d signup=%d", installer.calls, stub.signupCalls)
	}
	if blocked, err := dokployInstallationRecoveryRequired(); err != nil || blocked {
		t.Fatalf("refusing a foreign service left installation recovery required: blocked=%t err=%v", blocked, err)
	}
	if err := runInitTargetWith(context.Background(), []string{
		"--coolify-email", "admin@example.com",
		"--dokploy-url", server.URL,
	}, strings.NewReader(""), io.Discard, io.Discard, deps); err != nil {
		t.Fatalf("suggested non-install bootstrap was blocked: %v", err)
	}
	if installer.calls != 1 || stub.signupCalls != 1 {
		t.Fatalf("non-install bootstrap did not run against the existing service: installer=%d signup=%d", installer.calls, stub.signupCalls)
	}
}

func TestInitTargetInstallFailureKeepsRecoveryMarker(t *testing.T) {
	t.Cleanup(func() { _ = clearDokployInstallationRecoveryRequired() })
	t.Setenv(envDokployAuthBackup, filepath.Join(t.TempDir(), "dokploy-auth-secret"))
	stub := &dokployStub{}
	server := newDokployStub(t, stub)
	defer server.Close()
	deps := initTargetDeps{
		lister: &fakeAdminLister{admins: []coolifyAdmin{
			{Email: "admin@example.com", Name: "Admin User", PasswordHash: bcryptCoolifyHash(t, "right-password")},
		}},
		installer: &failingDokployInstaller{err: exec.Command("sh", "-c", "exit 1").Run()},
		newClient: func(string) *dokploy.Client { return defaultDokployClient(server.URL) },
		statePath: filepath.Join(t.TempDir(), "state.json"),
	}
	t.Setenv(envCoolifyAdminPwd, "right-password")
	err := runInitTargetWith(context.Background(), []string{
		"--install",
		"--coolify-email", "admin@example.com",
		"--dokploy-url", server.URL,
		"--install-port", serverPort(t, server),
	}, strings.NewReader(""), io.Discard, io.Discard, deps)
	if err == nil || !strings.Contains(err.Error(), "install dokploy: exit status 1") {
		t.Fatalf("expected generic installer failure, got %v", err)
	}
	if blocked, err := dokployInstallationRecoveryRequired(); err != nil || !blocked {
		t.Fatalf("a mutating installer failure must keep recovery required: blocked=%t err=%v", blocked, err)
	}
}

type failingDokployInstaller struct {
	err error
}

func (i *failingDokployInstaller) InstallDokploy(context.Context, dokployInstallOptions, io.Writer, io.Writer) error {
	return i.err
}

func TestInitTargetInstallRetryReplacesOrphanedAPIKeyAfterStateWriteFailure(t *testing.T) {
	t.Cleanup(func() { _ = clearDokployInstallationRecoveryRequired() })
	authBackup := filepath.Join(t.TempDir(), "dokploy-auth-secret")
	t.Setenv(envDokployAuthBackup, authBackup)
	stub := &dokployStub{apiKeys: []dokploy.APIKey{{ID: "unrelated-key", Name: "other tool"}}}
	server := newDokployStub(t, stub)
	defer server.Close()
	installer := &fakeDokployInstaller{}
	blockedParent := filepath.Join(t.TempDir(), "not-a-directory")
	if err := os.WriteFile(blockedParent, []byte("block state writes"), 0o600); err != nil {
		t.Fatal(err)
	}
	deps := initTargetDeps{
		lister: &fakeAdminLister{admins: []coolifyAdmin{
			{Email: "admin@example.com", Name: "Admin User", PasswordHash: bcryptCoolifyHash(t, "right-password")},
		}},
		installer: installer,
		newClient: func(string) *dokploy.Client { return defaultDokployClient(server.URL) },
		statePath: filepath.Join(blockedParent, "state.json"),
	}
	t.Setenv(envCoolifyAdminPwd, "right-password")
	args := []string{
		"--install",
		"--coolify-email", "admin@example.com",
		"--dokploy-url", server.URL,
		"--install-port", serverPort(t, server),
	}
	if err := runInitTargetWith(context.Background(), args, strings.NewReader(""), io.Discard, io.Discard, deps); err == nil {
		t.Fatal("expected state persistence failure after API-key creation")
	}
	recovery, found, err := readDokployInstallationRecovery()
	if err != nil || !found {
		t.Fatalf("read pending API-key recovery: found=%t err=%v", found, err)
	}
	if recovery.Phase != dokployInstallAPIKey || !slices.Equal(recovery.APIKeyBaselineIDs, []string{"unrelated-key"}) {
		t.Fatalf("unexpected pending API-key recovery: %#v", recovery)
	}
	if stub.userGetCalls != 1 || stub.createKeyCalls != 1 || stub.deleteKeyCalls != 0 {
		t.Fatalf("unexpected first-attempt API calls: user.get=%d create=%d delete=%d", stub.userGetCalls, stub.createKeyCalls, stub.deleteKeyCalls)
	}
	stub.apiKeys = append(stub.apiKeys, dokploy.APIKey{ID: "concurrent-key", Name: defaultDokployAPIName, Prefix: "manual_prefix"})

	deps.statePath = filepath.Join(t.TempDir(), "state.json")
	if err := runInitTargetWith(context.Background(), args, strings.NewReader(""), io.Discard, io.Discard, deps); err != nil {
		t.Fatalf("retry interrupted install: %v", err)
	}
	if installer.calls != 1 || stub.userGetCalls != 2 || stub.createKeyCalls != 2 || stub.deleteKeyCalls != 1 || !slices.Equal(stub.deletedKeyIDs, []string{"key-1"}) {
		t.Fatalf("retry did not replace only the orphaned key with one user.get per attempt: installer=%d user.get=%d create=%d delete=%d deleted=%v", installer.calls, stub.userGetCalls, stub.createKeyCalls, stub.deleteKeyCalls, stub.deletedKeyIDs)
	}
	if len(stub.apiKeys) != 3 || stub.apiKeys[0].ID != "unrelated-key" || stub.apiKeys[1].ID != "concurrent-key" || stub.apiKeys[2].ID != "key-2" || stub.apiKeys[2].Name != defaultDokployAPIName || stub.apiKeys[2].Prefix != dokployInstallAPIKeyPrefix(recovery) {
		t.Fatalf("unexpected active API keys after retry: %#v", stub.apiKeys)
	}
	state, err := readBortState(deps.statePath)
	if err != nil {
		t.Fatal(err)
	}
	if got := state.Targets["dokploy"].Token; got != "K-secret-2" {
		t.Fatalf("persisted API key=%q, want replacement key", got)
	}
	if _, found, err := readDokployInstallationRecovery(); err != nil || found {
		t.Fatalf("API-key recovery marker survived successful persistence: found=%t err=%v", found, err)
	}
}

func TestPrepareDokployAPIKeyRecoveryPreservesUnownedMatchingKey(t *testing.T) {
	stub := &dokployStub{}
	server := newDokployStub(t, stub)
	defer server.Close()
	recovery := newDokployInstallationRecovery(server.URL, dokployInstallOptions{
		HostPort:         "3030",
		AddrPool:         "auto",
		Version:          defaultDokployVersion + "@" + defaultDokployDigest,
		EndpointMode:     "dnsrr",
		ACMEEmail:        "admin@example.com",
		AuthSecretBackup: filepath.Join(t.TempDir(), "auth-secret"),
		AdminName:        "Admin User",
		APIKeyName:       defaultDokployAPIName,
	})
	recovery.Phase = dokployInstallAPIKey
	recovery.APIKeyBaselineIDs = []string{"baseline-key"}
	matching := dokploy.APIKey{ID: "operator-key", Name: recovery.APIKeyName, Prefix: dokployInstallAPIKeyPrefix(recovery)}
	client := defaultDokployClient(server.URL)
	_, err := prepareDokployAPIKeyRecovery(context.Background(), client, "session-xyz", recovery, []dokploy.APIKey{
		{ID: "baseline-key", Name: "existing"},
		matching,
	})
	if err == nil || !strings.Contains(err.Error(), "no created key ID was durably recorded") {
		t.Fatalf("expected unowned matching key refusal, got %v", err)
	}
	if stub.deleteKeyCalls != 0 {
		t.Fatalf("unowned matching API key was deleted: %#v", stub.deletedKeyIDs)
	}
}

func TestPrepareDokployAPIKeyRecoveryClearsDeletedIdentityBeforeReplacement(t *testing.T) {
	t.Cleanup(func() { _ = clearDokployInstallationRecoveryRequired() })
	stub := &dokployStub{}
	server := newDokployStub(t, stub)
	defer server.Close()
	recovery := newDokployInstallationRecovery(server.URL, dokployInstallOptions{
		HostPort:         "3030",
		AddrPool:         "auto",
		Version:          defaultDokployVersion + "@" + defaultDokployDigest,
		EndpointMode:     "dnsrr",
		ACMEEmail:        "admin@example.com",
		AuthSecretBackup: filepath.Join(t.TempDir(), "auth-secret"),
		AdminName:        "Admin User",
		APIKeyName:       defaultDokployAPIName,
	})
	recovery.Phase = dokployInstallAPIKey
	recovery.APIKeyBaselineIDs = []string{"baseline-key"}
	recovery.APIKeyCreatedID = "key-1"
	if err := markDokployInstallationRecoveryRequired(recovery); err != nil {
		t.Fatal(err)
	}
	client := defaultDokployClient(server.URL)
	updated, err := prepareDokployAPIKeyRecovery(context.Background(), client, "session-xyz", recovery, []dokploy.APIKey{
		{ID: "baseline-key", Name: "existing"},
		{ID: "key-1", Name: recovery.APIKeyName, Prefix: dokployInstallAPIKeyPrefix(recovery)},
	})
	if err != nil {
		t.Fatal(err)
	}
	if updated.APIKeyCreatedID != "" || !slices.Equal(stub.deletedKeyIDs, []string{"key-1"}) {
		t.Fatalf("recorded key replacement was not reconciled: recovery=%#v deleted=%v", updated, stub.deletedKeyIDs)
	}
	persisted, found, err := readDokployInstallationRecovery()
	if err != nil || !found || persisted.APIKeyCreatedID != "" {
		t.Fatalf("cleared key identity was not durable: found=%t recovery=%#v err=%v", found, persisted, err)
	}
	_, err = prepareDokployAPIKeyRecovery(context.Background(), client, "session-xyz", updated, []dokploy.APIKey{
		{ID: "baseline-key", Name: "existing"},
		{ID: "key-2", Name: recovery.APIKeyName, Prefix: dokployInstallAPIKeyPrefix(recovery)},
	})
	if err == nil || !strings.Contains(err.Error(), "no created key ID was durably recorded") {
		t.Fatalf("unrecorded replacement key did not block another creation: %v", err)
	}
	if !slices.Equal(stub.deletedKeyIDs, []string{"key-1"}) {
		t.Fatalf("unowned replacement key was deleted: %v", stub.deletedKeyIDs)
	}
}

func TestInitTargetInstallDefaultsSwarmAddrPoolToAuto(t *testing.T) {
	t.Setenv(envDokployAuthBackup, filepath.Join(t.TempDir(), "dokploy-auth-secret"))
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
		"--install-port", serverPort(t, server),
	}
	if err := runInitTargetWith(context.Background(), args, strings.NewReader(""), &bytes.Buffer{}, &bytes.Buffer{}, deps); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if installer.opts.AddrPool != "auto" {
		t.Fatalf("expected default addr pool auto, got %#v", installer.opts)
	}
}

func TestInitTargetInstallPromptsAndVerifiesPasswordBeforeInstaller(t *testing.T) {
	t.Setenv(envDokployAuthBackup, filepath.Join(t.TempDir(), "dokploy-auth-secret"))
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
		"--install-port", serverPort(t, server),
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
	t.Setenv(envDokployAuthBackup, filepath.Join(t.TempDir(), "dokploy-auth-secret"))
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
		"--install-port", serverPort(t, server),
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

func TestNormalizeSwarmAddressPool(t *testing.T) {
	valid := map[string]string{
		"auto":            "auto",
		" AUTO ":          "auto",
		"172.29.4.8/16":   "172.29.0.0/16",
		"192.168.20.0/24": "192.168.20.0/24",
	}
	for input, want := range valid {
		got, err := normalizeSwarmAddressPool(input)
		if err != nil || got != want {
			t.Fatalf("normalizeSwarmAddressPool(%q) = %q, %v; want %q", input, got, err, want)
		}
	}
	for _, input := range []string{"", "not-a-cidr", "172.29.0.0", "172.29.0.0/25", "fd00::/64"} {
		if _, err := normalizeSwarmAddressPool(input); err == nil {
			t.Fatalf("expected address pool %q to be rejected", input)
		}
	}
}

func TestValidateDokployAPIKeyName(t *testing.T) {
	for _, name := range []string{"a", strings.Repeat("k", 32)} {
		if err := validateDokployAPIKeyName(name); err != nil {
			t.Fatalf("expected API-key name %q to be valid: %v", name, err)
		}
	}
	for _, name := range []string{"", strings.Repeat("k", 33)} {
		if err := validateDokployAPIKeyName(name); err == nil {
			t.Fatalf("expected API-key name %q to be rejected", name)
		}
	}
}

func TestValidateDokployVersionTag(t *testing.T) {
	valid := []string{"v0.29.3", "v0.30.7-beta_1", "v1.0.0"}
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
		"feature",
		"feature-amd64",
		"latest",
		"canary",
		"_rc2",
		"0.29.3",
		strings.Repeat("a", 128),
		"v" + strings.Repeat("9", 30) + ".0.0",
		strings.Repeat("a", 129),
	}
	for _, tag := range invalid {
		if err := validateDokployVersionTag(tag); err == nil {
			t.Fatalf("expected tag %q to be rejected", tag)
		}
	}
	unsupported := []string{"v0.26.6", "0.29.2", "v0.29.2-rc.1", "v0.29.3-rc.1"}
	for _, tag := range unsupported {
		err := validateDokployVersionTag(tag)
		if err == nil || !strings.Contains(err.Error(), "v0.29.3 or newer") {
			t.Fatalf("expected tag %q to be rejected as unsupported, got %v", tag, err)
		}
	}
}

func TestNormalizeDokployVersionRequiresReviewedDigest(t *testing.T) {
	wantDefault := defaultDokployVersion + "@" + defaultDokployDigest
	if got, err := normalizeDokployVersion(defaultDokployVersion); err != nil || got != wantDefault {
		t.Fatalf("default version normalized to %q, %v; want %q", got, err, wantDefault)
	}
	digest := "sha256:" + strings.Repeat("a", 64)
	if got, err := normalizeDokployVersion("v0.30.8@" + digest); err != nil || got != "v0.30.8@"+digest {
		t.Fatalf("digest-qualified version normalized to %q, %v", got, err)
	}
	for _, version := range []string{
		"v0.30.8",
		"v0.30.7@sha256:abcdef",
		"v0.30.7@sha256:" + strings.Repeat("A", 64),
		"v0.30.7@sha512:" + strings.Repeat("a", 64),
		"v0.30.7@" + digest + "@" + digest,
	} {
		if _, err := normalizeDokployVersion(version); err == nil {
			t.Fatalf("expected version %q to be rejected", version)
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

func TestInitTargetInstallRejectsURLPortMismatchBeforeRecoveryMarker(t *testing.T) {
	installer := &fakeDokployInstaller{}
	deps := initTargetDeps{
		lister:    &fakeAdminLister{},
		installer: installer,
		statePath: filepath.Join(t.TempDir(), "state.json"),
	}
	t.Setenv(envDokployAuthBackup, filepath.Join(t.TempDir(), "dokploy-auth-secret"))
	args := []string{"--install", "--dokploy-url", "http://127.0.0.1:4040"}
	err := runInitTargetWith(context.Background(), args, strings.NewReader(""), io.Discard, io.Discard, deps)
	if err == nil || !strings.Contains(err.Error(), "port 4040 does not match --install-port 3030") {
		t.Fatalf("expected install port mismatch refusal, got %v", err)
	}
	if installer.calls != 0 {
		t.Fatalf("installer ran despite port mismatch")
	}
	if _, found, readErr := dokployInstallationRecoveryCommand(); readErr != nil || found {
		t.Fatalf("recovery marker written before refusal: found=%v err=%v", found, readErr)
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

func TestInitTargetInstallRejectsInvalidEndpointMode(t *testing.T) {
	installer := &fakeDokployInstaller{}
	deps := initTargetDeps{
		lister:    &fakeAdminLister{},
		installer: installer,
		statePath: filepath.Join(t.TempDir(), "state.json"),
	}
	args := []string{"--install", "--endpoint-mode", "ingress", "--dokploy-url", "http://127.0.0.1:3030"}
	err := runInitTargetWith(context.Background(), args, strings.NewReader(""), io.Discard, io.Discard, deps)
	if err == nil || !strings.Contains(err.Error(), "endpoint-mode") {
		t.Fatalf("expected endpoint-mode validation error, got %v", err)
	}
	if installer.calls != 0 {
		t.Fatalf("installer ran despite invalid endpoint mode")
	}
}

func TestValidateAuthSecretBackupPath(t *testing.T) {
	if err := validateAuthSecretBackupPath(""); err == nil || !strings.Contains(err.Error(), envDokployAuthBackup) {
		t.Fatalf("expected missing escrow refusal, got %v", err)
	}
	if err := validateAuthSecretBackupPath("relative/secret"); err == nil || !strings.Contains(err.Error(), "absolute path") {
		t.Fatalf("expected relative escrow refusal, got %v", err)
	}
	if err := validateAuthSecretBackupPath(filepath.Join(t.TempDir(), "dokploy-auth-secret")); err != nil {
		t.Fatalf("expected absolute escrow path to be accepted, got %v", err)
	}
}

func TestInitTargetInstallRejectsUnsafeACMEEmail(t *testing.T) {
	t.Setenv(envDokployAuthBackup, filepath.Join(t.TempDir(), "dokploy-auth-secret"))
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
	args := []string{"--install", "--coolify-email", "a#b@example.com", "--dokploy-url", server.URL, "--install-port", serverPort(t, server)}
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

func TestDokployShadowInstallUsesStableAuthenticationSecret(t *testing.T) {
	bash, err := exec.LookPath("bash")
	if err != nil {
		t.Skip("bash is unavailable")
	}
	syntax := exec.Command(bash, "-n")
	syntax.Stdin = strings.NewReader(dokployShadowInstallScript)
	if out, err := syntax.CombinedOutput(); err != nil {
		t.Fatalf("install script syntax is invalid: %v: %s", err, out)
	}
	for _, want := range []string{
		`docker secret inspect dokploy_auth_secret >/dev/null 2>&1`,
		`openssl rand -hex 32`,
		`DOKPLOY_AUTH_SECRET_BACKUP`,
		`AUTH_SECRET_PROVENANCE=/var/lib/bort/dokploy-auth-secret.id`,
		`AUTH_SECRET_FD_PATH="/dev/fd/$AUTH_SECRET_FD"`,
		`AUTH_SECRET_INTENT_LABEL=io.bort.auth-secret-intent`,
		`INSTALL_AUTH_SECRET_LABEL=io.bort.install-auth-secret-id`,
		`write_auth_secret_intent`,
		`validate_auth_secret_provenance "$DOKPLOY_AUTH_SECRET_CREATED_ID"`,
		`validate_auth_secret_provenance`,
		`docker secret create --label "$AUTH_SECRET_INTENT_LABEL=$DOKPLOY_AUTH_SECRET_INTENT" dokploy_auth_secret -`,
		`dokploy_auth_secret - < "$AUTH_SECRET_FD_PATH"`,
		`--secret source=dokploy_auth_secret,target=/run/secrets/dokploy_auth_secret`,
		`--replicas 0`,
		`DOKPLOY_BOUND_AUTH_SECRET_IDS`,
		`DOKPLOY_STAGED_SERVICE`,
		`the $service_state Dokploy service is not bound to the reviewed Bort authentication-secret identity`,
		`docker service scale dokploy=1`,
		`-e BETTER_AUTH_SECRET_FILE=/run/secrets/dokploy_auth_secret`,
		`BETTER_AUTH_SECRET=better-auth-secret-123456789`,
		`^BETTER_AUTH_SECRET=[0-9a-f]{64}$`,
		`cryptographically random 32-byte key`,
		`^BETTER_AUTH_SECRET_FILE=.+$`,
		`refusing to generate a new key because changing the legacy fallback can make encrypted settings unreadable`,
		`better-auth-secret-123456789`,
		`dokploy:db-encryption:v1`,
		`/etc/dokploy/encryption.key`,
		`fence every inbound Dokploy UI/API path`,
		`published host port targeting container port 3000`,
		`defaults to host port 3030`,
		`validate and preserve every existing 64-lowercase-hex line`,
		`atomically add HMAC-SHA256`,
		`keep the fence active through the service restart`,
		`hosted 0.29.3.sh wrapper`,
		`upgrade Dokploy to v0.29.5 or newer`,
		`verify the running service image`,
		`120-second migration timeout as success`,
		`--secret-add source=dokploy-auth-secret,target=/run/secrets/dokploy-auth-secret`,
		`pnpm run migrate-auth-secret`,
		`refusing to run migration while Docker secret dokploy-auth-secret already exists`,
		`DOKPLOY_AUTH_SECRET_BACKUP`,
		`mktemp "${TMPDIR:-/tmp}/dokploy-auth-secret.XXXXXX"`,
		`sync -f "$DOKPLOY_AUTH_SECRET_BACKUP"`,
		`cmp -s -- "$auth_secret_tmp" "$DOKPLOY_AUTH_SECRET_BACKUP"`,
		`sudo docker service update --force --update-order stop-first --detach=false dokploy`,
		`SELECT count(*) FROM pg_stat_activity`,
		`sudo docker secret create dokploy-auth-secret - < "$auth_secret_tmp"`,
		`shred -u -- "$auth_secret_tmp"`,
		`Retain DOKPLOY_AUTH_SECRET_BACKUP and DOKPLOY_TWO_FACTOR_BEFORE after success`,
		`DOKPLOY_TWO_FACTOR_BEFORE`,
		`public.two_factor`,
		`compare-and-swap recovery`,
		`DATABASE_URL is configured`,
		`use a database-operator procedure`,
		`do not restore the full PostgreSQL database`,
		`still uses the legacy fallback`,
		`verify representative application, Compose, project, environment, database, build-argument, and build-secret values render as plaintext`,
		`re-encrypts only Better Auth two-factor secret and backup-code fields`,
		`dokploy_image_supports_auth_secret_file`,
		`existing Dokploy database state has no reusable dokploy_auth_secret`,
		`Bort does not accept a manually recreated secret even when its key bytes match`,
	} {
		if !strings.Contains(dokployShadowInstallScript, want) {
			t.Fatalf("install script missing authentication-secret fragment %q", want)
		}
	}
	if strings.Contains(dokployShadowInstallScript, `-e BETTER_AUTH_SECRET="$AUTH_SECRET"`) {
		t.Fatal("authentication secret must not be stored in the service environment")
	}
	if strings.Contains(dokployShadowInstallScript, `dokploy_auth_secret - < "$AUTH_SECRET_BACKUP"`) {
		t.Fatal("fresh authentication secret must be created from the validated descriptor, not a reopened path")
	}
	if strings.Contains(dokployShadowInstallScript, `curl -fsSL https://dokploy.com/security/0.29.3.sh`) {
		t.Fatal("unsafe timeout-based authentication migration wrapper must not be recommended")
	}
	if strings.Contains(dokployShadowInstallScript, `openssl rand -hex 32 | sudo docker secret create dokploy-auth-secret -`) {
		t.Fatal("legacy authentication migration creates the Docker secret before escrowing the generated value")
	}
	restart := strings.Index(dokployShadowInstallScript, `sudo docker service update --force --update-order stop-first --detach=false dokploy`)
	snapshot := strings.Index(dokployShadowInstallScript, `COPY (SELECT id, secret, backup_codes FROM public.two_factor ORDER BY id)`)
	if restart < 0 || snapshot < 0 || restart >= snapshot {
		t.Fatal("legacy authentication migration must quiesce and settle Dokploy before its authoritative two-factor snapshot")
	}
	escrow := strings.Index(dokployShadowInstallScript, `sync -f "$DOKPLOY_AUTH_SECRET_BACKUP"`)
	verified := strings.Index(dokployShadowInstallScript, `cmp -s -- "$auth_secret_tmp" "$DOKPLOY_AUTH_SECRET_BACKUP"`)
	secretCreated := strings.Index(dokployShadowInstallScript, `sudo docker secret create dokploy-auth-secret - < "$auth_secret_tmp"`)
	migrated := strings.Index(dokployShadowInstallScript, `pnpm run migrate-auth-secret`)
	if escrow < 0 || verified <= escrow || secretCreated <= verified || migrated <= secretCreated {
		t.Fatalf("legacy authentication migration must sync and verify escrow before Docker secret creation and database migration: escrow=%d verified=%d created=%d migrated=%d", escrow, verified, secretCreated, migrated)
	}
	zeroReplicas := strings.Index(dokployShadowInstallScript, `--replicas 0`)
	boundIdentity := strings.Index(dokployShadowInstallScript, `DOKPLOY_BOUND_AUTH_SECRET_IDS=`)
	startTask := strings.Index(dokployShadowInstallScript, `docker service scale dokploy=1`)
	if zeroReplicas < 0 || boundIdentity <= zeroReplicas || startTask <= boundIdentity {
		t.Fatalf("Dokploy must bind the named secret at zero replicas, verify its ID, then start a task: zero=%d bound=%d start=%d", zeroReplicas, boundIdentity, startTask)
	}

	functionsStart := strings.Index(dokployShadowInstallScript, "validate_auth_secret_backup() {")
	functionsEnd := strings.Index(dokployShadowInstallScript, "\nDOKPLOY_AUTH_SECRET_EXISTS=false")
	setupStart := strings.LastIndex(dokployShadowInstallScript, `if [ "$DOKPLOY_SERVICE_EXISTS" = false ]; then`)
	setupEnd := strings.Index(dokployShadowInstallScript[setupStart:], `log "Starting Dokploy Postgres"`)
	if functionsStart < 0 || functionsEnd < functionsStart || setupStart < 0 || setupEnd < 0 {
		t.Fatal("missing fresh authentication-secret functions or setup block")
	}
	functions := dokployShadowInstallScript[functionsStart:functionsEnd]
	setup := dokployShadowInstallScript[setupStart : setupStart+setupEnd]
	dir := t.TempDir()
	statePath := filepath.Join(dir, "secret")
	backupPath := filepath.Join(dir, "escrow")
	provenancePath := filepath.Join(dir, "provenance", "dokploy-auth-secret.id")
	createCountPath := filepath.Join(dir, "create-count")
	randomCountPath := filepath.Join(dir, "random-count")
	const secret = "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"
	const secretID = "abcdefghijklmnopqrstuvwxy"
	script := `set -euo pipefail
DOKPLOY_SERVICE_EXISTS=false
STATE_PATH="$1"
AUTH_SECRET_BACKUP="$2"
AUTH_SECRET_BACKUP_DIR="$(dirname -- "$AUTH_SECRET_BACKUP")"
AUTH_SECRET_PROVENANCE="$3"
CREATE_COUNT_PATH="$4"
RANDOM_COUNT_PATH="$5"
FAIL_AFTER_CREATE="$6"
COLLIDE_ON_CREATE="$7"
AUTH_SECRET_INTENT_LABEL=io.bort.auth-secret-intent
mkdir -p "$AUTH_SECRET_BACKUP_DIR"
if [ ! -e "$AUTH_SECRET_BACKUP" ]; then
	printf '%s\n' '` + secret + `' > "$AUTH_SECRET_BACKUP"
	chmod 600 "$AUTH_SECRET_BACKUP"
fi
exec 3< "$AUTH_SECRET_BACKUP"
AUTH_SECRET_FD=3
AUTH_SECRET_FD_PATH="/dev/fd/$AUTH_SECRET_FD"
AUTH_SECRET_DIGEST='` + strings.Repeat("a", 64) + `'
log() { printf 'step: %s\n' "$*"; }
sync() { :; }
stat() {
    case "$2" in
        %a) if [ "$3" = "$(dirname -- "$AUTH_SECRET_PROVENANCE")" ]; then printf '700\n'; else printf '600\n'; fi ;;
        %u) id -u ;;
        *) return 1 ;;
    esac
}
sha256sum() { printf '%s  %s\n' '` + strings.Repeat("a", 64) + `' "$2"; }
docker() {
    if [ "$1 $2 $3" = "secret inspect dokploy_auth_secret" ]; then
        if [ ! -f "$STATE_PATH" ]; then
			return 1
		fi
		if [ "${4:-}" = "--format" ]; then
			case "${5:-}" in
				*Annotations.Labels*) printf '%s\n' '0123456789abcdef0123456789abcdef' ;;
				*) printf '%s\n' '` + secretID + `' ;;
			esac
		fi
        return
    fi
	if [ "$1 $2 $3" = "secret create --label" ] && [ "$4" = "$AUTH_SECRET_INTENT_LABEL=0123456789abcdef0123456789abcdef" ] && [ "$5 $6" = "dokploy_auth_secret -" ]; then
        cat >"$STATE_PATH"
		if [ "$COLLIDE_ON_CREATE" = 1 ]; then
			return 1
		fi
        printf 'create\n' >>"$CREATE_COUNT_PATH"
		if [ "$FAIL_AFTER_CREATE" = 1 ]; then
			kill -9 $$
		fi
		printf '%s\n' '` + secretID + `'
        return
    fi
    printf 'unexpected docker arguments' >&2
    return 1
}
openssl() {
	[ "$1 $2" = "rand -hex" ] || return 1
	case "$3" in
		32) printf 'random-secret\n' >>"$RANDOM_COUNT_PATH"; printf '%s\n' '` + secret + `' ;;
		16) printf 'random-intent\n' >>"$RANDOM_COUNT_PATH"; printf '%s\n' '0123456789abcdef0123456789abcdef' ;;
		*) return 1 ;;
	esac
}
` + functions + `
prepare_auth_secret_backup
` + setup
	collisionDir := filepath.Join(dir, "collision")
	if err := os.Mkdir(collisionDir, 0o700); err != nil {
		t.Fatal(err)
	}
	collisionState := filepath.Join(collisionDir, "secret")
	collisionBackup := filepath.Join(collisionDir, "escrow")
	collisionProvenance := filepath.Join(collisionDir, "provenance", "dokploy-auth-secret.id")
	collisionCreates := filepath.Join(collisionDir, "create-count")
	collisionRandom := filepath.Join(collisionDir, "random-count")
	cmd := exec.Command(bash, "-c", script, "test", collisionState, collisionBackup, collisionProvenance, collisionCreates, collisionRandom, "0", "1")
	out, err := cmd.CombinedOutput()
	if err == nil || !strings.Contains(string(out), "refusing to adopt any object that raced its creation") {
		t.Fatalf("expected colliding authentication-secret creation refusal, got %v: %s", err, out)
	}
	collisionMarker, err := os.ReadFile(collisionProvenance)
	if err != nil {
		t.Fatalf("read collision provenance marker: %v", err)
	}
	if !strings.HasPrefix(string(collisionMarker), "pending:") || strings.HasPrefix(string(collisionMarker), secretID+" ") {
		t.Fatalf("colliding Docker secret was finalized as Bort provenance: %q", collisionMarker)
	}

	cmd = exec.Command(bash, "-c", script, "test", statePath, backupPath, provenancePath, createCountPath, randomCountPath, "1", "0")
	out, err = cmd.CombinedOutput()
	if err == nil {
		t.Fatal("expected interruption after Docker secret creation")
	}
	pendingMarker, err := os.ReadFile(provenancePath)
	if err != nil {
		t.Fatalf("read pending provenance marker: %v", err)
	}
	if !strings.HasPrefix(string(pendingMarker), "pending:0123456789abcdef0123456789abcdef ") {
		t.Fatalf("creation intent was not durable before Docker secret creation: %q", pendingMarker)
	}
	if err := os.WriteFile(provenancePath, []byte("pending:fedcba9876543210fedcba9876543210 "+strings.Repeat("a", 64)+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	cmd = exec.Command(bash, "-c", script, "test", statePath, backupPath, provenancePath, createCountPath, randomCountPath, "0", "0")
	out, err = cmd.CombinedOutput()
	if err == nil || !strings.Contains(string(out), "does not match the trusted Bort creation intent") {
		t.Fatalf("expected mismatched pending creation intent refusal, got %v: %s", err, out)
	}
	if err := os.WriteFile(provenancePath, pendingMarker, 0o600); err != nil {
		t.Fatal(err)
	}
	cmd = exec.Command(bash, "-c", script, "test", statePath, backupPath, provenancePath, createCountPath, randomCountPath, "0", "0")
	out, err = cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("authentication-secret retry failed: %v: %s", err, out)
	}
	stored, err := os.ReadFile(statePath)
	if err != nil {
		t.Fatalf("read stored secret: %v", err)
	}
	if strings.TrimSpace(string(stored)) != secret {
		t.Fatalf("unexpected stored secret: %q", stored)
	}
	created, err := os.ReadFile(createCountPath)
	if err != nil {
		t.Fatalf("read create count: %v", err)
	}
	if string(created) != "create\n" {
		t.Fatalf("expected one secret creation, got %q", created)
	}
	random, err := os.ReadFile(randomCountPath)
	if err != nil {
		t.Fatalf("read random count: %v", err)
	}
	if string(random) != "random-intent\n" {
		t.Fatalf("expected one creation-intent generation, got %q", random)
	}
	marker, err := os.ReadFile(provenancePath)
	if err != nil {
		t.Fatalf("read provenance marker: %v", err)
	}
	if !strings.HasPrefix(string(marker), secretID+" ") {
		t.Fatalf("provenance marker does not bind the Docker secret ID: %q", marker)
	}
	if strings.Contains(string(out), secret) {
		t.Fatalf("authentication secret leaked to output: %q", out)
	}
	if err := os.WriteFile(provenancePath, []byte("zyxwvutsrqponmlkjihgfedcb "+strings.Repeat("a", 64)+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	cmd = exec.Command(bash, "-c", script, "test", statePath, backupPath, provenancePath, createCountPath, randomCountPath, "0", "0")
	out, err = cmd.CombinedOutput()
	if err == nil || !strings.Contains(string(out), "does not match the trusted Bort provenance marker") {
		t.Fatalf("expected mismatched Docker secret identity refusal, got %v: %s", err, out)
	}
}

func TestDokployShadowInstallRecoveryValidatesRunningService(t *testing.T) {
	bash, err := exec.LookPath("bash")
	if err != nil {
		t.Skip("bash is unavailable")
	}
	start := strings.Index(dokployShadowInstallScript, "validate_bort_install_service() {")
	end := strings.Index(dokployShadowInstallScript[start:], "\n}\n\nDOKPLOY_AUTH_SECRET_ID=")
	if start < 0 || end < 0 {
		t.Fatal("missing Bort-created Dokploy service recovery validator")
	}
	validator := dokployShadowInstallScript[start : start+end+3]
	recoveryStart := strings.Index(dokployShadowInstallScript, `elif ! DOKPLOY_SERVICE_REPLICAS=`)
	recoveryEnd := strings.Index(dokployShadowInstallScript[recoveryStart:], `if [ "$DOKPLOY_STAGED_SERVICE" = true ]; then`)
	if recoveryStart < 0 || recoveryEnd < 0 {
		t.Fatal("missing existing Dokploy service recovery path")
	}
	recovery := dokployShadowInstallScript[recoveryStart : recoveryStart+recoveryEnd]
	if !strings.Contains(recovery, "validate_bort_install_service \"$DOKPLOY_INSTALL_SERVICE_STATE\"\n\tif [ \"$DOKPLOY_SERVICE_REPLICAS\" = 0 ]; then") {
		t.Fatal("running Dokploy service recovery does not validate the durable Bort install identity before the zero-replica-only task check")
	}

	const secretID = "abcdefghijklmnopqrstuvwxy"
	const requestedImage = "dokploy/dokploy:v0.30.7@sha256:62e4354a49ee686ea749d3541d93113d494501befe81f5226c7f859f201e3a7c"
	wrapper := `set -euo pipefail
INSTALL_AUTH_SECRET_LABEL=io.bort.install-auth-secret-id
REQUESTED_DOKPLOY_IMAGE=` + requestedImage + `
INSTALL_LABEL="$1"
CURRENT_SECRET_ID="$2"
SERVICE_IMAGE="$3"
DOKPLOY_AUTH_ENV="$4"
BOUND_SECRET_IDS="$5"
validate_auth_secret_provenance() { DOKPLOY_AUTH_SECRET_ID="$CURRENT_SECRET_ID"; }
docker() {
    [ "$1 $2 $3 $4" = "service inspect dokploy --format" ] || return 1
    case "$5" in
        *Annotations.Labels*) printf '%s\n' "$INSTALL_LABEL" ;;
        *ContainerSpec.Image*) printf '%s\n' "$SERVICE_IMAGE" ;;
        *ContainerSpec.Secrets*) printf '%s\n' "$BOUND_SECRET_IDS" ;;
        *) return 1 ;;
    esac
}
` + validator + `
validate_bort_install_service running
`
	run := func(label, currentID, image, authEnv, boundIDs string) ([]byte, error) {
		return exec.Command(bash, "-c", wrapper, "test", label, currentID, image, authEnv, boundIDs).CombinedOutput()
	}
	validEnv := "BETTER_AUTH_SECRET_FILE=/run/secrets/dokploy_auth_secret"
	if out, err := run(secretID, secretID, requestedImage, validEnv, secretID); err != nil {
		t.Fatalf("valid running service recovery was refused: %v: %s", err, out)
	}
	for name, tc := range map[string]struct {
		label, currentID, image, authEnv, boundIDs, want string
	}{
		"removed label":        {"", secretID, requestedImage, validEnv, secretID, "not labeled"},
		"changed label":        {"zyxwvutsrqponmlkjihgfedcb", secretID, requestedImage, validEnv, secretID, "not bound"},
		"changed named secret": {secretID, "zyxwvutsrqponmlkjihgfedcb", requestedImage, validEnv, secretID, "not bound"},
		"bare requested tag":   {secretID, secretID, "dokploy/dokploy:v0.30.7", validEnv, secretID, "immutable image"},
		"changed digest":       {secretID, secretID, "dokploy/dokploy:v0.30.7@sha256:" + strings.Repeat("b", 64), validEnv, secretID, "immutable image"},
		"changed mount path":   {secretID, secretID, requestedImage, "BETTER_AUTH_SECRET_FILE=/run/secrets/other", secretID, "exactly Bort's"},
		"duplicate mount env":  {secretID, secretID, requestedImage, validEnv + "\n" + validEnv, secretID, "exactly Bort's"},
		"changed binding":      {secretID, secretID, requestedImage, validEnv, "zyxwvutsrqponmlkjihgfedcb", "does not bind exactly"},
	} {
		t.Run(name, func(t *testing.T) {
			out, err := run(tc.label, tc.currentID, tc.image, tc.authEnv, tc.boundIDs)
			if err == nil || !strings.Contains(string(out), tc.want) {
				t.Fatalf("expected %q refusal, got %v: %s", tc.want, err, out)
			}
		})
	}
}

func TestDokployShadowForeignServiceGuardExitsWithRefusalStatus(t *testing.T) {
	bash, err := exec.LookPath("bash")
	if err != nil {
		t.Skip("bash is unavailable")
	}
	statusLine := fmt.Sprintf("\nFOREIGN_SERVICE_EXIT_STATUS=%d\n", dokployInstallForeignServiceExitStatus)
	if !strings.Contains(dokployShadowInstallScript, statusLine) {
		t.Fatalf("install script does not define FOREIGN_SERVICE_EXIT_STATUS=%d", dokployInstallForeignServiceExitStatus)
	}
	const guardStart = "    if [ -z \"$DOKPLOY_INSTALL_LABEL\" ]; then\n"
	const guardEnd = "\n    fi\n"
	start := strings.Index(dokployShadowInstallScript, guardStart)
	if start < 0 {
		t.Fatal("missing foreign-service guard")
	}
	if authInspect := strings.Index(dokployShadowInstallScript, "DOKPLOY_AUTH_ENV=\"$(docker service inspect"); authInspect < 0 || authInspect > start {
		t.Fatal("authentication-secret refusal must run before the foreign-service guard so a legacy-secret service is not routed to the unchecked non-install bootstrap")
	}
	end := strings.Index(dokployShadowInstallScript[start:], guardEnd)
	if end < 0 {
		t.Fatal("unterminated foreign-service guard")
	}
	guard := dokployShadowInstallScript[start : start+end+len(guardEnd)]
	binDir := t.TempDir()
	if err := os.WriteFile(filepath.Join(binDir, "docker"), []byte("#!/bin/sh\necho 3000\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command(bash, "-c", statusLine+"DOKPLOY_INSTALL_LABEL=\n"+guard+"echo guard fell through\nexit 0\n")
	cmd.Env = append(os.Environ(), "PATH="+binDir+string(os.PathListSeparator)+os.Getenv("PATH"))
	out, err := cmd.CombinedOutput()
	var exitErr *exec.ExitError
	if !errors.As(err, &exitErr) || exitErr.ExitCode() != dokployInstallForeignServiceExitStatus {
		t.Fatalf("expected exit status %d from the foreign-service guard, got %v: %s", dokployInstallForeignServiceExitStatus, err, out)
	}
	if !strings.Contains(string(out), "was not installed by Bort") || !strings.Contains(string(out), "http://127.0.0.1:3000") {
		t.Fatalf("unexpected guard output: %s", out)
	}
}

func TestDokployShadowEndpointMode(t *testing.T) {
	bash, err := exec.LookPath("bash")
	if err != nil {
		t.Skip("bash is unavailable")
	}
	start := strings.Index(dokployShadowInstallScript, "validate_endpoint_mode() {")
	end := strings.Index(dokployShadowInstallScript, "\nkernel_config=")
	if start < 0 || end < start {
		t.Fatal("missing endpoint validation function")
	}
	for _, tc := range []struct {
		mode, config, wantError string
	}{
		{"vip", "CONFIG_IP_VS=y", ""},
		{"vip", "CONFIG_IP_VS=m", ""},
		{"vip", "", ""},
		{"vip", "# CONFIG_IP_VS is not set", "requires kernel IPVS"},
		{"vip", "CONFIG_IP_VS=n", "requires kernel IPVS"},
		{"dnsrr", "# CONFIG_IP_VS is not set", ""},
		{"dnsrr", "CONFIG_IP_VS=y", ""},
		{"invalid", "CONFIG_IP_VS=y", "must be vip or dnsrr"},
	} {
		t.Run(tc.mode+"/"+tc.config, func(t *testing.T) {
			cmd := exec.Command(bash, "-c", dokployShadowInstallScript[start:end]+"\nvalidate_endpoint_mode \"$1\" \"$2\"", "test", tc.mode, tc.config)
			out, err := cmd.CombinedOutput()
			if tc.wantError == "" {
				if err != nil {
					t.Fatalf("validation failed: %v: %s", err, out)
				}
			} else if err == nil || !strings.Contains(string(out), tc.wantError) {
				t.Fatalf("wanted %q, got %v: %s", tc.wantError, err, out)
			}
		})
	}
	for _, service := range []string{"dokploy-postgres", "dokploy-redis", "dokploy"} {
		start := strings.Index(dokployShadowInstallScript, "--name "+service+" \\")
		if start < 0 {
			t.Fatalf("%s service create block not found", service)
		}
		block := dokployShadowInstallScript[start:]
		if end := strings.Index(block, ">/dev/null"); end >= 0 {
			block = block[:end]
		}
		if !strings.Contains(block, `--endpoint-mode "$ENDPOINT_MODE"`) {
			t.Errorf("%s must use the selected endpoint mode", service)
		}
	}
	if !strings.Contains(dokployShadowInstallScript, `--publish published="$HOST_PORT",target=3000,mode=host`) {
		t.Fatal("DNSRR requires host-mode publication")
	}
}

func TestLegacyAuthSecretEscrowPreparation(t *testing.T) {
	bash, err := exec.LookPath("bash")
	if err != nil {
		t.Skip("bash is unavailable")
	}
	start := strings.Index(dokployShadowInstallScript, `: "${DOKPLOY_AUTH_SECRET_BACKUP:?`)
	if start < 0 {
		t.Fatal("missing legacy authentication-secret escrow block")
	}
	endNeedle := `sudo docker secret create dokploy-auth-secret - < "$auth_secret_tmp"`
	end := strings.Index(dokployShadowInstallScript[start:], endNeedle)
	if end < 0 {
		t.Fatal("missing legacy authentication-secret escrow block")
	}
	block := dokployShadowInstallScript[start : start+end+len(endNeedle)]
	dir := t.TempDir()
	backup := filepath.Join(dir, "off-host", "dokploy-auth-secret")
	twoFactorBefore := filepath.Join(dir, "off-host", "dokploy-two-factor-before.csv")
	if err := os.Mkdir(filepath.Dir(backup), 0o700); err != nil {
		t.Fatal(err)
	}
	run := func(capture, databaseEnv string) ([]byte, error) {
		wrapper := `set -euo pipefail
DOKPLOY_AUTH_SECRET_BACKUP="$1"
CAPTURE="$2"
TMPDIR="$3"
DOKPLOY_TWO_FACTOR_BEFORE="$4"
DATABASE_ENV="$5"
sync() { :; }
stat() {
    [ "$1 $2" = "-c %a" ] || return 1
    printf '600\n'
}
shred() {
    local last="${!#}"
	printf 'destroyed\n' > "$last"
    rm -f -- "$last"
}
sudo() {
    if [ "$1 $2 $3" = "docker service inspect" ]; then
		printf '%s\n' "$DATABASE_ENV"
        return
    fi
	if [ "$1 $2 $3" = "docker service update" ]; then
		return
	fi
    if [ "$1 $2" = "docker ps" ]; then
        printf 'postgres-container\n'
        return
    fi
    if [ "$1 $2 $3" = "docker exec postgres-container" ]; then
		if [[ "$*" == *pg_stat_activity* ]]; then
			printf '0\n'
			return
		fi
		printf 'id,secret,backup_codes\n'
		return
	fi
	if [ "$1 $2 $3 $4" = "docker secret create dokploy-auth-secret" ]; then
		cat > "$CAPTURE"
		return
	fi
	echo "unexpected sudo command: $*" >&2
	return 1
}
` + block
		cmd := exec.Command(bash, "-c", wrapper, "test", backup, capture, dir, twoFactorBefore, databaseEnv)
		return cmd.CombinedOutput()
	}

	for name, databaseEnv := range map[string]string{
		"database URL":   "DATABASE_URL=postgresql://external.example/dokploy",
		"host":           "POSTGRES_HOST=external-db",
		"port":           "POSTGRES_PORT=6543",
		"user":           "POSTGRES_USER=operator",
		"database":       "POSTGRES_DB=other",
		"duplicate host": "POSTGRES_HOST=dokploy-postgres\nPOSTGRES_HOST=external-db",
	} {
		t.Run(name, func(t *testing.T) {
			externalCapture := filepath.Join(dir, "external-"+strings.ReplaceAll(name, " ", "-"))
			if out, err := run(externalCapture, databaseEnv); err == nil || !strings.Contains(string(out), "refusing authentication-secret migration") && !strings.Contains(string(out), "database-operator procedure") {
				t.Fatalf("expected custom database refusal, got %v: %s", err, out)
			}
			if _, err := os.Stat(backup); !os.IsNotExist(err) {
				t.Fatalf("custom database refusal mutated the escrow: %v", err)
			}
		})
	}

	firstCapture := filepath.Join(dir, "first")
	defaultDatabaseEnv := "POSTGRES_HOST=dokploy-postgres\nPOSTGRES_PORT=5432\nPOSTGRES_USER=dokploy\nPOSTGRES_DB=dokploy"
	if out, err := run(firstCapture, defaultDatabaseEnv); err != nil {
		t.Fatalf("initial escrow preparation failed: %v: %s", err, out)
	}
	escrowed, err := os.ReadFile(backup)
	if err != nil {
		t.Fatal(err)
	}
	created, err := os.ReadFile(firstCapture)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(escrowed, created) || len(escrowed) != 65 || escrowed[64] != '\n' {
		t.Fatalf("Docker secret input differs from escrow: escrow=%q input=%q", escrowed, created)
	}
	info, err := os.Stat(backup)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Fatalf("escrow mode=%#o, want 0600", info.Mode().Perm())
	}
	twoFactorSnapshot, err := os.ReadFile(twoFactorBefore)
	if err != nil {
		t.Fatalf("read retained two-factor snapshot: %v", err)
	}
	if string(twoFactorSnapshot) != "id,secret,backup_codes\n" {
		t.Fatalf("retained two-factor snapshot was changed through its temporary hard link: %q", twoFactorSnapshot)
	}

	secondCapture := filepath.Join(dir, "second")
	if out, err := run(secondCapture, defaultDatabaseEnv); err != nil {
		t.Fatalf("retained escrow reuse failed: %v: %s", err, out)
	}
	reused, err := os.ReadFile(secondCapture)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(escrowed, reused) {
		t.Fatal("retry did not reuse the retained escrow")
	}
	temporaryFiles, err := filepath.Glob(filepath.Join(dir, "dokploy-auth-secret.*"))
	if err != nil {
		t.Fatal(err)
	}
	if len(temporaryFiles) != 0 {
		t.Fatalf("temporary secret files were retained: %v", temporaryFiles)
	}
}

func TestDokployShadowEndpointPreflight(t *testing.T) {
	bash, err := exec.LookPath("bash")
	if err != nil {
		t.Skip("bash is unavailable")
	}
	configStart := strings.Index(dokployShadowInstallScript, "\nkernel_config=")
	configEnd := strings.Index(dokployShadowInstallScript, "\nvalidate_endpoint_mode \"")
	end := strings.Index(dokployShadowInstallScript, "\nprivate_ip() {")
	if configStart < 0 || configEnd < configStart || end < configEnd {
		t.Fatal("missing endpoint preflight boundaries")
	}
	preflight := dokployShadowInstallScript[:configStart] + "\nkernel_config=\"$2\"" + dokployShadowInstallScript[configEnd:end]
	authBackup := filepath.Join(t.TempDir(), "dokploy-auth-secret")
	stub := `
ENDPOINT_MODE="$1"
ACME_EMAIL=admin@dokploy.local
DOKPLOY_VERSION=v0.30.7@sha256:62e4354a49ee686ea749d3541d93113d494501befe81f5226c7f859f201e3a7c
printf '%064d\n' 0 > "$DOKPLOY_AUTH_SECRET_BACKUP"
chmod 600 "$DOKPLOY_AUTH_SECRET_BACKUP"
exec 3< "$DOKPLOY_AUTH_SECRET_BACKUP"
DOKPLOY_AUTH_SECRET_FD=3
DOKPLOY_AUTH_SECRET_DIGEST="$(printf '%064d' 0)"
id() { echo 0; }
python3() { return 0; }
sync() { :; }
stat() {
    case "$2" in
        %a) printf '600\n' ;;
        %u) printf '0\n' ;;
        *) return 1 ;;
    esac
}
sha256sum() { printf '%064d  %s\n' 0 "$2"; }
docker() {
    if [ "$1 $2" = "info --format" ]; then
        printf '%s\n' "$SWARM_STATUS"
        return
    fi
    if [ "$1 $2" = "node ls" ]; then
        for ((i=0; i<MANAGER_COUNT; i++)); do
            echo "manager-$i"
        done
        return
    fi
    if [ "$1 $2" = "service inspect" ]; then
        case "$3" in
            dokploy-postgres) mode="$POSTGRES_MODE" ;;
            dokploy-redis) mode="$REDIS_MODE" ;;
            dokploy) mode="$DOKPLOY_MODE" ;;
            *) return 1 ;;
        esac
        if [ "$mode" = absent ]; then
            echo 'no such service' >&2
            return 1
        fi
        if [ "$mode" = error ]; then
            echo 'docker daemon unavailable' >&2
            return 1
        fi
        if [ "$mode" = not-manager ]; then
            echo 'This node is not a swarm manager.' >&2
            return 1
        fi
        if [[ "${5:-}" == *ContainerSpec.Env* ]]; then
            printf '%s\n' "$DOKPLOY_AUTH"
            return
        fi
        if [[ "${5:-}" == *ContainerSpec.Image* ]]; then
            printf '%s\n' "$DOKPLOY_IMAGE"
            return
        fi
        printf '%s\n' "$mode"
        return
    fi
    if [ "$1 $2 $3" = "secret inspect dokploy_auth_secret" ]; then
        if [ "$AUTH_SECRET_MODE" = present ]; then
			if [ "${4:-}" = "--format" ]; then
				printf 'abcdefghijklmnopqrstuvwxy\n'
			fi
            return
        fi
        if [ "$AUTH_SECRET_MODE" = error ]; then
            echo 'docker daemon unavailable' >&2
            return 1
        fi
        if [ "$AUTH_SECRET_MODE" = not-manager ]; then
            echo 'This node is not a swarm manager.' >&2
            return 1
        fi
        echo 'no such secret' >&2
        return 1
    fi
    if [ "$1 $2 $3" = "volume inspect dokploy-postgres" ]; then
        if [ "$POSTGRES_VOLUME_MODE" = present ]; then
            return
        fi
        if [ "$POSTGRES_VOLUME_MODE" = error ]; then
            echo 'docker daemon unavailable' >&2
            return 1
        fi
        echo 'no such volume' >&2
        return 1
    fi
    echo unexpected-docker-command
    return 1
}
POSTGRES_MODE="$3"
REDIS_MODE="$4"
DOKPLOY_MODE="$5"
DOKPLOY_AUTH="$6"
DOKPLOY_IMAGE="$7"
AUTH_SECRET_MODE="$8"
POSTGRES_VOLUME_MODE="$9"
SWARM_STATUS="${10}"
MANAGER_COUNT="${11:-1}"
`
	for _, tc := range []struct {
		name, mode, config, postgres, redis, dokploy, auth, wantError string
	}{
		{"matching file secret", "dnsrr", "", "dnsrr", "dnsrr", "dnsrr", "BETTER_AUTH_SECRET_FILE=/run/secrets/dokploy_auth_secret", ""},
		{"matching environment secret", "dnsrr", "", "dnsrr", "dnsrr", "dnsrr", "BETTER_AUTH_SECRET=0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef", ""},
		{"duplicate environment secret", "dnsrr", "", "dnsrr", "dnsrr", "dnsrr", "BETTER_AUTH_SECRET=0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef\nBETTER_AUTH_SECRET=0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef", "duplicate BETTER_AUTH_SECRET entries"},
		{"custom and legacy environment secrets", "dnsrr", "", "dnsrr", "dnsrr", "dnsrr", "BETTER_AUTH_SECRET=0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef\nBETTER_AUTH_SECRET=better-auth-secret-123456789", "duplicate BETTER_AUTH_SECRET entries"},
		{"published legacy environment secret", "dnsrr", "", "dnsrr", "dnsrr", "dnsrr", "BETTER_AUTH_SECRET=better-auth-secret-123456789", "published legacy authentication secret"},
		{"short environment secret", "dnsrr", "", "dnsrr", "dnsrr", "dnsrr", "BETTER_AUTH_SECRET=too-short", "shorter than Better Auth's 32-character minimum"},
		{"absent", "dnsrr", "", "absent", "absent", "absent", "", ""},
		{"inactive fresh host", "dnsrr", "", "not-manager", "not-manager", "not-manager", "", ""},
		{"missing auth secret", "dnsrr", "", "dnsrr", "dnsrr", "dnsrr", "", "existing Dokploy service has no authentication secret"},
		{"first conflict", "dnsrr", "", "vip", "dnsrr", "dnsrr", "BETTER_AUTH_SECRET=configured", "existing service dokploy-postgres uses endpoint mode vip"},
		{"later conflict", "dnsrr", "", "dnsrr", "vip", "dnsrr", "BETTER_AUTH_SECRET=configured", "existing service dokploy-redis uses endpoint mode vip"},
		{"last conflict", "dnsrr", "", "dnsrr", "dnsrr", "vip", "BETTER_AUTH_SECRET=configured", "existing service dokploy uses endpoint mode vip"},
		{"disabled VIP", "vip", "# CONFIG_IP_VS is not set", "absent", "absent", "absent", "", "requires kernel IPVS"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			image := "dokploy/dokploy:v0.30.7"
			authSecret := "present"
			if tc.dokploy == "absent" {
				image = ""
			}
			if tc.name == "absent" {
				authSecret = "absent"
			}
			swarmStatus := "active true"
			if tc.postgres == "absent" && tc.redis == "absent" && tc.dokploy == "absent" {
				swarmStatus = "inactive false"
			}
			if tc.name == "inactive fresh host" {
				authSecret = "not-manager"
				swarmStatus = "inactive false"
			}
			cmd := exec.Command(bash, "-c", stub+preflight+"\necho preflight-complete", "test", tc.mode, tc.config, tc.postgres, tc.redis, tc.dokploy, tc.auth, image, authSecret, "absent", swarmStatus)
			cmd.Env = append(os.Environ(), "DOKPLOY_AUTH_SECRET_BACKUP="+authBackup)
			out, err := cmd.CombinedOutput()
			if tc.wantError == "" {
				if err != nil || !strings.Contains(string(out), "preflight-complete") {
					t.Fatalf("preflight did not continue: %v: %s", err, out)
				}
			} else if err == nil || !strings.Contains(string(out), tc.wantError) || strings.Contains(string(out), "preflight-complete") {
				t.Fatalf("wanted refusal %q before continuation, got %v: %s", tc.wantError, err, out)
			}
		})
	}
	for _, tc := range []struct {
		name, postgres, dokploy, auth, image, authSecret, postgresVolume, wantError string
	}{
		{"minimum file-secret image", "dnsrr", "dnsrr", "BETTER_AUTH_SECRET_FILE=/run/secrets/dokploy_auth_secret", "docker.io/dokploy/dokploy:v0.29.3", "present", "present", ""},
		{"digest-qualified file-secret image", "dnsrr", "dnsrr", "BETTER_AUTH_SECRET_FILE=/run/secrets/dokploy_auth_secret", "dokploy/dokploy:v0.29.3@sha256:0123456789abcdef", "present", "present", ""},
		{"floating latest file-secret image", "dnsrr", "dnsrr", "BETTER_AUTH_SECRET_FILE=/run/secrets/dokploy_auth_secret", "dokploy/dokploy:latest@sha256:0123456789abcdef", "present", "present", "cannot verify file-backed authentication-secret support"},
		{"legacy file-secret image", "dnsrr", "dnsrr", "BETTER_AUTH_SECRET_FILE=/run/secrets/dokploy_auth_secret", "dokploy/dokploy:v0.29.2", "present", "present", "cannot verify file-backed authentication-secret support"},
		{"prerelease minimum file-secret image", "dnsrr", "dnsrr", "BETTER_AUTH_SECRET_FILE=/run/secrets/dokploy_auth_secret", "dokploy/dokploy:v0.29.3-rc.1", "present", "present", "cannot verify file-backed authentication-secret support"},
		{"unknown file-secret image", "dnsrr", "dnsrr", "BETTER_AUTH_SECRET_FILE=/run/secrets/dokploy_auth_secret", "dokploy/dokploy:feature", "present", "present", "cannot verify file-backed authentication-secret support"},
		{"partial database service without secret", "dnsrr", "absent", "", "", "absent", "absent", "existing Dokploy database state has no reusable dokploy_auth_secret"},
		{"partial database volume without secret", "absent", "absent", "", "", "absent", "present", "existing Dokploy database state has no reusable dokploy_auth_secret"},
		{"partial database with unprovenanced secret", "dnsrr", "absent", "", "", "present", "present", "no trusted Bort provenance marker"},
		{"database service inspection failure", "error", "absent", "", "", "absent", "absent", "could not determine whether Docker service dokploy-postgres exists"},
		{"database volume inspection failure", "absent", "absent", "", "", "absent", "error", "could not determine whether Docker volume dokploy-postgres exists"},
		{"Dokploy service inspection failure", "absent", "error", "", "", "absent", "absent", "could not determine whether Docker service dokploy exists"},
		{"auth-secret inspection failure", "absent", "absent", "", "", "error", "absent", "could not determine whether Docker secret dokploy_auth_secret exists"},
		{"active swarm worker", "absent", "absent", "", "", "absent", "absent", "active Docker swarm worker, not a manager"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			swarmStatus := "active true"
			if tc.postgres == "absent" && tc.dokploy == "absent" {
				swarmStatus = "inactive false"
			}
			if tc.name == "active swarm worker" {
				swarmStatus = "active false"
			}
			cmd := exec.Command(bash, "-c", stub+preflight+"\necho preflight-complete", "test", "dnsrr", "", tc.postgres, "absent", tc.dokploy, tc.auth, tc.image, tc.authSecret, tc.postgresVolume, swarmStatus)
			cmd.Env = append(os.Environ(), "DOKPLOY_AUTH_SECRET_BACKUP="+authBackup)
			out, err := cmd.CombinedOutput()
			if tc.wantError == "" {
				if err != nil || !strings.Contains(string(out), "preflight-complete") {
					t.Fatalf("preflight did not continue: %v: %s", err, out)
				}
			} else if err == nil || !strings.Contains(string(out), tc.wantError) || strings.Contains(string(out), "preflight-complete") {
				t.Fatalf("wanted refusal %q before continuation, got %v: %s", tc.wantError, err, out)
			}
		})
	}
	t.Run("multi-manager fresh state is ambiguous", func(t *testing.T) {
		cmd := exec.Command(bash, "-c", stub+preflight+"\necho preflight-complete", "test", "dnsrr", "", "absent", "absent", "absent", "", "", "absent", "absent", "active true", "2")
		cmd.Env = append(os.Environ(), "DOKPLOY_AUTH_SECRET_BACKUP="+authBackup)
		out, err := cmd.CombinedOutput()
		want := "automatic fresh install is unsupported unless the original Bort-managed dokploy_auth_secret and /var/lib/bort/dokploy-auth-secret.id provenance marker are restored together"
		if err == nil || !strings.Contains(string(out), want) || strings.Contains(string(out), "preflight-complete") {
			t.Fatalf("wanted refusal %q before continuation, got %v: %s", want, err, out)
		}
	})
}

func TestValidateDokployBootstrapURLPolicy(t *testing.T) {
	t.Run("accepts http localhost case-insensitively", func(t *testing.T) {
		if err := validateDokployBootstrapURL("http://LOCALHOST:3030"); err != nil {
			t.Fatalf("expected LOCALHOST to be accepted: %v", err)
		}
	})

	t.Run("rejects remote https for same-VPS bootstrap", func(t *testing.T) {
		if err := validateDokployBootstrapURL("https://dokploy.example.com"); err == nil || !strings.Contains(err.Error(), "non-loopback") {
			t.Fatalf("expected remote bootstrap URL to be rejected: %v", err)
		}
	})

	t.Run("rejects loopback https", func(t *testing.T) {
		if err := validateDokployBootstrapURL("https://localhost:3030"); err == nil || !strings.Contains(err.Error(), "direct loopback HTTP endpoint") {
			t.Fatalf("expected loopback HTTPS to be rejected: %v", err)
		}
	})

	t.Run("rejects loopback proxy path", func(t *testing.T) {
		if err := validateDokployBootstrapURL("http://127.0.0.1:3030/dokploy"); err == nil || !strings.Contains(err.Error(), "root of the direct loopback HTTP endpoint") {
			t.Fatalf("expected loopback proxy path to be rejected: %v", err)
		}
	})

	t.Run("requires the published service port", func(t *testing.T) {
		if err := validateDokployBootstrapURL("http://127.0.0.1"); err == nil || !strings.Contains(err.Error(), "service port") {
			t.Fatalf("expected missing local service port to be rejected: %v", err)
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

func requireNoDokployInstallationRecovery(t *testing.T) {
	t.Helper()
	t.Cleanup(func() { _ = clearDokployInstallationRecoveryRequired() })
	if blocked, err := dokployInstallationRecoveryRequired(); err != nil || blocked {
		t.Fatalf("blocked init-target left installation recovery required: blocked=%t err=%v", blocked, err)
	}
}

type abortingAdminLister struct{}

func (*abortingAdminLister) listAdmins(context.Context) ([]coolifyAdmin, error) {
	return nil, errors.New("admin listing must not run")
}

func strandedDokployInstallRecovery(t *testing.T, targetURL string) dokployInstallationRecovery {
	t.Helper()
	t.Cleanup(func() { _ = clearDokployInstallationRecoveryRequired() })
	recovery := newDokployInstallationRecovery(targetURL, dokployInstallOptions{
		HostPort:         "3030",
		AddrPool:         "auto",
		Version:          defaultDokployVersion + "@" + defaultDokployDigest,
		EndpointMode:     "vip",
		ACMEEmail:        "admin@example.com",
		AuthSecretBackup: filepath.Join(t.TempDir(), "dokploy-auth-secret"),
		AdminName:        "Recovery Admin",
		APIKeyName:       "recovery key",
	})
	if err := markDokployInstallationRecoveryRequired(recovery); err != nil {
		t.Fatal(err)
	}
	return recovery
}

func requireDokployInstallationRecovery(t *testing.T) {
	t.Helper()
	blocked, err := dokployInstallationRecoveryRequired()
	if err != nil || !blocked {
		t.Fatalf("expected installation recovery state to remain: blocked=%t err=%v", blocked, err)
	}
}

func TestInitTargetInstallRefusesVIPModeWhenKernelConfigDisablesIPVS(t *testing.T) {
	for _, line := range []string{"# CONFIG_IP_VS is not set", "CONFIG_IP_VS=n"} {
		t.Run(line, func(t *testing.T) {
			t.Setenv(envDokployAuthBackup, filepath.Join(t.TempDir(), "dokploy-auth-secret"))
			installer := &fakeDokployInstaller{}
			deps := initTargetDeps{
				lister:    &abortingAdminLister{},
				installer: installer,
				newClient: func(string) *dokploy.Client {
					t.Fatal("Dokploy client was created after a refused VIP preflight")
					return nil
				},
				statePath:    filepath.Join(t.TempDir(), "state.json"),
				kernelConfig: func() (string, bool) { return line + "\nCONFIG_NET_IP_TUNNEL=y\n", true },
			}
			err := runInitTargetWith(context.Background(), []string{
				"--install",
				"--dokploy-url", "http://127.0.0.1:3030",
				"--endpoint-mode", "vip",
			}, strings.NewReader(""), io.Discard, io.Discard, deps)
			if err == nil || !strings.Contains(err.Error(), "Docker Swarm VIP mode requires kernel IPVS support; set ENDPOINT_MODE=dnsrr in the environment when rerunning the original command, or pass --endpoint-mode dnsrr to init-target") {
				t.Fatalf("expected VIP refusal for %q, got %v", line, err)
			}
			if installer.calls != 0 {
				t.Fatalf("refused VIP preflight reached the installer %d time(s)", installer.calls)
			}
			requireNoDokployInstallationRecovery(t)
		})
	}
}

func TestInitTargetInstallVIPPreflightPassesWithIPVSOrUnavailableConfig(t *testing.T) {
	for _, tc := range []struct {
		name         string
		mode         string
		kernelConfig func() (string, bool)
	}{
		{name: "ipvs enabled", mode: "vip", kernelConfig: func() (string, bool) {
			return "CONFIG_IP_VS=m\nCONFIG_IP_VS_PROTO_TCP=y\n", true
		}},
		{name: "kernel config unavailable", mode: "vip", kernelConfig: func() (string, bool) {
			return "", false
		}},
		{name: "dnsrr bypasses the kernel check", mode: "dnsrr", kernelConfig: func() (string, bool) {
			return "# CONFIG_IP_VS is not set\n", true
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv(envDokployAuthBackup, filepath.Join(t.TempDir(), "dokploy-auth-secret"))
			stub := &dokployStub{}
			server := newDokployStub(t, stub)
			defer server.Close()
			installer := &fakeDokployInstaller{}
			deps := initTargetDeps{
				lister: &fakeAdminLister{admins: []coolifyAdmin{
					{Email: "admin@example.com", Name: "Admin User", PasswordHash: bcryptCoolifyHash(t, "right-password")},
				}},
				installer:    installer,
				newClient:    func(string) *dokploy.Client { return defaultDokployClient(server.URL) },
				statePath:    filepath.Join(t.TempDir(), "state.json"),
				kernelConfig: tc.kernelConfig,
			}
			t.Setenv(envCoolifyAdminPwd, "right-password")
			err := runInitTargetWith(context.Background(), []string{
				"--install",
				"--dokploy-url", server.URL,
				"--install-port", serverPort(t, server),
				"--coolify-email", "admin@example.com",
				"--endpoint-mode", tc.mode,
			}, strings.NewReader(""), io.Discard, io.Discard, deps)
			if err != nil {
				t.Fatalf("init-target --install failed: %v", err)
			}
			if installer.calls != 1 || stub.createKeyCalls != 1 {
				t.Fatalf("expected one installer call and one api key, got installer=%d api_keys=%d", installer.calls, stub.createKeyCalls)
			}
			requireNoDokployInstallationRecovery(t)
		})
	}
}

func TestInitTargetAbandonRecoveryDryRunKeepsState(t *testing.T) {
	resetDokployTrafficOwner(t)
	recovery := strandedDokployInstallRecovery(t, "http://127.0.0.1:3030")
	deps := initTargetDeps{
		probeTargetLiveness: func(context.Context, string) (bool, error) { return false, nil },
	}
	stdout := &bytes.Buffer{}
	if err := runInitTargetWith(context.Background(), []string{"--abandon-recovery"}, strings.NewReader(""), stdout, io.Discard, deps); err != nil {
		t.Fatalf("abandon dry run failed: %v", err)
	}
	output := stdout.String()
	if !strings.Contains(output, "Dry run: found interrupted Dokploy installation recovery state") || !strings.Contains(output, recovery.Command) {
		t.Fatalf("abandon dry run omitted its findings: %q", output)
	}
	requireDokployInstallationRecovery(t)
}

func TestInitTargetAbandonRecoveryLiveClearsState(t *testing.T) {
	resetDokployTrafficOwner(t)
	recovery := strandedDokployInstallRecovery(t, "http://127.0.0.1:3030")
	deps := initTargetDeps{
		probeTargetLiveness: func(context.Context, string) (bool, error) { return false, nil },
	}
	stdout := &bytes.Buffer{}
	if err := runInitTargetWith(context.Background(), []string{"--abandon-recovery", "--live"}, strings.NewReader(""), stdout, io.Discard, deps); err != nil {
		t.Fatalf("abandon --live failed: %v", err)
	}
	output := stdout.String()
	if !strings.Contains(output, "Cleared the interrupted Dokploy installation recovery state") || !strings.Contains(output, recovery.Command) {
		t.Fatalf("abandon --live omitted its findings: %q", output)
	}
	requireNoDokployInstallationRecovery(t)
}

func TestInitTargetAbandonRecoveryLiveClearsStateAgainstClosedListener(t *testing.T) {
	resetDokployTrafficOwner(t)
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	port := listener.Addr().(*net.TCPAddr).Port
	if err := listener.Close(); err != nil {
		t.Fatal(err)
	}
	recovery := strandedDokployInstallRecovery(t, fmt.Sprintf("http://127.0.0.1:%d", port))
	stdout := &bytes.Buffer{}
	if err := runInitTargetWith(context.Background(), []string{"--abandon-recovery", "--live"}, strings.NewReader(""), stdout, io.Discard, initTargetDeps{}); err != nil {
		t.Fatalf("abandon --live against a closed listener failed: %v", err)
	}
	if !strings.Contains(stdout.String(), "Cleared the interrupted Dokploy installation recovery state") || !strings.Contains(stdout.String(), recovery.Command) {
		t.Fatalf("abandon --live omitted its findings: %q", stdout.String())
	}
	requireNoDokployInstallationRecovery(t)
}

func TestInitTargetAbandonRecoveryRefusesWhileDokployAnswers(t *testing.T) {
	resetDokployTrafficOwner(t)
	strandedDokployInstallRecovery(t, "http://127.0.0.1:3030")
	deps := initTargetDeps{
		probeTargetLiveness: func(context.Context, string) (bool, error) { return true, nil },
	}
	err := runInitTargetWith(context.Background(), []string{"--abandon-recovery", "--live"}, strings.NewReader(""), io.Discard, io.Discard, deps)
	if err == nil || !strings.Contains(err.Error(), "reconcile it by rerunning") {
		t.Fatalf("expected refusal while Dokploy answers, got %v", err)
	}
	requireDokployInstallationRecovery(t)
}

func TestInitTargetAbandonRecoveryRefusesCompletedInstall(t *testing.T) {
	resetDokployTrafficOwner(t)
	recovery := strandedDokployInstallRecovery(t, "http://127.0.0.1:3030")
	recovery.Phase = dokployInstallAPIKey
	recovery.APIKeyBaselineIDs = []string{"key-1"}
	recovery = canonicalDokployInstallationRecovery(recovery)
	if err := markDokployInstallationRecoveryRequired(recovery); err != nil {
		t.Fatal(err)
	}
	deps := initTargetDeps{
		probeTargetLiveness: func(context.Context, string) (bool, error) {
			t.Fatal("Dokploy liveness probe ran for a completed install")
			return false, nil
		},
	}
	err := runInitTargetWith(context.Background(), []string{"--abandon-recovery", "--live"}, strings.NewReader(""), io.Discard, io.Discard, deps)
	if err == nil || !strings.Contains(err.Error(), `reached phase "api-key-pending"`) || !strings.Contains(err.Error(), recovery.Command) {
		t.Fatalf("expected completed-install refusal, got %v", err)
	}
	requireDokployInstallationRecovery(t)
}

func TestInitTargetAbandonRecoveryWithoutStateReportsNone(t *testing.T) {
	resetDokployTrafficOwner(t)
	requireNoDokployInstallationRecovery(t)
	stdout := &bytes.Buffer{}
	if err := runInitTargetWith(context.Background(), []string{"--abandon-recovery"}, strings.NewReader(""), stdout, io.Discard, initTargetDeps{}); err != nil {
		t.Fatalf("abandon without state failed: %v", err)
	}
	if !strings.Contains(stdout.String(), "No interrupted Dokploy installation recovery state found.") {
		t.Fatalf("abandon without state printed unexpected output: %q", stdout.String())
	}
}

func TestInitTargetAbandonRecoveryRefusesUnreadableState(t *testing.T) {
	resetDokployTrafficOwner(t)
	path, err := dokployInstallationRecoveryPath()
	if err != nil {
		t.Fatal(err)
	}
	if err := prepareDokployLiveOperationLockPath(path); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Remove(path) })
	if err := os.WriteFile(path, []byte("not-json\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	err = runInitTargetWith(context.Background(), []string{"--abandon-recovery", "--live"}, strings.NewReader(""), io.Discard, io.Discard, initTargetDeps{})
	if err == nil || !strings.Contains(err.Error(), "is malformed") {
		t.Fatalf("expected unreadable recovery state refusal, got %v", err)
	}
}

func TestInitTargetAbandonRecoveryRefusesWhileHostOwned(t *testing.T) {
	resetDokployTrafficOwner(t)
	strandedDokployInstallRecovery(t, "http://127.0.0.1:3030")
	ownerRun := migrationRun{Name: "owner-run", RunDir: filepath.Join(t.TempDir(), "owner-run"), CreatedAt: time.Now().UTC(), BundleDigest: "owner-digest"}
	if err := claimDokployHostOwnership(ownerRun, "http://127.0.0.1:3030", dokployCredentialID("test-token")); err != nil {
		t.Fatal(err)
	}
	deps := initTargetDeps{
		probeTargetLiveness: func(context.Context, string) (bool, error) {
			t.Fatal("Dokploy liveness probe ran while the host was owned")
			return false, nil
		},
	}
	err := runInitTargetWith(context.Background(), []string{"--abandon-recovery", "--live"}, strings.NewReader(""), io.Discard, io.Discard, deps)
	if err == nil || !strings.Contains(err.Error(), "refusing standalone init-target") {
		t.Fatalf("expected host-ownership refusal, got %v", err)
	}
	requireDokployInstallationRecovery(t)
}

func TestInitTargetAbandonRecoveryRefusesWhenProbeIsInconclusive(t *testing.T) {
	resetDokployTrafficOwner(t)
	strandedDokployInstallRecovery(t, "http://127.0.0.1:3030")
	deps := initTargetDeps{
		probeTargetLiveness: func(context.Context, string) (bool, error) {
			return false, errors.New("probe interrupted")
		},
	}
	err := runInitTargetWith(context.Background(), []string{"--abandon-recovery", "--live"}, strings.NewReader(""), io.Discard, io.Discard, deps)
	if err == nil || !strings.Contains(err.Error(), "cannot tell whether the recorded Dokploy installation is live") || !strings.Contains(err.Error(), "probe interrupted") {
		t.Fatalf("expected inconclusive-probe refusal, got %v", err)
	}
	requireDokployInstallationRecovery(t)
}

func TestInitTargetAbandonRecoveryRefusesWhenCanceledContext(t *testing.T) {
	resetDokployTrafficOwner(t)
	strandedDokployInstallRecovery(t, "http://127.0.0.1:3030")
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	err := runInitTargetWith(ctx, []string{"--abandon-recovery", "--live"}, strings.NewReader(""), io.Discard, io.Discard, initTargetDeps{})
	if err == nil || !strings.Contains(err.Error(), "cannot tell whether the recorded Dokploy installation is live") {
		t.Fatalf("expected canceled-context refusal, got %v", err)
	}
	requireDokployInstallationRecovery(t)
}

func TestInitTargetAbandonRecoveryTreatsRedirectResponseAsLive(t *testing.T) {
	resetDokployTrafficOwner(t)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "http://127.0.0.1:1/unreachable", http.StatusFound)
	}))
	defer server.Close()
	strandedDokployInstallRecovery(t, server.URL)
	err := runInitTargetWith(context.Background(), []string{"--abandon-recovery", "--live"}, strings.NewReader(""), io.Discard, io.Discard, initTargetDeps{})
	if err == nil || !strings.Contains(err.Error(), "reconcile it by rerunning") {
		t.Fatalf("expected live-endpoint refusal behind a redirect, got %v", err)
	}
	requireDokployInstallationRecovery(t)
}

func TestInitTargetAbandonRecoveryRefusesWhenConnectionDropsWithoutResponse(t *testing.T) {
	resetDokployTrafficOwner(t)
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	go func() {
		for {
			conn, err := listener.Accept()
			if err != nil {
				return
			}
			conn.Close()
		}
	}()
	strandedDokployInstallRecovery(t, "http://"+listener.Addr().String())
	err = runInitTargetWith(context.Background(), []string{"--abandon-recovery", "--live"}, strings.NewReader(""), io.Discard, io.Discard, initTargetDeps{})
	if err == nil || !strings.Contains(err.Error(), "cannot tell whether the recorded Dokploy installation is live") {
		t.Fatalf("expected dropped-connection refusal, got %v", err)
	}
	requireDokployInstallationRecovery(t)
}

func TestDokployTargetProbeUnreachableClassifiesOnlyConnectionRefusal(t *testing.T) {
	refused := &net.OpError{Op: "dial", Err: os.NewSyscallError("connect", syscall.ECONNREFUSED)}
	if !dokployTargetProbeUnreachable(refused) {
		t.Fatal("expected a connection refusal to be classified as unreachable")
	}
	if dokployTargetProbeUnreachable(&net.OpError{Op: "dial", Err: os.NewSyscallError("connect", syscall.EACCES)}) {
		t.Fatal("expected a permission-denied dial failure to stay inconclusive")
	}
	if dokployTargetProbeUnreachable(&net.OpError{Op: "dial", Err: errors.New("i/o timeout")}) {
		t.Fatal("expected a dial timeout to stay inconclusive")
	}
}

func TestInitTargetAbandonRecoveryFlagConflicts(t *testing.T) {
	deps := initTargetDeps{lister: &abortingAdminLister{}}
	err := runInitTargetWith(context.Background(), []string{"--abandon-recovery", "--install"}, strings.NewReader(""), io.Discard, io.Discard, deps)
	if err == nil || !strings.Contains(err.Error(), "cannot run together with --install") {
		t.Fatalf("expected --abandon-recovery --install refusal, got %v", err)
	}
	err = runInitTargetWith(context.Background(), []string{"--live"}, strings.NewReader(""), io.Discard, io.Discard, deps)
	if err == nil || !strings.Contains(err.Error(), "--live is only valid with --abandon-recovery") {
		t.Fatalf("expected --live alone refusal, got %v", err)
	}
	requireNoDokployInstallationRecovery(t)
}
