package cli

import (
	"bufio"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"flag"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/mail"
	"net/netip"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/aikins01/bort/internal/dockercli"
	"github.com/aikins01/bort/internal/target/dokploy"
	"golang.org/x/crypto/bcrypt"
	"golang.org/x/term"
)

const (
	defaultCoolifyEnvPath            = "/data/coolify/source/.env"
	defaultCoolifyDBContainer        = "coolify-db"
	defaultDokployAPIName            = "bort-cli"
	defaultDokployVersion            = "v0.30.7"
	defaultDokployDigest             = "sha256:62e4354a49ee686ea749d3541d93113d494501befe81f5226c7f859f201e3a7c"
	envCoolifyAdminPwd               = "BORT_COOLIFY_ADMIN_PASSWORD"
	envDokployAuthBackup             = "BORT_DOKPLOY_AUTH_SECRET_BACKUP"
	envDokployEndpointMode           = "ENDPOINT_MODE"
	installProgressPrefix            = "BORT_INSTALL_STEP\t"
	psqlAdminFieldSeparator          = "\t"
	dokployInstallTimeout            = 20 * time.Minute
	dokployInstallRecoveryAPIVersion = "bort.dokploy-install-recovery/v1alpha1"
	dokployInstallInstalling         = "installing"
	dokployInstallAPIKey             = "api-key-pending"
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
	lister                    coolifyAdminLister
	installer                 dokployInstaller
	newClient                 func(baseURL string) *dokploy.Client
	verifyLocalService        func(context.Context, *dokploy.Client) error
	statePath                 string
	liveOperationLock         *applyLock
	retainLiveOperationLock   **applyLock
	revalidateAfterTargetLock func() error
	installTimeout            time.Duration
}

type dokployInstallOptions struct {
	HostPort         string
	AddrPool         string
	Version          string
	EndpointMode     string
	ACMEEmail        string
	AuthSecretBackup string
	AdminName        string
	APIKeyName       string
	operationLock    *os.File
}

type dokployInstallationRecovery struct {
	APIVersion        string   `json:"apiVersion"`
	Identity          string   `json:"identity"`
	Phase             string   `json:"phase"`
	CommandPrefix     string   `json:"commandPrefix"`
	TargetURL         string   `json:"targetUrl"`
	HostPort          string   `json:"hostPort"`
	AddressPool       string   `json:"addressPool"`
	DokployVersion    string   `json:"dokployVersion"`
	EndpointMode      string   `json:"endpointMode"`
	ACMEEmail         string   `json:"acmeEmail"`
	AuthSecretBackup  string   `json:"authSecretBackup"`
	AdminName         string   `json:"adminName"`
	APIKeyName        string   `json:"apiKeyName"`
	APIKeyBaselineIDs []string `json:"apiKeyBaselineIds"`
	APIKeyCreatedID   string   `json:"apiKeyCreatedId,omitempty"`
	Command           string   `json:"command"`
}

func newDokployInstallationRecovery(baseURL string, opts dokployInstallOptions) dokployInstallationRecovery {
	recovery := canonicalDokployInstallationRecovery(dokployInstallationRecovery{
		APIVersion:       dokployInstallRecoveryAPIVersion,
		Phase:            dokployInstallInstalling,
		CommandPrefix:    bortCommand(""),
		TargetURL:        baseURL,
		HostPort:         opts.HostPort,
		AddressPool:      opts.AddrPool,
		DokployVersion:   opts.Version,
		EndpointMode:     opts.EndpointMode,
		ACMEEmail:        opts.ACMEEmail,
		AuthSecretBackup: opts.AuthSecretBackup,
		AdminName:        opts.AdminName,
		APIKeyName:       opts.APIKeyName,
	})
	recovery.Identity = dokployInstallRecoveryIdentity(recovery)
	recovery.Command = dokployInstallRecoveryCommand(recovery)
	return recovery
}

func canonicalDokployInstallationRecovery(recovery dokployInstallationRecovery) dokployInstallationRecovery {
	recovery.Phase = strings.TrimSpace(recovery.Phase)
	recovery.CommandPrefix = strings.TrimSpace(recovery.CommandPrefix)
	recovery.TargetURL = strings.TrimSpace(recovery.TargetURL)
	recovery.HostPort = strings.TrimSpace(recovery.HostPort)
	recovery.AddressPool = strings.TrimSpace(recovery.AddressPool)
	if normalized, err := normalizeSwarmAddressPool(recovery.AddressPool); err == nil {
		recovery.AddressPool = normalized
	}
	recovery.DokployVersion = strings.TrimSpace(recovery.DokployVersion)
	recovery.EndpointMode = strings.TrimSpace(recovery.EndpointMode)
	recovery.ACMEEmail = strings.TrimSpace(recovery.ACMEEmail)
	recovery.AuthSecretBackup = strings.TrimSpace(recovery.AuthSecretBackup)
	recovery.APIKeyName = strings.TrimSpace(recovery.APIKeyName)
	recovery.APIKeyCreatedID = strings.TrimSpace(recovery.APIKeyCreatedID)
	recovery.APIKeyBaselineIDs = append([]string(nil), recovery.APIKeyBaselineIDs...)
	for index := range recovery.APIKeyBaselineIDs {
		recovery.APIKeyBaselineIDs[index] = strings.TrimSpace(recovery.APIKeyBaselineIDs[index])
	}
	slices.Sort(recovery.APIKeyBaselineIDs)
	return recovery
}

func sameDokployInstallationRecovery(a, b dokployInstallationRecovery) bool {
	return a.APIVersion == b.APIVersion &&
		a.Identity == b.Identity &&
		a.Phase == b.Phase &&
		a.CommandPrefix == b.CommandPrefix &&
		a.TargetURL == b.TargetURL &&
		a.HostPort == b.HostPort &&
		a.AddressPool == b.AddressPool &&
		a.DokployVersion == b.DokployVersion &&
		a.EndpointMode == b.EndpointMode &&
		a.ACMEEmail == b.ACMEEmail &&
		a.AuthSecretBackup == b.AuthSecretBackup &&
		a.AdminName == b.AdminName &&
		a.APIKeyName == b.APIKeyName &&
		slices.Equal(a.APIKeyBaselineIDs, b.APIKeyBaselineIDs) &&
		a.APIKeyCreatedID == b.APIKeyCreatedID &&
		a.Command == b.Command
}

func dokployInstallRecoveryCommand(recovery dokployInstallationRecovery) string {
	return strings.Join([]string{
		recovery.CommandPrefix + " init-target --install",
		"--dokploy-url " + shellQuote(recovery.TargetURL),
		"--install-port " + shellQuote(recovery.HostPort),
		"--swarm-addr-pool " + shellQuote(recovery.AddressPool),
		"--dokploy-version " + shellQuote(recovery.DokployVersion),
		"--endpoint-mode " + shellQuote(recovery.EndpointMode),
		"--coolify-email " + shellQuote(recovery.ACMEEmail),
		"--auth-secret-backup " + shellQuote(recovery.AuthSecretBackup),
		"--name " + shellQuote(recovery.AdminName),
		"--api-key-name " + shellQuote(recovery.APIKeyName),
	}, " ")
}

func dokployInstallRecoveryIdentity(recovery dokployInstallationRecovery) string {
	canonical := strings.Join([]string{
		recovery.CommandPrefix,
		recovery.TargetURL,
		recovery.HostPort,
		recovery.AddressPool,
		recovery.DokployVersion,
		recovery.EndpointMode,
		recovery.ACMEEmail,
		recovery.AuthSecretBackup,
		recovery.AdminName,
		recovery.APIKeyName,
	}, "\x00")
	digest := sha256.Sum256([]byte(canonical))
	return hex.EncodeToString(digest[:])
}

type dokployInstaller interface {
	InstallDokploy(ctx context.Context, opts dokployInstallOptions, stdout, stderr io.Writer) error
}

// the install script exits with this status only from its read-only
// foreign-service guard, before any host mutation, so the recovery
// marker can be cleared without inspecting the host.
const dokployInstallForeignServiceExitStatus = 75

func installerRefusedForeignService(err error) bool {
	var exit interface{ ExitCode() int }
	return errors.As(err, &exit) && exit.ExitCode() == dokployInstallForeignServiceExitStatus
}

type shellDokployInstaller struct{}

func runInitTarget(ctx context.Context, args []string, stdin io.Reader, stdout, stderr io.Writer) error {
	return runInitTargetWith(ctx, args, stdin, stdout, stderr, defaultInitTargetDeps(stderr))
}

func defaultInitTargetDeps(stderr io.Writer) initTargetDeps {
	return initTargetDeps{
		lister:    &coolifyDBLister{envPath: defaultCoolifyEnvPath, container: defaultCoolifyDBContainer, stderr: stderr},
		installer: shellDokployInstaller{},
		newClient: defaultDokployClient,
		verifyLocalService: func(ctx context.Context, client *dokploy.Client) error {
			return client.VerifySameDockerHost(ctx)
		},
		statePath: defaultStatePath(),
	}
}

func defaultDokployClient(baseURL string) *dokploy.Client {
	return &dokploy.Client{
		BaseURL: strings.TrimRight(baseURL, "/"),
		HTTPClient: &http.Client{
			Timeout: 30 * time.Second,
			CheckRedirect: func(req *http.Request, via []*http.Request) error {
				if len(via) >= 10 {
					return errors.New("dokploy bootstrap stopped after 10 redirects")
				}
				if !sameURLOrigin(req.URL, via[0].URL) {
					return fmt.Errorf("refusing Dokploy bootstrap redirect to %s://%s: credentials stay on the original origin", req.URL.Scheme, req.URL.Host)
				}
				return nil
			},
		},
	}
}

func sameURLOrigin(a, b *url.URL) bool {
	return strings.EqualFold(a.Scheme, b.Scheme) && normalizedURLHost(a) == normalizedURLHost(b)
}

func normalizedURLHost(u *url.URL) string {
	host := strings.ToLower(u.Hostname())
	port := u.Port()
	if port == "" || (u.Scheme == "https" && port == "443") || (u.Scheme == "http" && port == "80") {
		return host
	}
	return net.JoinHostPort(host, port)
}

func runInitTargetWith(ctx context.Context, args []string, stdin io.Reader, stdout, stderr io.Writer, deps initTargetDeps) (err error) {
	fs := flag.NewFlagSet("init-target", flag.ContinueOnError)
	fs.SetOutput(stderr)

	var (
		target       string
		email        string
		dokployURL   string
		nameOverride string
		apiKeyName   string
		install      bool
		installPort  string
		addrPool     string
		version      string
		endpointMode string
		authBackup   string
	)
	fs.StringVar(&target, "target", "dokploy", "target platform to bootstrap (only dokploy is supported)")
	fs.StringVar(&email, "coolify-email", "", "coolify admin email to reuse (prompted if absent and multiple admins exist)")
	fs.StringVar(&dokployURL, "dokploy-url", "", "dokploy base url (defaults to BORT_DOKPLOY_URL)")
	fs.StringVar(&nameOverride, "name", "", "display name for the dokploy admin (defaults to coolify user name)")
	fs.StringVar(&apiKeyName, "api-key-name", defaultDokployAPIName, "label for the dokploy api key")
	fs.BoolVar(&install, "install", false, "install dokploy on this VPS before bootstrapping credentials")
	fs.StringVar(&installPort, "install-port", "3030", "host port for dokploy UI/API when --install is used")
	fs.StringVar(&addrPool, "swarm-addr-pool", "auto", "docker swarm default address pool for --install (auto selects an unused private /16)")
	fs.StringVar(&version, "dokploy-version", defaultDokployVersion, "dokploy release tag for --install; nondefault versions require @sha256:<digest>")
	endpointMode = strings.TrimSpace(os.Getenv(envDokployEndpointMode))
	if endpointMode == "" {
		endpointMode = "vip"
	}
	fs.StringVar(&endpointMode, "endpoint-mode", endpointMode, "Docker Swarm endpoint mode for Dokploy control-plane services: vip or dnsrr")
	fs.StringVar(&authBackup, "auth-secret-backup", strings.TrimSpace(os.Getenv(envDokployAuthBackup)), "absolute path on encrypted or off-host storage for the Dokploy authentication-secret escrow")

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
	apiKeyName = strings.TrimSpace(apiKeyName)
	if err := validateDokployAPIKeyName(apiKeyName); err != nil {
		return err
	}
	if install {
		if err := validateInstallPort(installPort); err != nil {
			return err
		}
		normalizedAddrPool, err := normalizeSwarmAddressPool(addrPool)
		if err != nil {
			return err
		}
		addrPool = normalizedAddrPool
		normalizedVersion, err := normalizeDokployVersion(version)
		if err != nil {
			return err
		}
		version = normalizedVersion
		endpointMode = strings.ToLower(strings.TrimSpace(endpointMode))
		if endpointMode != "vip" && endpointMode != "dnsrr" {
			return fmt.Errorf("invalid --endpoint-mode %q: must be vip or dnsrr", endpointMode)
		}
		if err := validateAuthSecretBackupPath(authBackup); err != nil {
			return err
		}
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
	if err := validateDokployBootstrapURL(dokployURL); err != nil {
		return err
	}
	if install {
		if err := validateInstallURLPort(dokployURL, installPort); err != nil {
			return err
		}
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
	if install {
		if err := validateACMEEmail(admin.Email); err != nil {
			return err
		}
	}

	password := strings.TrimSpace(os.Getenv(envCoolifyAdminPwd))
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
	acquiredTargetLock := false
	if deps.liveOperationLock == nil {
		var targetLock *applyLock
		if install {
			targetLock, err = acquireDokployInstallRecoveryLock()
		} else {
			targetLock, err = acquireDokployLiveOperationLock()
		}
		if err != nil {
			return fmt.Errorf("lock Dokploy live operations: %w", err)
		}
		deps.liveOperationLock = targetLock
		acquiredTargetLock = true
	}
	revalidate := deps.revalidateAfterTargetLock
	if revalidate == nil {
		revalidate = ensureStandaloneDokployInitAvailable
	}
	if err := revalidate(); err != nil {
		if acquiredTargetLock {
			deps.liveOperationLock.Release()
		}
		return err
	}
	if acquiredTargetLock {
		if deps.retainLiveOperationLock != nil {
			*deps.retainLiveOperationLock = deps.liveOperationLock
		} else {
			defer deps.liveOperationLock.Release()
		}
	}

	installationRecoveryPending := false
	var installationRecovery dokployInstallationRecovery
	defer func() {
		if err == nil || !installationRecoveryPending {
			return
		}
		blocked, stateErr := dokployInstallationRecoveryRequired()
		switch {
		case stateErr != nil:
			err = fmt.Errorf("%w; Dokploy installation recovery state is unreadable, so other host mutations may stay blocked: %v", err, stateErr)
		case blocked:
			err = fmt.Errorf("%w; the interrupted installation now blocks other host mutations: run `%s` to reconcile it", err, dokployInstallRecoveryCommand(installationRecovery))
		}
	}()
	if install {
		installOpts := dokployInstallOptions{
			HostPort:         installPort,
			AddrPool:         addrPool,
			Version:          version,
			EndpointMode:     endpointMode,
			ACMEEmail:        admin.Email,
			AuthSecretBackup: authBackup,
			AdminName:        displayName,
			APIKeyName:       apiKeyName,
		}
		installOpts.operationLock = deps.liveOperationLock.file
		installationRecovery = newDokployInstallationRecovery(dokployURL, installOpts)
		if err := markDokployInstallationRecoveryRequired(installationRecovery); err != nil {
			return fmt.Errorf("record Dokploy installation recovery state: %w", err)
		}
		installationRecoveryPending = true
		persistedRecovery, found, err := readDokployInstallationRecovery()
		if err != nil {
			return fmt.Errorf("read Dokploy installation recovery state: %w", err)
		}
		if !found {
			return errors.New("Dokploy installation recovery state disappeared before installation")
		}
		installationRecovery = persistedRecovery
		if installationRecovery.Phase != dokployInstallAPIKey {
			installer := deps.installer
			if installer == nil {
				installer = shellDokployInstaller{}
			}
			installTimeout := deps.installTimeout
			if installTimeout <= 0 {
				installTimeout = dokployInstallTimeout
			}
			installCtx, cancelInstall := context.WithTimeout(ctx, installTimeout)
			fmt.Fprintf(stdout, "Installing Dokploy in same-VPS shadow mode at %s\n", dokployURL)
			installErr := installer.InstallDokploy(installCtx, installOpts, stdout, stderr)
			installContextErr := installCtx.Err()
			cancelInstall()
			if installErr != nil {
				if errors.Is(installContextErr, context.DeadlineExceeded) {
					return fmt.Errorf("install dokploy timed out after %s: %w", installTimeout, installContextErr)
				}
				if installerRefusedForeignService(installErr) {
					if err := clearDokployInstallationRecoveryRequired(); err != nil {
						return fmt.Errorf("clear Dokploy installation recovery state after refusing a foreign service: %w", err)
					}
					return fmt.Errorf("install dokploy: the existing Dokploy service was not installed by Bort, so --install cannot adopt or repair it; bootstrap it without --install using the URL reported above: %s init-target --dokploy-url <existing Dokploy URL>", installationRecovery.CommandPrefix)
				}
				return fmt.Errorf("install dokploy: %w", installErr)
			}
			fmt.Fprintf(stdout, "Waiting for Dokploy to answer at %s\n", dokployURL)
			if err := waitForDokployHTTP(ctx, dokployURL, 2*time.Minute); err != nil {
				return fmt.Errorf("wait for dokploy at %s: %w", dokployURL, err)
			}
			persistedRecovery, found, err = readDokployInstallationRecovery()
			if err != nil {
				return fmt.Errorf("read Dokploy installation recovery state: %w", err)
			}
			if !found {
				return errors.New("Dokploy installation recovery state disappeared before credential bootstrap")
			}
			installationRecovery = persistedRecovery
		}
	}

	client := deps.newClient(dokployURL)
	if deps.verifyLocalService != nil {
		if err := deps.verifyLocalService(ctx, client); err != nil {
			return fmt.Errorf("verify local Dokploy service before bootstrap: %w", err)
		}
	}
	if err := client.SignUpAdmin(ctx, displayName, admin.Email, password); err != nil {
		return fmt.Errorf("dokploy sign-up: %w", err)
	}
	cookie, err := client.SignIn(ctx, admin.Email, password)
	if err != nil {
		return fmt.Errorf("dokploy sign-in: %w", err)
	}
	var (
		orgID   string
		apiKeys []dokploy.APIKey
	)
	if installationRecoveryPending {
		orgID, apiKeys, err = client.GetCurrentUserOrgAndAPIKeys(ctx, cookie)
	} else {
		orgID, err = client.GetCurrentUserOrg(ctx, cookie)
	}
	if err != nil {
		return fmt.Errorf("dokploy user.get: %w", err)
	}
	if installationRecoveryPending {
		installationRecovery, err = prepareDokployAPIKeyRecovery(ctx, client, cookie, installationRecovery, apiKeys)
		if err != nil {
			return err
		}
	}
	apiKeyPrefix := ""
	if installationRecoveryPending {
		apiKeyPrefix = dokployInstallAPIKeyPrefix(installationRecovery)
	}
	apiKeyID, apiKey, err := client.CreateAPIKey(ctx, cookie, apiKeyName, apiKeyPrefix, orgID)
	if err != nil {
		return fmt.Errorf("dokploy createApiKey: %w", err)
	}
	if installationRecoveryPending {
		installationRecovery.APIKeyCreatedID = apiKeyID
		installationRecovery = canonicalDokployInstallationRecovery(installationRecovery)
		if err := markDokployInstallationRecoveryRequired(installationRecovery); err != nil {
			return fmt.Errorf("record created Dokploy API-key identity: %w", err)
		}
	}

	creds := targetCredentials{
		URL:            client.BaseURL,
		Token:          apiKey,
		AdminEmail:     admin.Email,
		BootstrappedAt: time.Now().UTC(),
		APIKeyName:     apiKeyName,
	}
	if err := mutateBortState(deps.statePath, func(state *bortState) bool {
		*state = setTargetCredentials(*state, target, creds)
		return true
	}); err != nil {
		return err
	}
	if installationRecoveryPending {
		if err := clearDokployInstallationRecoveryRequired(); err != nil {
			return fmt.Errorf("clear reconciled Dokploy installation recovery state: %w", err)
		}
	}

	fmt.Fprintf(stdout, "Dokploy setup complete for %s at %s\n", admin.Email, client.BaseURL)
	fmt.Fprintln(stdout, "Bort can now continue with this migration.")
	return nil
}

func prepareDokployAPIKeyRecovery(ctx context.Context, client *dokploy.Client, cookie string, recovery dokployInstallationRecovery, keys []dokploy.APIKey) (dokployInstallationRecovery, error) {
	if recovery.Phase == dokployInstallAPIKey {
		baseline := make(map[string]struct{}, len(recovery.APIKeyBaselineIDs))
		for _, id := range recovery.APIKeyBaselineIDs {
			baseline[id] = struct{}{}
		}
		matching := map[string]dokploy.APIKey{}
		for _, key := range keys {
			if key.Name != recovery.APIKeyName || key.Prefix != dokployInstallAPIKeyPrefix(recovery) {
				continue
			}
			if _, existed := baseline[key.ID]; !existed {
				matching[key.ID] = key
			}
		}
		if recovery.APIKeyCreatedID == "" {
			if len(matching) > 0 {
				return recovery, fmt.Errorf("refusing to delete %d post-baseline Dokploy API key(s) named %q with recovery prefix %q because no created key ID was durably recorded; inspect them manually before retrying", len(matching), recovery.APIKeyName, dokployInstallAPIKeyPrefix(recovery))
			}
			return recovery, nil
		}
		if key, found := matching[recovery.APIKeyCreatedID]; found {
			if err := client.DeleteAPIKey(ctx, cookie, key.ID); err != nil {
				return recovery, fmt.Errorf("delete orphaned Dokploy API key %s: %w", key.ID, err)
			}
			delete(matching, key.ID)
		} else {
			for _, key := range keys {
				if key.ID == recovery.APIKeyCreatedID {
					return recovery, fmt.Errorf("refusing to delete Dokploy API key %s because its name or prefix no longer matches the durable creation intent", key.ID)
				}
			}
		}
		recovery.APIKeyCreatedID = ""
		recovery = canonicalDokployInstallationRecovery(recovery)
		if err := markDokployInstallationRecoveryRequired(recovery); err != nil {
			return recovery, fmt.Errorf("clear replaced Dokploy API-key identity: %w", err)
		}
		if len(matching) > 0 {
			return recovery, fmt.Errorf("refusing to create another Dokploy API key while %d unowned post-baseline key(s) named %q with recovery prefix %q remain; inspect them manually before retrying", len(matching), recovery.APIKeyName, dokployInstallAPIKeyPrefix(recovery))
		}
		return recovery, nil
	}
	recovery.Phase = dokployInstallAPIKey
	recovery.APIKeyBaselineIDs = make([]string, 0, len(keys))
	for _, key := range keys {
		recovery.APIKeyBaselineIDs = append(recovery.APIKeyBaselineIDs, key.ID)
	}
	recovery = canonicalDokployInstallationRecovery(recovery)
	if err := markDokployInstallationRecoveryRequired(recovery); err != nil {
		return recovery, fmt.Errorf("record Dokploy API-key recovery state: %w", err)
	}
	return recovery, nil
}

func dokployInstallAPIKeyPrefix(recovery dokployInstallationRecovery) string {
	return "bort_" + recovery.Identity[:12]
}

func validateDokployBootstrapURL(raw string) error {
	parsed, err := url.Parse(strings.TrimSpace(raw))
	if err != nil || parsed.Scheme == "" || parsed.Host == "" {
		return errors.New("invalid --dokploy-url: must be an absolute URL with a host")
	}
	if parsed.User != nil {
		return fmt.Errorf("--dokploy-url must not embed credentials")
	}
	if parsed.Scheme != "http" && parsed.Scheme != "https" {
		return fmt.Errorf("--dokploy-url must use direct HTTP on loopback for same-VPS bootstrap")
	}
	host := parsed.Hostname()
	loopback := strings.EqualFold(host, "localhost")
	if ip := net.ParseIP(host); ip != nil && ip.IsLoopback() {
		loopback = true
	}
	if !loopback {
		return fmt.Errorf("refusing same-VPS Dokploy bootstrap through non-loopback host %q; use http://127.0.0.1:<port>", host)
	}
	if parsed.Scheme != "http" {
		return fmt.Errorf("--dokploy-url must use the direct loopback HTTP endpoint, such as http://127.0.0.1:<port>")
	}
	if parsed.Path != "" && parsed.Path != "/" || parsed.RawPath != "" || parsed.RawQuery != "" || parsed.ForceQuery || parsed.Fragment != "" {
		return fmt.Errorf("--dokploy-url must be the root of the direct loopback HTTP endpoint")
	}
	port, err := strconv.ParseUint(parsed.Port(), 10, 16)
	if err != nil || port == 0 {
		return fmt.Errorf("--dokploy-url must include the direct local Dokploy service port")
	}
	return nil
}

var dokployVersionTagPattern = regexp.MustCompile(`^[A-Za-z0-9_][A-Za-z0-9_.-]{0,127}$`)
var dokployReleaseTagPattern = regexp.MustCompile(`^v([0-9]+)\.([0-9]+)\.([0-9]+)(?:-(.+))?$`)
var dockerSHA256DigestPattern = regexp.MustCompile(`^sha256:[0-9a-f]{64}$`)

// keep the acme email inside a yaml-plain-safe character class: it is
// interpolated unquoted into traefik.yml by the install script.
var acmeEmailSafePattern = regexp.MustCompile(`^[A-Za-z0-9._%+-]+@[A-Za-z0-9.-]+\.[A-Za-z]{2,}$`)

func validateInstallPort(port string) error {
	trimmed := strings.TrimSpace(port)
	n, err := strconv.Atoi(trimmed)
	if err != nil || n < 1 || n > 65535 || strconv.Itoa(n) != trimmed {
		return fmt.Errorf("invalid --install-port %q: must be an integer between 1 and 65535", port)
	}
	return nil
}

func validateInstallURLPort(rawURL, installPort string) error {
	parsed, err := url.Parse(strings.TrimSpace(rawURL))
	if err != nil {
		return err
	}
	urlPort := parsed.Port()
	if urlPort != strings.TrimSpace(installPort) {
		return fmt.Errorf("--dokploy-url port %s does not match --install-port %s; the install publishes Dokploy on --install-port, so use http://127.0.0.1:%s or pass --install-port %s", urlPort, installPort, installPort, urlPort)
	}
	return nil
}

func normalizeSwarmAddressPool(raw string) (string, error) {
	raw = strings.TrimSpace(raw)
	if strings.EqualFold(raw, "auto") {
		return "auto", nil
	}
	prefix, err := netip.ParsePrefix(raw)
	if err != nil || !prefix.Addr().Is4() || prefix.Bits() > 24 {
		return "", fmt.Errorf("invalid --swarm-addr-pool %q: must be auto or an IPv4 CIDR with a prefix length from 0 through 24", raw)
	}
	return prefix.Masked().String(), nil
}

func validateDokployAPIKeyName(name string) error {
	length := utf8.RuneCountInString(name)
	if !utf8.ValidString(name) || length < 1 || length > 32 {
		return fmt.Errorf("invalid --api-key-name: must contain 1 to 32 characters")
	}
	return nil
}

func validateAuthSecretBackupPath(path string) error {
	trimmed := strings.TrimSpace(path)
	if trimmed == "" {
		return fmt.Errorf("--auth-secret-backup is required with --install (or set %s); use an absolute path on encrypted or off-host storage and retain it after installation", envDokployAuthBackup)
	}
	if !filepath.IsAbs(trimmed) {
		return fmt.Errorf("invalid --auth-secret-backup %q: use an absolute path on encrypted or off-host storage", path)
	}
	return nil
}

func validateDokployVersionTag(tag string) error {
	if !dokployVersionTagPattern.MatchString(tag) {
		return fmt.Errorf("invalid --dokploy-version %q: must be a docker tag matching [A-Za-z0-9_][A-Za-z0-9_.-]{0,127}", tag)
	}
	matches := dokployReleaseTagPattern.FindStringSubmatch(tag)
	if matches == nil {
		return fmt.Errorf("unsupported --dokploy-version %q: use a release tag at v0.29.3 or newer so Dokploy can load its authentication key from a Docker secret", tag)
	}
	major, majorErr := strconv.ParseUint(matches[1], 10, 64)
	minor, minorErr := strconv.ParseUint(matches[2], 10, 64)
	patch, patchErr := strconv.ParseUint(matches[3], 10, 64)
	if majorErr != nil || minorErr != nil || patchErr != nil {
		return fmt.Errorf("invalid --dokploy-version %q: numeric version component is too large", tag)
	}
	if major == 0 && (minor < 29 || minor == 29 && (patch < 3 || patch == 3 && matches[4] != "")) {
		return fmt.Errorf("unsupported --dokploy-version %q: Bort requires v0.29.3 or newer so Dokploy can load its authentication key from a Docker secret", tag)
	}
	return nil
}

func normalizeDokployVersion(version string) (string, error) {
	version = strings.TrimSpace(version)
	tag, digest, qualified := strings.Cut(version, "@")
	if err := validateDokployVersionTag(tag); err != nil {
		return "", err
	}
	if !qualified {
		if tag == defaultDokployVersion {
			return tag + "@" + defaultDokployDigest, nil
		}
		return "", fmt.Errorf("unsupported --dokploy-version %q: nondefault releases require an independently reviewed digest-qualified reference such as %s@sha256:<64 lowercase hex characters>", version, tag)
	}
	if !dockerSHA256DigestPattern.MatchString(digest) {
		return "", fmt.Errorf("invalid --dokploy-version %q: digest must be sha256 followed by 64 lowercase hexadecimal characters", version)
	}
	return tag + "@" + digest, nil
}

func validateACMEEmail(email string) error {
	parsed, err := mail.ParseAddress(email)
	if err != nil || parsed.Address != email {
		return fmt.Errorf("invalid coolify admin email %q for ACME registration", email)
	}
	if !acmeEmailSafePattern.MatchString(email) {
		return fmt.Errorf("coolify admin email %q contains characters unsafe for the ACME config; use --dokploy-url with an existing dokploy instead", email)
	}
	return nil
}

func (shellDokployInstaller) InstallDokploy(ctx context.Context, opts dokployInstallOptions, stdout, stderr io.Writer) error {
	env, err := dockercli.LocalEnvironment(os.Environ())
	if err != nil {
		return err
	}
	authSecret, authSecretDigest, err := prepareDokployAuthSecretEscrow(opts.AuthSecretBackup)
	if err != nil {
		return err
	}
	defer authSecret.Close()
	installCtx, stopSignals := dokployInstallerSignalContext(ctx)
	defer stopSignals()
	cmd := exec.CommandContext(installCtx, "bash", "-s")
	cmd.Stdin = strings.NewReader(dokployShadowInstallScript)
	authSecretFD := configureDokployInstallerCommand(cmd, opts.operationLock, authSecret)
	progress := newInstallProgressWriter(stdout)
	cmd.Stdout = progress
	cmd.Stderr = stderr
	if strings.TrimSpace(opts.HostPort) != "" {
		env = append(env, "HOST_PORT="+strings.TrimSpace(opts.HostPort))
	}
	if strings.TrimSpace(opts.AddrPool) != "" {
		env = append(env, "ADDR_POOL="+strings.TrimSpace(opts.AddrPool))
	}
	if strings.TrimSpace(opts.Version) != "" {
		env = append(env, "DOKPLOY_VERSION="+strings.TrimSpace(opts.Version))
	}
	if strings.TrimSpace(opts.EndpointMode) != "" {
		env = append(env, envDokployEndpointMode+"="+strings.TrimSpace(opts.EndpointMode))
	}
	if strings.TrimSpace(opts.ACMEEmail) != "" {
		env = append(env, "ACME_EMAIL="+strings.TrimSpace(opts.ACMEEmail))
	}
	if strings.TrimSpace(opts.AuthSecretBackup) != "" {
		env = append(env, "DOKPLOY_AUTH_SECRET_BACKUP="+strings.TrimSpace(opts.AuthSecretBackup))
	}
	env = append(env,
		"DOKPLOY_AUTH_SECRET_FD="+strconv.Itoa(authSecretFD),
		"DOKPLOY_AUTH_SECRET_DIGEST="+authSecretDigest,
	)
	cmd.Env = env
	err = cmd.Run()
	if flushErr := progress.Flush(); flushErr != nil && err == nil {
		err = flushErr
	}
	if err == nil {
		err = verifyDokployAuthSecretEscrow(opts.AuthSecretBackup, authSecretDigest)
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
	stderr     io.Writer
	runCommand func(ctx context.Context, name string, args ...string) ([]byte, error)
}

func (l *coolifyDBLister) warnf(format string, args ...any) {
	w := l.stderr
	if w == nil {
		w = os.Stderr
	}
	fmt.Fprintf(w, format, args...)
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
	pgpassPath, cleanup, err := stageCoolifyPgpass(ctx, runCommand, container, creds.Password)
	if err != nil {
		return nil, err
	}
	defer func() {
		if err := cleanup(); err != nil {
			l.warnf("warning: %v; the file holds the coolify database password, remove it manually\n", err)
		}
	}()
	args := []string{
		"exec",
		"-e", "PGPASSFILE=" + pgpassPath,
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

func coolifyPgpassFileContent(password string) string {
	escaped := strings.NewReplacer(`\`, `\\`, `:`, `\:`).Replace(password)
	return "*:*:*:*:" + escaped + "\n"
}

// the coolify db password must not ride on process argv, so it is staged as a
// private .pgpass file and referenced by path only.
func stageCoolifyPgpass(ctx context.Context, runCommand func(context.Context, string, ...string) ([]byte, error), container, password string) (string, func() error, error) {
	noop := func() error { return nil }
	if strings.ContainsAny(password, "\r\n") {
		return "", noop, fmt.Errorf("coolify database password must not contain newline characters")
	}
	tmp, err := os.CreateTemp("", "bort-pgpass-")
	if err != nil {
		return "", noop, fmt.Errorf("create pgpass file: %w", err)
	}
	hostPath := tmp.Name()
	removeHost := func() { _ = os.Remove(hostPath) }
	if _, err := tmp.WriteString(coolifyPgpassFileContent(password)); err != nil {
		_ = tmp.Close()
		removeHost()
		return "", noop, fmt.Errorf("write pgpass file: %w", err)
	}
	if err := tmp.Close(); err != nil {
		removeHost()
		return "", noop, fmt.Errorf("close pgpass file: %w", err)
	}

	containerPath := "/tmp/" + filepath.Base(hostPath)
	if _, err := runCommand(ctx, "docker", "cp", hostPath, container+":"+containerPath); err != nil {
		removeHost()
		cleanupCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		_, _ = runCommand(cleanupCtx, "docker", "exec", container, "rm", "-f", containerPath)
		cancel()
		return "", noop, fmt.Errorf("stage pgpass file in %s: %w", container, err)
	}
	removeHost()
	cleanup := func() error {
		hostErr := os.Remove(hostPath)
		if errors.Is(hostErr, os.ErrNotExist) {
			hostErr = nil
		}
		if hostErr != nil {
			hostErr = fmt.Errorf("remove host pgpass file: %w", hostErr)
		}
		cleanupCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		_, rmErr := runCommand(cleanupCtx, "docker", "exec", container, "rm", "-f", containerPath)
		if rmErr != nil {
			rmErr = fmt.Errorf("remove pgpass file %s from container %s: %w", containerPath, container, rmErr)
		}
		return errors.Join(hostErr, rmErr)
	}
	return containerPath, cleanup, nil
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
	env, err := dockercli.LocalEnvironment(os.Environ())
	if err != nil {
		return nil, err
	}
	cmd := exec.CommandContext(ctx, name, args...)
	cmd.Env = env
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
VERSION_REF="${DOKPLOY_VERSION:?DOKPLOY_VERSION must be a digest-qualified release reference}"
VERSION_TAG="${VERSION_REF%@*}"
REQUESTED_DOKPLOY_IMAGE="dokploy/dokploy:${VERSION_REF}"
ACME_EMAIL="${ACME_EMAIL:-admin@dokploy.local}"
TRAEFIK_IMAGE="${DOKPLOY_TRAEFIK_IMAGE:-traefik:v3.6.7}"
ENDPOINT_MODE="${ENDPOINT_MODE:-vip}"
AUTH_SECRET_BACKUP="${DOKPLOY_AUTH_SECRET_BACKUP:-}"
AUTH_SECRET_FD="${DOKPLOY_AUTH_SECRET_FD:?DOKPLOY_AUTH_SECRET_FD is required}"
AUTH_SECRET_DIGEST="${DOKPLOY_AUTH_SECRET_DIGEST:?DOKPLOY_AUTH_SECRET_DIGEST is required}"
AUTH_SECRET_FD_PATH="/dev/fd/$AUTH_SECRET_FD"
AUTH_SECRET_PROVENANCE=/var/lib/bort/dokploy-auth-secret.id
AUTH_SECRET_INTENT_LABEL=io.bort.auth-secret-intent
INSTALL_AUTH_SECRET_LABEL=io.bort.install-auth-secret-id
FOREIGN_SERVICE_EXIT_STATUS=75

validate_endpoint_mode() {
    case "$1" in
        vip)
            if grep -Eq '^(# CONFIG_IP_VS is not set|CONFIG_IP_VS=n)$' <<<"$2"; then
                echo "Docker Swarm VIP mode requires kernel IPVS support; use ENDPOINT_MODE=dnsrr for Dokploy's control-plane services or a kernel with IPVS support" >&2
                return 1
            fi
            ;;
        dnsrr) ;;
        *) echo "ENDPOINT_MODE must be vip or dnsrr" >&2; return 1 ;;
    esac
}

kernel_config=""
if [ -r /proc/config.gz ]; then
    kernel_config="$(gzip -dc /proc/config.gz 2>/dev/null || true)"
elif [ -r "/boot/config-$(uname -r)" ]; then
    kernel_config="$(cat "/boot/config-$(uname -r)")"
fi
validate_endpoint_mode "$ENDPOINT_MODE" "$kernel_config"

if ! [[ "$ACME_EMAIL" =~ ^[A-Za-z0-9._%+-]+@[A-Za-z0-9.-]+\.[A-Za-z]{2,}$ ]]; then
    echo "invalid ACME_EMAIL \"$ACME_EMAIL\"" >&2
    exit 1
fi

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
if ! command -v python3 >/dev/null 2>&1; then
    echo "python3 is required before installing Dokploy" >&2
    exit 1
fi
if [ -z "$AUTH_SECRET_BACKUP" ]; then
    echo "DOKPLOY_AUTH_SECRET_BACKUP is required; set it to an absolute path on encrypted or off-host storage and retain it after installation" >&2
    exit 1
fi
case "$AUTH_SECRET_BACKUP" in
    /*) ;;
    *) echo "DOKPLOY_AUTH_SECRET_BACKUP must be an absolute path" >&2; exit 1 ;;
esac
AUTH_SECRET_BACKUP_DIR="$(dirname -- "$AUTH_SECRET_BACKUP")"
if [ ! -d "$AUTH_SECRET_BACKUP_DIR" ]; then
    echo "DOKPLOY_AUTH_SECRET_BACKUP parent directory does not exist: $AUTH_SECRET_BACKUP_DIR" >&2
    exit 1
fi
if [ -L "$AUTH_SECRET_BACKUP" ]; then
    echo "DOKPLOY_AUTH_SECRET_BACKUP must not be a symbolic link" >&2
    exit 1
fi

if ! SWARM_STATUS="$(docker info --format '{{.Swarm.LocalNodeState}} {{.Swarm.ControlAvailable}}' 2>/dev/null)"; then
    echo "could not inspect Docker swarm state; refusing to continue" >&2
    exit 1
fi
case "$SWARM_STATUS" in
    "inactive false"|"active true") ;;
    "active false") echo "this host is an active Docker swarm worker, not a manager; run Dokploy installation on a manager" >&2; exit 1 ;;
    *) echo "unsupported Docker swarm state \"$SWARM_STATUS\"; refusing to continue" >&2; exit 1 ;;
esac

for service in dokploy-postgres dokploy-redis dokploy; do
    if mode="$(docker service inspect "$service" --format '{{.Spec.EndpointSpec.Mode}}' 2>/dev/null)"; then
        if [ "$mode" != "$ENDPOINT_MODE" ]; then
            echo "existing service $service uses endpoint mode $mode, not $ENDPOINT_MODE; review and update its endpoint mode before rerunning (existing services are not changed automatically)" >&2
            exit 1
        fi
    fi
done

dokploy_image_supports_auth_secret_file() {
    local image="${1%@*}"
    local repository="${image%:*}"
    local tag="${image##*:}"
    case "$repository" in
        dokploy/dokploy|docker.io/dokploy/dokploy|index.docker.io/dokploy/dokploy) ;;
        *) return 1 ;;
    esac
    if ! [[ "$tag" =~ ^v([0-9]{1,9})\.([0-9]{1,9})\.([0-9]{1,9})(-(.+))?$ ]]; then
        return 1
    fi
    local major=$((10#${BASH_REMATCH[1]}))
    local minor=$((10#${BASH_REMATCH[2]}))
    local patch=$((10#${BASH_REMATCH[3]}))
    if (( major > 0 || minor > 29 || minor == 29 && patch > 3 )); then
        return 0
    fi
    (( major == 0 && minor == 29 && patch == 3 )) && [ -z "${BASH_REMATCH[4]:-}" ]
}

docker_object_exists() {
    local object_kind="$1"
    local object_name="$2"
    local absent_pattern="$3"
    local output
    if output="$(docker "$object_kind" inspect "$object_name" 2>&1)"; then
        return 0
    fi
    if grep -Eqi "$absent_pattern" <<<"$output"; then
        return 1
    fi
    echo "could not determine whether Docker $object_kind $object_name exists; refusing to continue: $output" >&2
    return 2
}

validate_auth_secret_backup() {
    if ! [[ "$AUTH_SECRET_FD" =~ ^[0-9]+$ ]]; then
        echo "authentication-secret escrow descriptor is unavailable" >&2
        return 1
    fi
    if ! : < "$AUTH_SECRET_FD_PATH" 2>/dev/null; then
        echo "authentication-secret escrow descriptor is unavailable" >&2
        return 1
    fi
	if ! grep -Eq '^[0-9a-f]{64}$' <<<"$AUTH_SECRET_DIGEST"; then
        echo "authentication-secret escrow digest validated by Bort is malformed" >&2
		return 1
	fi
}

prepare_auth_secret_backup() {
    validate_auth_secret_backup
}

current_auth_secret_id() {
    local secret_id
    if ! secret_id="$(docker secret inspect dokploy_auth_secret --format '{{.ID}}' 2>/dev/null)"; then
        echo "could not inspect Docker secret dokploy_auth_secret identity; refusing to continue" >&2
        return 1
    fi
    if ! grep -Eq '^[a-z0-9]{20,64}$' <<<"$secret_id"; then
        echo "Docker returned an invalid identity for secret dokploy_auth_secret; refusing to continue" >&2
        return 1
    fi
    printf '%s\n' "$secret_id"
}

validate_auth_secret_provenance() {
	local expected_secret_id="${1:-}" secret_id secret_intent backup_digest recorded_id recorded_digest recorded_intent extra
	validate_auth_secret_backup
    secret_id="$(current_auth_secret_id)"
	if [ -n "$expected_secret_id" ] && [ "$secret_id" != "$expected_secret_id" ]; then
		echo "Docker secret dokploy_auth_secret changed while Bort was creating it; refusing to continue" >&2
		return 1
	fi
	backup_digest="$AUTH_SECRET_DIGEST"
    if [ ! -f "$AUTH_SECRET_PROVENANCE" ] || [ -L "$AUTH_SECRET_PROVENANCE" ]; then
        echo "orphaned Docker secret dokploy_auth_secret has no trusted Bort provenance marker at $AUTH_SECRET_PROVENANCE; refusing to reuse or replace it" >&2
        return 1
    fi
    if [ "$(stat -c '%a' "$AUTH_SECRET_PROVENANCE")" != 600 ] || [ "$(stat -c '%u' "$AUTH_SECRET_PROVENANCE")" != "$(id -u)" ]; then
        echo "Dokploy authentication-secret provenance marker must be mode 0600 and owned by root: $AUTH_SECRET_PROVENANCE" >&2
        return 1
    fi
	IFS=' ' read -r recorded_id recorded_digest extra < "$AUTH_SECRET_PROVENANCE"
	if [ "$(wc -l < "$AUTH_SECRET_PROVENANCE")" -ne 1 ] || [ -n "${extra:-}" ] || ! grep -Eq '^[0-9a-f]{64}$' <<<"$recorded_digest"; then
        echo "Dokploy authentication-secret provenance marker is malformed: $AUTH_SECRET_PROVENANCE" >&2
        return 1
    fi
	if [[ "$recorded_id" == pending:* ]]; then
		recorded_intent="${recorded_id#pending:}"
		if ! grep -Eq '^[0-9a-f]{32}$' <<<"$recorded_intent"; then
			echo "Dokploy authentication-secret provenance marker is malformed: $AUTH_SECRET_PROVENANCE" >&2
			return 1
		fi
		if ! secret_intent="$(docker secret inspect dokploy_auth_secret --format "{{ index .Spec.Annotations.Labels \"$AUTH_SECRET_INTENT_LABEL\" }}" 2>/dev/null)"; then
			echo "could not inspect Docker secret dokploy_auth_secret creation intent; refusing to continue" >&2
			return 1
		fi
		if [ "$recorded_digest" != "$backup_digest" ] || [ "$recorded_intent" != "$secret_intent" ]; then
			echo "orphaned Docker secret dokploy_auth_secret or its retained escrow does not match the trusted Bort creation intent; refusing to continue" >&2
			return 1
		fi
		write_auth_secret_provenance "$secret_id"
		DOKPLOY_AUTH_SECRET_ID="$secret_id"
		return
	fi
	if ! grep -Eq '^[a-z0-9]{20,64}$' <<<"$recorded_id"; then
		echo "Dokploy authentication-secret provenance marker is malformed: $AUTH_SECRET_PROVENANCE" >&2
		return 1
	fi
	if [ "$recorded_id" != "$secret_id" ] || [ "$recorded_digest" != "$backup_digest" ]; then
		echo "orphaned Docker secret dokploy_auth_secret or its retained escrow does not match the trusted Bort provenance marker; refusing to continue" >&2
        return 1
    fi
	DOKPLOY_AUTH_SECRET_ID="$secret_id"
}

write_auth_secret_marker() {
    local marker_id="$1" backup_digest="$2" provenance_dir provenance_tmp
    provenance_dir="$(dirname -- "$AUTH_SECRET_PROVENANCE")"
    if [ -L "$provenance_dir" ] || { [ -e "$provenance_dir" ] && [ ! -d "$provenance_dir" ]; }; then
        echo "Dokploy provenance directory must be a real directory: $provenance_dir" >&2
        return 1
    fi
    install -d -m 700 -o "$(id -u)" -g "$(id -g)" "$provenance_dir"
    if [ "$(stat -c '%a' "$provenance_dir")" != 700 ] || [ "$(stat -c '%u' "$provenance_dir")" != "$(id -u)" ]; then
        echo "Dokploy provenance directory must be mode 0700 and owned by root: $provenance_dir" >&2
        return 1
    fi
    provenance_tmp="$(mktemp "$provenance_dir/.dokploy-auth-secret.id.XXXXXX")"
    printf '%s %s\n' "$marker_id" "$backup_digest" > "$provenance_tmp"
    chmod 600 "$provenance_tmp"
    sync -f "$provenance_tmp"
    mv -f -- "$provenance_tmp" "$AUTH_SECRET_PROVENANCE"
    sync -f "$provenance_dir"
}

write_auth_secret_intent() {
	local creation_intent backup_digest
	validate_auth_secret_backup
	creation_intent="$(openssl rand -hex 16)"
	if ! grep -Eq '^[0-9a-f]{32}$' <<<"$creation_intent"; then
		echo "openssl did not produce the required authentication-secret creation intent" >&2
		return 1
	fi
	backup_digest="$AUTH_SECRET_DIGEST"
	write_auth_secret_marker "pending:$creation_intent" "$backup_digest"
	printf '%s\n' "$creation_intent"
}

write_auth_secret_provenance() {
    local secret_id="$1" backup_digest
	validate_auth_secret_backup
	backup_digest="$AUTH_SECRET_DIGEST"
	write_auth_secret_marker "$secret_id" "$backup_digest"
}

validate_bort_install_service() {
	local service_state="$1" service_image install_secret_id bound_secret_ids auth_file_count
	if ! install_secret_id="$(docker service inspect dokploy --format "{{ index .Spec.Annotations.Labels \"$INSTALL_AUTH_SECRET_LABEL\" }}" 2>/dev/null)" || ! grep -Eq '^[a-z0-9]{20,64}$' <<<"$install_secret_id"; then
		echo "the $service_state Dokploy service is not labeled with a valid Bort authentication-secret identity; refusing installation recovery" >&2
		return 1
	fi
	validate_auth_secret_provenance
	if [ "$install_secret_id" != "$DOKPLOY_AUTH_SECRET_ID" ]; then
		echo "the $service_state Dokploy service is not bound to the reviewed Bort authentication-secret identity; refusing installation recovery" >&2
		return 1
	fi
	if ! service_image="$(docker service inspect dokploy --format '{{.Spec.TaskTemplate.ContainerSpec.Image}}' 2>/dev/null)"; then
		echo "could not inspect the $service_state Dokploy service image; refusing installation recovery" >&2
		return 1
	fi
	case "$service_image" in
		"$REQUESTED_DOKPLOY_IMAGE"|"docker.io/$REQUESTED_DOKPLOY_IMAGE"|"index.docker.io/$REQUESTED_DOKPLOY_IMAGE") ;;
		*)
			echo "the $service_state Dokploy service does not use the requested immutable image; refusing installation recovery" >&2
			return 1
			;;
	esac
	auth_file_count="$(grep -Ec '^BETTER_AUTH_SECRET_FILE=' <<<"$DOKPLOY_AUTH_ENV" || true)"
	if [ "$auth_file_count" -ne 1 ] || ! grep -Fqx 'BETTER_AUTH_SECRET_FILE=/run/secrets/dokploy_auth_secret' <<<"$DOKPLOY_AUTH_ENV"; then
		echo "the $service_state Dokploy service does not use exactly Bort's authentication-secret mount path; refusing installation recovery" >&2
		return 1
	fi
	if ! bound_secret_ids="$(docker service inspect dokploy --format '{{range .Spec.TaskTemplate.ContainerSpec.Secrets}}{{if eq .SecretName "dokploy_auth_secret"}}{{println .SecretID}}{{end}}{{end}}' 2>/dev/null)"; then
		echo "could not verify the authentication secret bound to the $service_state Dokploy service; refusing installation recovery" >&2
		return 1
	fi
	set -- $bound_secret_ids
	if [ "$#" -ne 1 ] || [ "$1" != "$DOKPLOY_AUTH_SECRET_ID" ]; then
		echo "the $service_state Dokploy service does not bind exactly the reviewed authentication-secret identity; refusing installation recovery" >&2
		return 1
	fi
}

DOKPLOY_AUTH_SECRET_ID=
DOKPLOY_AUTH_SECRET_EXISTS=false
if docker_object_exists secret dokploy_auth_secret 'no such (secret|object)|not a swarm manager'; then
    DOKPLOY_AUTH_SECRET_EXISTS=true
else
    inspect_status=$?
    if [ "$inspect_status" -ne 1 ]; then
        exit 1
    fi
fi

DOKPLOY_SERVICE_EXISTS=false
if docker_object_exists service dokploy 'no such (service|object)|not a swarm manager'; then
    DOKPLOY_SERVICE_EXISTS=true
    if ! DOKPLOY_AUTH_ENV="$(docker service inspect dokploy --format '{{range .Spec.TaskTemplate.ContainerSpec.Env}}{{println .}}{{end}}' 2>/dev/null)"; then
        echo "could not inspect the existing Dokploy service authentication configuration; refusing to continue" >&2
        exit 1
    fi
    DOKPLOY_AUTH_SECRET_REFUSAL=
	DOKPLOY_INLINE_AUTH_COUNT="$(grep -Ec '^BETTER_AUTH_SECRET=' <<<"$DOKPLOY_AUTH_ENV" || true)"
	if [ "$DOKPLOY_INLINE_AUTH_COUNT" -gt 0 ]; then
		if [ "$DOKPLOY_INLINE_AUTH_COUNT" -ne 1 ]; then
			echo "existing Dokploy service uses duplicate BETTER_AUTH_SECRET entries or a value that is shorter than Better Auth's 32-character minimum or is not a 64-lowercase-hex key; rotate it with a Dokploy-supported maintenance procedure and a cryptographically random 32-byte key before rerunning" >&2
			exit 1
		fi
		if grep -Fqx 'BETTER_AUTH_SECRET=better-auth-secret-123456789' <<<"$DOKPLOY_AUTH_ENV"; then
			DOKPLOY_AUTH_SECRET_REFUSAL="existing Dokploy service explicitly uses the published legacy authentication secret; refusing bootstrap until it is migrated because that value cannot securely sign authentication artifacts."
		elif ! LC_ALL=C grep -Eq '^BETTER_AUTH_SECRET=[0-9a-f]{64}$' <<<"$DOKPLOY_AUTH_ENV"; then
			echo "existing Dokploy service uses duplicate BETTER_AUTH_SECRET entries or a value that is shorter than Better Auth's 32-character minimum or is not a 64-lowercase-hex key; rotate it with a Dokploy-supported maintenance procedure and a cryptographically random 32-byte key before rerunning" >&2
			exit 1
		fi
    elif grep -Eq '^BETTER_AUTH_SECRET_FILE=.+$' <<<"$DOKPLOY_AUTH_ENV"; then
        if ! DOKPLOY_IMAGE="$(docker service inspect dokploy --format '{{.Spec.TaskTemplate.ContainerSpec.Image}}' 2>/dev/null)"; then
            echo "could not inspect the existing Dokploy service image; refusing to continue" >&2
            exit 1
        fi
        if ! dokploy_image_supports_auth_secret_file "$DOKPLOY_IMAGE"; then
            echo "existing Dokploy service uses BETTER_AUTH_SECRET_FILE with image \"$DOKPLOY_IMAGE\", but Bort cannot verify file-backed authentication-secret support; upgrade to dokploy/dokploy:v0.29.3 or newer, then rerun" >&2
            exit 1
        fi
    else
        DOKPLOY_AUTH_SECRET_REFUSAL="existing Dokploy service has no authentication secret explicitly configured; refusing to generate a new key because changing the legacy fallback can make encrypted settings unreadable."
    fi
    if [ -n "$DOKPLOY_AUTH_SECRET_REFUSAL" ]; then
        printf '%s\n' "$DOKPLOY_AUTH_SECRET_REFUSAL" >&2
        cat >&2 <<'EOF'
Before rotating it: schedule maintenance; inspect the service's published ports; fence every inbound Dokploy UI/API path at the firewall or external load balancer, including each published host port targeting container port 3000 (Bort defaults to host port 3030, while --install-port may differ) and dashboard routes on 80/443; verify those paths are unreachable; and keep the fence active through the service restart. Then back up the complete Dokploy PostgreSQL database and /etc/dokploy; validate and preserve every existing 64-lowercase-hex line in /etc/dokploy/encryption.key and atomically add HMAC-SHA256(key='better-auth-secret-123456789', data='dokploy:db-encryption:v1') only if absent; upgrade Dokploy to v0.29.5 or newer; and verify the running service image. Do not use the hosted 0.29.3.sh wrapper because it treats a 120-second migration timeout as success. Run this no-timeout sequence instead:
  (
    set -eu
    if sudo docker secret inspect dokploy-auth-secret >/dev/null 2>&1; then
      echo 'refusing to run migration while Docker secret dokploy-auth-secret already exists; after an interrupted attempt, keep the write fence active and use the migration guide two-factor compare-and-swap recovery before removing the unused secret' >&2
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
	: "${DOKPLOY_TWO_FACTOR_BEFORE:?set DOKPLOY_TWO_FACTOR_BEFORE to a new absolute snapshot path on separately backed-up encrypted or off-host storage}"
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
	set -- $(sudo docker ps --filter label=com.docker.swarm.service.name=dokploy --format '{{.ID}}')
	test "$#" -eq 1
	set -- $(sudo docker ps --filter label=com.docker.swarm.service.name=dokploy-postgres --format '{{.ID}}')
	test "$#" -eq 1
	dokploy_postgres_container="$1"
	database_settled=false
	settle_attempt=0
	while [ "$settle_attempt" -lt 30 ]; do
	  active_sessions="$(sudo docker exec "$dokploy_postgres_container" psql -U dokploy -d dokploy -X -v ON_ERROR_STOP=1 -Atc "SELECT count(*) FROM pg_stat_activity WHERE datname = current_database() AND pid <> pg_backend_pid() AND state <> 'idle'")"
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
	sudo docker exec "$dokploy_postgres_container" psql -U dokploy -d dokploy -X -v ON_ERROR_STOP=1 -c "COPY (SELECT id, secret, backup_codes FROM public.two_factor ORDER BY id) TO STDOUT WITH (FORMAT csv, HEADER true, NULL '\N', FORCE_QUOTE *)" > "$two_factor_tmp"
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
    sudo docker service update --secret-add source=dokploy-auth-secret,target=/run/secrets/dokploy-auth-secret --update-order stop-first dokploy
    set -- $(sudo docker ps --filter label=com.docker.swarm.service.name=dokploy --format '{{.ID}}')
    test "$#" -eq 1
    dokploy_container="$1"
    sudo docker exec -e OLD_SECRET=better-auth-secret-123456789 "$dokploy_container" sh -c 'cd /app && NEW_SECRET="$(cat /run/secrets/dokploy-auth-secret)" pnpm run migrate-auth-secret'
    sudo docker service update --env-add BETTER_AUTH_SECRET_FILE=/run/secrets/dokploy-auth-secret --env-rm BETTER_AUTH_SECRET --update-order stop-first dokploy
    shred -u -- "$auth_secret_tmp"
    auth_secret_tmp=
    trap - HUP INT TERM
    trap - EXIT
  )
Retain DOKPLOY_AUTH_SECRET_BACKUP and DOKPLOY_TWO_FACTOR_BEFORE after success. Keep the inbound write fence active after an interruption. If neither the post-quiescence DOKPLOY_TWO_FACTOR_BEFORE snapshot nor the Docker secret exists, rerun the exact command under the fence. If the snapshot exists but the secret does not, rerunning replaces and settles the legacy task again and requires the snapshot to match. Once the Docker secret exists, do not rerun; confirm no migrate-auth-secret process remains. If Dokploy still uses the legacy fallback, capture an immediate post-interruption snapshot of only public.two_factor id, secret, and backup_codes, then use the compare-and-swap recovery in Bort's migration guide; do not restore the full PostgreSQL database because unrelated worker writes may have continued. If the service sets BETTER_AUTH_SECRET_FILE, do not remove the secret or rewrite the database; verify login, two-factor authentication, and stored values.
Restart Dokploy and verify representative application, Compose, project, environment, database, build-argument, and build-secret values render as plaintext before any deploy. The official migration re-encrypts only Better Auth two-factor secret and backup-code fields; it does not migrate those Dokploy configuration fields. See 'Recover an existing Dokploy service using the legacy secret' in Bort's migration guide for exact commands.
EOF
        exit 1
    fi
    if ! DOKPLOY_INSTALL_LABEL="$(docker service inspect dokploy --format "{{ index .Spec.Annotations.Labels \"$INSTALL_AUTH_SECRET_LABEL\" }}" 2>/dev/null)"; then
        echo "could not inspect the existing Dokploy service labels; refusing to continue" >&2
        exit 1
    fi
    if [ -z "$DOKPLOY_INSTALL_LABEL" ]; then
        DOKPLOY_EXISTING_PORT="$(docker service inspect dokploy --format '{{range .Endpoint.Ports}}{{if eq .TargetPort 3000}}{{.PublishedPort}}{{end}}{{end}}' 2>/dev/null | head -n1)"
        echo "existing Dokploy service was not installed by Bort, so --install cannot adopt or repair it; it answers at http://127.0.0.1:${DOKPLOY_EXISTING_PORT:-<port>}" >&2
        exit "$FOREIGN_SERVICE_EXIT_STATUS"
    fi
else
    inspect_status=$?
    if [ "$inspect_status" -ne 1 ]; then
        exit 1
    fi
fi
if [ "$DOKPLOY_SERVICE_EXISTS" = false ] && [ "$DOKPLOY_AUTH_SECRET_EXISTS" = false ]; then
	if [ "$SWARM_STATUS" = "active true" ]; then
		if ! SWARM_MANAGER_COUNT="$(docker node ls --filter role=manager --format '{{.ID}}' | awk 'NF { count++ } END { print count + 0 }')"; then
			echo "could not enumerate Docker swarm managers before checking for orphaned Dokploy database state; refusing to continue" >&2
			exit 1
		fi
		if [ "$SWARM_MANAGER_COUNT" -ne 1 ]; then
			echo "existing multi-manager Docker swarm can hide a node-local dokploy-postgres volume on another manager; automatic fresh install is unsupported unless the original Bort-managed dokploy_auth_secret and /var/lib/bort/dokploy-auth-secret.id provenance marker are restored together from a consistent host backup" >&2
			exit 1
		fi
	fi
    DOKPLOY_DATABASE_STATE_EXISTS=false
    if docker_object_exists service dokploy-postgres 'no such (service|object)|not a swarm manager'; then
        DOKPLOY_DATABASE_STATE_EXISTS=true
    else
        inspect_status=$?
        if [ "$inspect_status" -ne 1 ]; then
            exit 1
        fi
    fi
    if docker_object_exists volume dokploy-postgres 'no such (volume|object)'; then
        DOKPLOY_DATABASE_STATE_EXISTS=true
    else
        inspect_status=$?
        if [ "$inspect_status" -ne 1 ]; then
            exit 1
        fi
    fi
    if [ "$DOKPLOY_DATABASE_STATE_EXISTS" = true ]; then
        echo "existing Dokploy database state has no reusable dokploy_auth_secret; refusing to generate a new key because changing it can invalidate sessions and encrypted settings; restore the prior Dokploy service, original Bort-managed Docker secret, and /var/lib/bort/dokploy-auth-secret.id provenance marker together from a consistent host backup, then rerun; Bort does not accept a manually recreated secret even when its key bytes match" >&2
        exit 1
    fi
	prepare_auth_secret_backup
elif [ "$DOKPLOY_SERVICE_EXISTS" = false ]; then
	validate_auth_secret_provenance
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
if ! python3 -c 'import ipaddress, sys; ipaddress.ip_address(sys.argv[1])' "$ADVERTISE_ADDR" 2>/dev/null; then
    echo "ADVERTISE_ADDR \"$ADVERTISE_ADDR\" is not a valid IP address; set ADVERTISE_ADDR and rerun" >&2
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
chown root:root /etc/dokploy /etc/dokploy/traefik /etc/dokploy/traefik/dynamic
chmod 755 /etc/dokploy /etc/dokploy/traefik /etc/dokploy/traefik/dynamic
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

if [ "$DOKPLOY_SERVICE_EXISTS" = false ]; then
    log "Creating Dokploy authentication secret"
    if ! docker secret inspect dokploy_auth_secret >/dev/null 2>&1; then
		validate_auth_secret_backup
		DOKPLOY_AUTH_SECRET_INTENT="$(write_auth_secret_intent)"
		if ! DOKPLOY_AUTH_SECRET_CREATED_ID="$(docker secret create --label "$AUTH_SECRET_INTENT_LABEL=$DOKPLOY_AUTH_SECRET_INTENT" dokploy_auth_secret - < "$AUTH_SECRET_FD_PATH")"; then
			echo "could not create Docker secret dokploy_auth_secret after recording Bort's creation intent; refusing to adopt any object that raced its creation" >&2
			exit 1
		fi
		if ! grep -Eq '^[a-z0-9]{20,64}$' <<<"$DOKPLOY_AUTH_SECRET_CREATED_ID"; then
			echo "Docker returned an invalid identity after creating secret dokploy_auth_secret; refusing to continue" >&2
			exit 1
		fi
		validate_auth_secret_provenance "$DOKPLOY_AUTH_SECRET_CREATED_ID"
	else
		validate_auth_secret_provenance
    fi
fi

log "Starting Dokploy Postgres"
if ! docker service inspect dokploy-postgres >/dev/null 2>&1; then
    docker service create \
        --name dokploy-postgres \
        --endpoint-mode "$ENDPOINT_MODE" \
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
        --endpoint-mode "$ENDPOINT_MODE" \
        --detach=true \
        --constraint 'node.role==manager' \
        --network dokploy-network \
		--mount type=volume,source=dokploy-redis,target=/data \
		redis:7 >/dev/null
fi

release_tag_args=()
release_tag_args=(-e "RELEASE_TAG=$VERSION_TAG")

log "Starting Dokploy UI/API on port ${HOST_PORT}"
DOKPLOY_STAGED_SERVICE=false
if ! docker service inspect dokploy >/dev/null 2>&1; then
    docker service create \
        --name dokploy \
        --label "$INSTALL_AUTH_SECRET_LABEL=$DOKPLOY_AUTH_SECRET_ID" \
        --endpoint-mode "$ENDPOINT_MODE" \
        --detach=true \
        --replicas 0 \
        --network dokploy-network \
		--mount type=bind,source=/var/run/docker.sock,target=/var/run/docker.sock \
        --mount type=bind,source=/etc/dokploy,target=/etc/dokploy \
        --mount type=volume,source=dokploy,target=/root/.docker \
        --secret source=dokploy_postgres_password,target=/run/secrets/postgres_password \
        --secret source=dokploy_auth_secret,target=/run/secrets/dokploy_auth_secret \
        --publish published="$HOST_PORT",target=3000,mode=host \
        --update-parallelism 1 \
        --update-order stop-first \
        --constraint 'node.role == manager' \
        "${release_tag_args[@]}" \
        -e ADVERTISE_ADDR="$ADVERTISE_ADDR" \
        -e POSTGRES_PASSWORD_FILE=/run/secrets/postgres_password \
        -e BETTER_AUTH_SECRET_FILE=/run/secrets/dokploy_auth_secret \
        "$REQUESTED_DOKPLOY_IMAGE" >/dev/null
	DOKPLOY_STAGED_SERVICE=true

elif ! DOKPLOY_SERVICE_REPLICAS="$(docker service inspect dokploy --format '{{.Spec.Mode.Replicated.Replicas}}' 2>/dev/null)"; then
	echo "could not inspect the existing Dokploy service replica count; refusing installation recovery" >&2
	exit 1
elif [ "$DOKPLOY_SERVICE_REPLICAS" != 0 ] && [ "$DOKPLOY_SERVICE_REPLICAS" != 1 ]; then
	echo "the existing Dokploy service has $DOKPLOY_SERVICE_REPLICAS replicas, not Bort's staged or running replica count; refusing installation recovery" >&2
	exit 1
else
	DOKPLOY_INSTALL_SERVICE_STATE=running
	if [ "$DOKPLOY_SERVICE_REPLICAS" = 0 ]; then
		DOKPLOY_INSTALL_SERVICE_STATE=zero-replica
	fi
	validate_bort_install_service "$DOKPLOY_INSTALL_SERVICE_STATE"
	if [ "$DOKPLOY_SERVICE_REPLICAS" = 0 ]; then
		if ! DOKPLOY_STAGED_RUNNING_TASKS="$(docker service ps --filter desired-state=running -q dokploy 2>/dev/null)"; then
			echo "could not inspect tasks for the zero-replica Dokploy service; refusing installation recovery" >&2
			exit 1
		fi
		if [ -n "$DOKPLOY_STAGED_RUNNING_TASKS" ]; then
			echo "the zero-replica Dokploy service still has a running task; refusing installation recovery" >&2
			exit 1
		fi
		DOKPLOY_STAGED_SERVICE=true
	fi
fi

if [ "$DOKPLOY_STAGED_SERVICE" = true ]; then
	if ! DOKPLOY_BOUND_AUTH_SECRET_IDS="$(docker service inspect dokploy --format '{{range .Spec.TaskTemplate.ContainerSpec.Secrets}}{{if eq .SecretName "dokploy_auth_secret"}}{{println .SecretID}}{{end}}{{end}}' 2>/dev/null)"; then
		echo "could not verify the authentication secret bound to the new Dokploy service; leaving it at zero replicas" >&2
		exit 1
	fi
	set -- $DOKPLOY_BOUND_AUTH_SECRET_IDS
	if [ "$#" -ne 1 ] || [ "$1" != "$DOKPLOY_AUTH_SECRET_ID" ]; then
		echo "the new zero-replica Dokploy service did not bind the reviewed authentication-secret identity; removing it and refusing to start a task" >&2
		docker service rm dokploy >/dev/null 2>&1 || true
		exit 1
	fi
	docker service scale dokploy=1 >/dev/null
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
