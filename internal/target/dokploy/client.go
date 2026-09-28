package dokploy

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"strings"
	"time"
)

const (
	EnvBaseURL = "BORT_DOKPLOY_URL"
	EnvToken   = "BORT_DOKPLOY_TOKEN"

	defaultTimeout = 30 * time.Second
)

var ErrNotImplemented = errors.New("dokploy live mode is not implemented")

type Client struct {
	BaseURL    string
	Token      string
	HTTPClient *http.Client
	DockerPath string
	Docker     dockerRunner
}

func NewClientFromEnv() (*Client, error) {
	baseURL := os.Getenv(EnvBaseURL)
	token := strings.TrimSpace(os.Getenv(EnvToken))
	if strings.TrimSpace(baseURL) == "" {
		return nil, fmt.Errorf("%s is required for live mode", EnvBaseURL)
	}
	if token == "" {
		return nil, fmt.Errorf("%s is required for live mode", EnvToken)
	}
	baseURL, err := NormalizeTokenBaseURL(baseURL)
	if err != nil {
		return nil, err
	}
	return &Client{
		BaseURL:    baseURL,
		Token:      token,
		HTTPClient: &http.Client{Timeout: defaultTimeout},
	}, nil
}

func NormalizeTokenBaseURL(raw string) (string, error) {
	parsed, err := url.Parse(strings.TrimSpace(raw))
	if err != nil || parsed.Scheme == "" || parsed.Host == "" {
		return "", fmt.Errorf("invalid Dokploy URL %q", raw)
	}
	if parsed.User != nil || parsed.RawQuery != "" || parsed.ForceQuery || parsed.Fragment != "" {
		return "", fmt.Errorf("Dokploy URL must not include user information, a query, or a fragment")
	}
	if parsed.RawPath != "" {
		return "", fmt.Errorf("Dokploy URL path must not use escaped separators")
	}
	scheme := strings.ToLower(parsed.Scheme)
	hostname := strings.ToLower(parsed.Hostname())
	if hostname == "" {
		return "", fmt.Errorf("invalid Dokploy URL %q", raw)
	}
	port := parsed.Port()
	if port != "" {
		portNumber, err := strconv.ParseUint(port, 10, 16)
		if err != nil || portNumber == 0 {
			return "", fmt.Errorf("invalid Dokploy URL port in %q", raw)
		}
		port = strconv.FormatUint(portNumber, 10)
	}
	if scheme == "https" && port == "443" {
		port = ""
	}
	host := hostname
	if port != "" {
		host = net.JoinHostPort(hostname, port)
	} else if strings.Contains(hostname, ":") {
		host = "[" + hostname + "]"
	}
	path := strings.TrimRight(parsed.Path, "/")
	normalized := (&url.URL{Scheme: scheme, Host: host, Path: path}).String()
	if scheme == "https" {
		return normalized, nil
	}
	if scheme != "http" {
		return "", fmt.Errorf("Dokploy URL must use https, or http on loopback")
	}
	if hostname == "localhost" {
		return normalized, nil
	}
	if ip := net.ParseIP(hostname); ip != nil && ip.IsLoopback() {
		return normalized, nil
	}
	return "", fmt.Errorf("refusing to send Dokploy API token to non-loopback http URL %q", raw)
}

type APIError struct {
	Status  int
	Code    string `json:"code"`
	Message string `json:"message"`
}

func (e *APIError) Error() string {
	if e.Code != "" {
		return fmt.Sprintf("dokploy %s (%d): %s", e.Code, e.Status, e.Message)
	}
	return fmt.Sprintf("dokploy http %d: %s", e.Status, e.Message)
}

type ambiguousMutationResponseError struct {
	err error
}

func (e ambiguousMutationResponseError) Error() string {
	return e.err.Error()
}

func (e ambiguousMutationResponseError) Unwrap() error {
	return e.err
}

func (e ambiguousMutationResponseError) mutationResponseAmbiguous() {}

func markMutationResponseAmbiguous(err error) error {
	if err == nil {
		return nil
	}
	var apiErr *APIError
	if errors.As(err, &apiErr) && apiErr.Status != http.StatusRequestTimeout && apiErr.Status < http.StatusInternalServerError {
		return err
	}
	return ambiguousMutationResponseError{err: err}
}

func mutationResponseMayHaveSucceeded(err error) bool {
	var ambiguous interface {
		error
		mutationResponseAmbiguous()
	}
	return errors.As(err, &ambiguous)
}

func (c *Client) do(ctx context.Context, method, path string, query url.Values, body any, out any) error {
	endpoint := c.BaseURL + path
	if len(query) > 0 {
		endpoint += "?" + query.Encode()
	}
	var reader io.Reader
	if body != nil {
		buf, err := json.Marshal(body)
		if err != nil {
			return fmt.Errorf("encode %s body: %w", path, err)
		}
		reader = bytes.NewReader(buf)
	}
	req, err := http.NewRequestWithContext(ctx, method, endpoint, reader)
	if err != nil {
		return err
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	req.Header.Set("Accept", "application/json")
	req.Header.Set("x-api-key", c.Token)

	configuredClient := c.httpClient()
	httpClient := &http.Client{
		Transport: configuredClient.Transport,
		CheckRedirect: func(*http.Request, []*http.Request) error {
			return http.ErrUseLastResponse
		},
		Jar:     configuredClient.Jar,
		Timeout: configuredClient.Timeout,
	}
	resp, err := httpClient.Do(req)
	if err != nil {
		return fmt.Errorf("dokploy %s %s: %w", method, path, err)
	}
	defer resp.Body.Close()

	data, readErr := io.ReadAll(resp.Body)
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		apiErr := &APIError{Status: resp.StatusCode}
		_ = json.Unmarshal(data, apiErr)
		if apiErr.Message == "" {
			apiErr.Message = strings.TrimSpace(string(data))
		}
		if readErr != nil {
			readFailure := fmt.Sprintf("read response: %v", readErr)
			if apiErr.Message == "" {
				apiErr.Message = readFailure
			} else {
				apiErr.Message += " (" + readFailure + ")"
			}
		}
		return apiErr
	}
	if readErr != nil && out != nil {
		return fmt.Errorf("read %s response: %w", path, readErr)
	}
	if out != nil && len(data) > 0 {
		if err := json.Unmarshal(data, out); err != nil {
			return fmt.Errorf("decode %s response: %w", path, err)
		}
	}
	return nil
}

func (c *Client) Ping(ctx context.Context) error {
	return c.do(ctx, http.MethodGet, "/api/project.all", nil, nil, nil)
}

func (c *Client) DokployVersion(ctx context.Context) (string, error) {
	var version string
	if err := c.do(ctx, http.MethodGet, "/api/settings.getDokployVersion", nil, nil, &version); err != nil {
		return "", err
	}
	return strings.TrimSpace(version), nil
}

type dokployServiceInspect struct {
	Spec struct {
		EndpointSpec struct {
			Mode  string `json:"Mode"`
			Ports []struct {
				Protocol      string `json:"Protocol"`
				TargetPort    uint32 `json:"TargetPort"`
				PublishedPort uint32 `json:"PublishedPort"`
				PublishMode   string `json:"PublishMode"`
			} `json:"Ports"`
		} `json:"EndpointSpec"`
		TaskTemplate struct {
			ContainerSpec struct {
				Env []string `json:"Env"`
			} `json:"ContainerSpec"`
			Networks []struct {
				Target string `json:"Target"`
			} `json:"Networks"`
		} `json:"TaskTemplate"`
	} `json:"Spec"`
}

type dokployTaskInspect struct {
	NodeID string `json:"NodeID"`
	Status struct {
		State           string `json:"State"`
		ContainerStatus struct {
			ContainerID string `json:"ContainerID"`
		} `json:"ContainerStatus"`
	} `json:"Status"`
}

func (c *Client) VerifySameDockerHost(ctx context.Context) error {
	_, _, err := c.verifySameDockerHost(ctx)
	return err
}

const localEndpointRebindHint = "rebind this workspace as the same OS user with `bort init-target dokploy --dokploy-url http://127.0.0.1:<port>` (or set both " + EnvBaseURL + " and " + EnvToken + "), using the host port the dokploy service publishes for container port 3000"

func (c *Client) verifySameDockerHost(ctx context.Context) (dokployServiceInspect, string, error) {
	verifyCtx, cancel := context.WithTimeout(ctx, dockerStopTimeout)
	defer cancel()
	parsed, err := url.Parse(c.BaseURL)
	if err != nil {
		return dokployServiceInspect{}, "", fmt.Errorf("parse Dokploy URL for local-host verification: %w", err)
	}
	hostname := parsed.Hostname()
	loopback := strings.EqualFold(hostname, "localhost")
	if ip := net.ParseIP(hostname); ip != nil && ip.IsLoopback() {
		loopback = true
	}
	if !loopback {
		return dokployServiceInspect{}, "", fmt.Errorf("Dokploy URL %q is not loopback; same-VPS migration requires a loopback Dokploy API URL: %s", c.BaseURL, localEndpointRebindHint)
	}
	if !strings.EqualFold(parsed.Scheme, "http") {
		return dokployServiceInspect{}, "", fmt.Errorf("Dokploy URL %q is not the direct local HTTP endpoint; same-VPS migration does not accept a loopback proxy or tunnel: %s", c.BaseURL, localEndpointRebindHint)
	}
	if parsed.Path != "" && parsed.Path != "/" || parsed.RawPath != "" || parsed.RawQuery != "" || parsed.ForceQuery || parsed.Fragment != "" {
		return dokployServiceInspect{}, "", fmt.Errorf("Dokploy URL %q must be the root of the direct local HTTP endpoint", c.BaseURL)
	}
	port, err := strconv.ParseUint(parsed.Port(), 10, 16)
	if err != nil || port == 0 {
		return dokployServiceInspect{}, "", fmt.Errorf("Dokploy URL %q must include the local Dokploy service's published port", c.BaseURL)
	}
	runner := c.dockerRunner()
	service, err := inspectDokployService(verifyCtx, runner, "dokploy")
	if err != nil {
		return dokployServiceInspect{}, "", fmt.Errorf("verify local Dokploy service before API access: %w", err)
	}
	mode := strings.ToLower(service.Spec.EndpointSpec.Mode)
	if mode != "vip" && mode != "dnsrr" {
		return dokployServiceInspect{}, "", fmt.Errorf("local Dokploy service endpoint mode is %q, want vip or dnsrr", service.Spec.EndpointSpec.Mode)
	}
	portPublished := false
	for _, published := range service.Spec.EndpointSpec.Ports {
		if published.PublishedPort == uint32(port) && published.TargetPort == 3000 && strings.EqualFold(published.Protocol, "tcp") && strings.EqualFold(published.PublishMode, "host") {
			portPublished = true
			break
		}
	}
	if !portPublished {
		return dokployServiceInspect{}, "", fmt.Errorf("Dokploy URL %q does not match a host-mode TCP port published by the local Dokploy service to target port 3000", c.BaseURL)
	}
	nodeID, err := localDockerNodeID(verifyCtx, runner)
	if err != nil {
		return dokployServiceInspect{}, "", err
	}
	if err := verifyLocalDokployTask(verifyCtx, runner, nodeID); err != nil {
		return dokployServiceInspect{}, "", err
	}
	return service, nodeID, nil
}

func (c *Client) VerifySameDockerHostAndDatabase(ctx context.Context) error {
	_, err := c.verifySameDockerHostAndDatabase(ctx)
	return err
}

func (c *Client) verifySameDockerHostAndDatabase(ctx context.Context) (string, error) {
	verifyCtx, cancel := context.WithTimeout(ctx, dockerStopTimeout)
	defer cancel()
	app, nodeID, err := c.verifySameDockerHost(verifyCtx)
	if err != nil {
		return "", err
	}
	return c.verifyLocalDokployDatabaseWith(verifyCtx, app, nodeID)
}

func (c *Client) verifyLocalDokployDatabaseWith(ctx context.Context, app dokployServiceInspect, nodeID string) (string, error) {
	runner := c.dockerRunner()
	if err := verifyDefaultDokployDatabaseEnvironment(app.Spec.TaskTemplate.ContainerSpec.Env); err != nil {
		return "", err
	}
	postgres, err := inspectDokployService(ctx, runner, "dokploy-postgres")
	if err != nil {
		return "", fmt.Errorf("inspect local Dokploy postgres service: %w", err)
	}
	networks := map[string]struct{}{}
	for _, network := range app.Spec.TaskTemplate.Networks {
		if target := strings.TrimSpace(network.Target); target != "" {
			networks[target] = struct{}{}
		}
	}
	for _, network := range postgres.Spec.TaskTemplate.Networks {
		if _, ok := networks[strings.TrimSpace(network.Target)]; ok {
			containerID, err := localRunningServiceContainerIDOnNode(ctx, runner, "dokploy-postgres", nodeID)
			if err != nil {
				return "", fmt.Errorf("resolve local Dokploy postgres container: %w", err)
			}
			return containerID, nil
		}
	}
	return "", fmt.Errorf("local Dokploy and dokploy-postgres services do not share a Docker network")
}

func verifyDefaultDokployDatabaseEnvironment(env []string) error {
	defaults := map[string]string{
		"POSTGRES_HOST": "dokploy-postgres",
		"POSTGRES_PORT": "5432",
		"POSTGRES_USER": "dokploy",
		"POSTGRES_DB":   "dokploy",
	}
	seen := map[string]bool{}
	for _, entry := range env {
		name, value, ok := strings.Cut(entry, "=")
		if !ok {
			continue
		}
		if name == "DATABASE_URL" {
			return fmt.Errorf("Dokploy service configures DATABASE_URL; direct cleanup of the default internal dokploy-postgres database is refused")
		}
		expected, selected := defaults[name]
		if !selected {
			continue
		}
		if seen[name] {
			return fmt.Errorf("Dokploy service has duplicate %s settings; direct database cleanup is refused", name)
		}
		seen[name] = true
		if value != expected {
			return fmt.Errorf("Dokploy service configures %s=%s, not the supported default %s; direct cleanup of the default internal dokploy-postgres database is refused", name, value, expected)
		}
	}
	return nil
}

func localDockerNodeID(ctx context.Context, runner dockerRunner) (string, error) {
	nodeOut, err := runner.Output(ctx, "node", "inspect", "self", "--format", "{{.ID}}")
	if err != nil {
		return "", fmt.Errorf("identify local Docker node: %w", err)
	}
	nodeID := strings.TrimSpace(string(nodeOut))
	if nodeID == "" {
		return "", fmt.Errorf("local Docker node has no ID")
	}
	return nodeID, nil
}

func localRunningServiceContainerIDOnNode(ctx context.Context, runner dockerRunner, service, nodeID string) (string, error) {
	taskOut, err := runner.Output(ctx, "service", "ps", "--filter", "desired-state=running", "-q", service)
	if err != nil {
		return "", fmt.Errorf("list running %s service tasks: %w", service, err)
	}
	taskIDs := strings.Fields(string(taskOut))
	if len(taskIDs) == 0 {
		return "", fmt.Errorf("%s service has no running tasks", service)
	}
	args := append([]string{"inspect", "--type", "task"}, taskIDs...)
	inspected, err := runner.Output(ctx, args...)
	if err != nil {
		return "", fmt.Errorf("inspect running %s service tasks: %w", service, err)
	}
	var tasks []dokployTaskInspect
	if err := json.Unmarshal(inspected, &tasks); err != nil {
		return "", fmt.Errorf("decode running %s service tasks: %w", service, err)
	}
	containerIDs := []string{}
	for _, task := range tasks {
		if task.NodeID != nodeID || !strings.EqualFold(task.Status.State, "running") {
			continue
		}
		containerID := strings.TrimSpace(task.Status.ContainerStatus.ContainerID)
		if containerID == "" {
			return "", fmt.Errorf("running local %s service task has no container ID", service)
		}
		containerIDs = append(containerIDs, containerID)
	}
	if len(containerIDs) != 1 {
		return "", fmt.Errorf("local Docker node %s has %d running %s service tasks, want exactly 1", nodeID, len(containerIDs), service)
	}
	return containerIDs[0], nil
}

func verifyLocalDokployTask(ctx context.Context, runner dockerRunner, nodeID string) error {
	taskOut, err := runner.Output(ctx, "service", "ps", "--filter", "desired-state=running", "-q", "dokploy")
	if err != nil {
		return fmt.Errorf("list running local Dokploy service tasks: %w", err)
	}
	taskIDs := strings.Fields(string(taskOut))
	if len(taskIDs) == 0 {
		return fmt.Errorf("local Dokploy service has no running tasks")
	}
	args := append([]string{"inspect", "--type", "task"}, taskIDs...)
	inspected, err := runner.Output(ctx, args...)
	if err != nil {
		return fmt.Errorf("inspect running local Dokploy service tasks: %w", err)
	}
	var tasks []dokployTaskInspect
	if err := json.Unmarshal(inspected, &tasks); err != nil {
		return fmt.Errorf("decode running local Dokploy service tasks: %w", err)
	}
	for _, task := range tasks {
		if task.NodeID == nodeID && strings.EqualFold(task.Status.State, "running") {
			return nil
		}
	}
	return fmt.Errorf("local Docker node %s has no running Dokploy service task", nodeID)
}

func inspectDokployService(ctx context.Context, runner dockerRunner, name string) (dokployServiceInspect, error) {
	out, err := runner.Output(ctx, "service", "inspect", name)
	if err != nil {
		return dokployServiceInspect{}, fmt.Errorf("docker service inspect %s: %w", name, err)
	}
	var services []dokployServiceInspect
	if err := json.Unmarshal(out, &services); err != nil {
		return dokployServiceInspect{}, fmt.Errorf("decode docker service inspect %s: %w", name, err)
	}
	if len(services) != 1 {
		return dokployServiceInspect{}, fmt.Errorf("docker service inspect %s returned %d services, want 1", name, len(services))
	}
	return services[0], nil
}

func (c *Client) httpClient() *http.Client {
	if c.HTTPClient != nil {
		return c.HTTPClient
	}
	return &http.Client{Timeout: defaultTimeout}
}

func (c *Client) sessionRequest(ctx context.Context, method, path string, body any, headers map[string]string) (*http.Response, []byte, error) {
	endpoint := strings.TrimRight(c.BaseURL, "/") + path
	var reader io.Reader
	if body != nil {
		buf, err := json.Marshal(body)
		if err != nil {
			return nil, nil, fmt.Errorf("encode %s body: %w", path, err)
		}
		reader = bytes.NewReader(buf)
	}
	req, err := http.NewRequestWithContext(ctx, method, endpoint, reader)
	if err != nil {
		return nil, nil, err
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	req.Header.Set("Accept", "application/json")
	for key, value := range headers {
		req.Header.Set(key, value)
	}
	resp, err := c.httpClient().Do(req)
	if err != nil {
		return nil, nil, fmt.Errorf("dokploy %s %s: %w", method, path, err)
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(resp.Body)
	if err != nil {
		return resp, nil, fmt.Errorf("read %s response: %w", path, err)
	}
	return resp, data, nil
}

func (c *Client) SignUpAdmin(ctx context.Context, name, email, password string) error {
	body := map[string]string{"name": name, "email": email, "password": password}
	resp, data, err := c.sessionRequest(ctx, http.MethodPost, "/api/auth/sign-up/email", body, nil)
	if err != nil {
		return err
	}
	if resp.StatusCode < 400 {
		return nil
	}
	if isUserExistsResponse(resp.StatusCode, data) {
		return nil
	}
	return &APIError{Status: resp.StatusCode, Message: strings.TrimSpace(string(data))}
}

func isUserExistsResponse(status int, data []byte) bool {
	if status != http.StatusUnprocessableEntity && status != http.StatusBadRequest && status != http.StatusConflict {
		return false
	}
	body := strings.ToLower(string(data))
	return strings.Contains(body, "already") || strings.Contains(body, "exist") || strings.Contains(body, "duplicate") || strings.Contains(body, "taken")
}

func (c *Client) SignIn(ctx context.Context, email, password string) (string, error) {
	body := map[string]string{"email": email, "password": password}
	resp, data, err := c.sessionRequest(ctx, http.MethodPost, "/api/auth/sign-in/email", body, nil)
	if err != nil {
		return "", err
	}
	if resp.StatusCode >= 400 {
		return "", &APIError{Status: resp.StatusCode, Message: strings.TrimSpace(string(data))}
	}
	for _, cookie := range resp.Cookies() {
		if cookie.Name == "better-auth.session_token" {
			return cookie.Value, nil
		}
	}
	return "", fmt.Errorf("dokploy sign-in did not return better-auth.session_token cookie")
}

type APIKey struct {
	ID     string `json:"id"`
	Name   string `json:"name"`
	Prefix string `json:"prefix"`
}

type currentUserData struct {
	OrganizationID string `json:"organizationId"`
	User           *struct {
		APIKeys []APIKey `json:"apiKeys"`
	} `json:"user"`
}

func (c *Client) getCurrentUser(ctx context.Context, sessionCookie string) (currentUserData, []byte, error) {
	headers := map[string]string{"Cookie": "better-auth.session_token=" + sessionCookie}
	resp, data, err := c.sessionRequest(ctx, http.MethodGet, "/api/trpc/user.get?batch=1", nil, headers)
	if err != nil {
		return currentUserData{}, nil, err
	}
	if resp.StatusCode >= 400 {
		return currentUserData{}, nil, &APIError{Status: resp.StatusCode, Message: strings.TrimSpace(string(data))}
	}
	var batch []struct {
		Result struct {
			Data struct {
				JSON currentUserData `json:"json"`
			} `json:"data"`
		} `json:"result"`
	}
	if err := json.Unmarshal(data, &batch); err != nil {
		return currentUserData{}, nil, fmt.Errorf("decode user.get response: %w", err)
	}
	if len(batch) == 0 {
		return currentUserData{}, data, fmt.Errorf("dokploy user.get response missing result data: %s", strings.TrimSpace(string(data)))
	}
	return batch[0].Result.Data.JSON, data, nil
}

func (c *Client) GetCurrentUserOrg(ctx context.Context, sessionCookie string) (string, error) {
	user, data, err := c.getCurrentUser(ctx, sessionCookie)
	if err != nil {
		return "", err
	}
	if user.OrganizationID == "" {
		return "", fmt.Errorf("dokploy user.get response missing organizationId: %s", strings.TrimSpace(string(data)))
	}
	return user.OrganizationID, nil
}

func (c *Client) GetCurrentUserOrgAndAPIKeys(ctx context.Context, sessionCookie string) (string, []APIKey, error) {
	user, data, err := c.getCurrentUser(ctx, sessionCookie)
	if err != nil {
		return "", nil, err
	}
	if user.OrganizationID == "" {
		return "", nil, fmt.Errorf("dokploy user.get response missing organizationId: %s", strings.TrimSpace(string(data)))
	}
	if user.User == nil {
		return "", nil, fmt.Errorf("dokploy user.get response missing user data: %s", strings.TrimSpace(string(data)))
	}
	return user.OrganizationID, user.User.APIKeys, nil
}

func (c *Client) DeleteAPIKey(ctx context.Context, sessionCookie, apiKeyID string) error {
	headers := map[string]string{"Cookie": "better-auth.session_token=" + sessionCookie}
	body := map[string]any{
		"0": map[string]any{
			"json": map[string]string{"apiKeyId": apiKeyID},
		},
	}
	resp, data, err := c.sessionRequest(ctx, http.MethodPost, "/api/trpc/user.deleteApiKey?batch=1", body, headers)
	if err != nil {
		return err
	}
	if resp.StatusCode >= 400 {
		return &APIError{Status: resp.StatusCode, Message: strings.TrimSpace(string(data))}
	}
	return nil
}

func (c *Client) CreateAPIKey(ctx context.Context, sessionCookie, keyName, prefix, organizationID string) (string, string, error) {
	headers := map[string]string{"Cookie": "better-auth.session_token=" + sessionCookie}
	// better-auth's api-key plugin defaults to 10 requests / 24h, which is
	// far too low for a migration tool. disable rate limiting at creation.
	input := map[string]any{
		"name":             keyName,
		"metadata":         map[string]string{"organizationId": organizationID},
		"rateLimitEnabled": false,
	}
	if prefix != "" {
		input["prefix"] = prefix
	}
	body := map[string]any{
		"0": map[string]any{
			"json": input,
		},
	}
	resp, data, err := c.sessionRequest(ctx, http.MethodPost, "/api/trpc/user.createApiKey?batch=1", body, headers)
	if err != nil {
		return "", "", err
	}
	if resp.StatusCode >= 400 {
		return "", "", &APIError{Status: resp.StatusCode, Message: strings.TrimSpace(string(data))}
	}
	var batch []struct {
		Result struct {
			Data struct {
				JSON struct {
					ID  string `json:"id"`
					Key string `json:"key"`
				} `json:"json"`
			} `json:"data"`
		} `json:"result"`
	}
	if err := json.Unmarshal(data, &batch); err != nil {
		return "", "", fmt.Errorf("decode createApiKey response: %w", err)
	}
	if len(batch) == 0 || batch[0].Result.Data.JSON.ID == "" || batch[0].Result.Data.JSON.Key == "" {
		return "", "", fmt.Errorf("dokploy createApiKey response missing id or key: %s", strings.TrimSpace(string(data)))
	}
	return batch[0].Result.Data.JSON.ID, batch[0].Result.Data.JSON.Key, nil
}

type Project struct {
	ProjectID    string                `json:"projectId"`
	Name         string                `json:"name"`
	Description  string                `json:"description,omitempty"`
	Environments []ProjectEnvironment  `json:"environments,omitempty"`
	Compose      []ProjectComposeChild `json:"compose,omitempty"`
}

type ProjectEnvironment struct {
	EnvironmentID string `json:"environmentId"`
	Name          string `json:"name"`
}

type ProjectComposeChild struct {
	ComposeID string `json:"composeId"`
	Name      string `json:"name"`
	AppName   string `json:"appName"`
}

func (c *Client) ListProjects(ctx context.Context) ([]Project, error) {
	var projects []Project
	if err := c.do(ctx, http.MethodGet, "/api/project.all", nil, nil, &projects); err != nil {
		return nil, err
	}
	return projects, nil
}

func (c *Client) FindProjectByName(ctx context.Context, name string) (*Project, error) {
	projects, err := c.ListProjects(ctx)
	if err != nil {
		return nil, err
	}
	for i := range projects {
		if projects[i].Name == name {
			return &projects[i], nil
		}
	}
	return nil, nil
}

type createProjectRequest struct {
	Name        string `json:"name"`
	Description string `json:"description,omitempty"`
}

func (c *Client) CreateProject(ctx context.Context, name, description string) (*Project, error) {
	if existing, err := c.FindProjectByName(ctx, name); err != nil {
		return nil, err
	} else if existing != nil {
		return existing, nil
	}
	var project Project
	if err := c.do(ctx, http.MethodPost, "/api/project.create", nil, createProjectRequest{Name: name, Description: description}, &project); err != nil {
		return nil, markMutationResponseAmbiguous(err)
	}
	if project.ProjectID == "" {
		fresh, err := c.FindProjectByName(ctx, name)
		if err != nil {
			return nil, err
		}
		if fresh == nil {
			return nil, fmt.Errorf("dokploy created project %q but it was not visible in project.all", name)
		}
		return fresh, nil
	}
	return &project, nil
}

func (c *Client) GetProject(ctx context.Context, projectID string) (*Project, error) {
	q := url.Values{}
	q.Set("projectId", projectID)
	var project Project
	if err := c.do(ctx, http.MethodGet, "/api/project.one", q, nil, &project); err != nil {
		return nil, err
	}
	return &project, nil
}

func FindEnvironmentInProject(project *Project, name string) *ProjectEnvironment {
	if project == nil || len(project.Environments) == 0 {
		return nil
	}
	for i := range project.Environments {
		if strings.EqualFold(project.Environments[i].Name, name) {
			return &project.Environments[i]
		}
	}
	return &project.Environments[0]
}

type Compose struct {
	ComposeID     string       `json:"composeId"`
	Name          string       `json:"name"`
	AppName       string       `json:"appName,omitempty"`
	EnvironmentID string       `json:"environmentId,omitempty"`
	ComposeStatus string       `json:"composeStatus,omitempty"`
	Command       string       `json:"command,omitempty"`
	CreateEnvFile *bool        `json:"createEnvFile,omitempty"`
	Deployments   []Deployment `json:"deployments,omitempty"`
}

type Deployment struct {
	Status       string `json:"status,omitempty"`
	Title        string `json:"title,omitempty"`
	LogPath      string `json:"logPath,omitempty"`
	ErrorMessage string `json:"errorMessage,omitempty"`
}

type composeSearchResponse struct {
	Items []Compose `json:"items"`
	Total int       `json:"total"`
}

func (c *Client) SearchCompose(ctx context.Context, name, environmentID string) (*Compose, error) {
	timeout := c.httpClient().Timeout
	if timeout <= 0 {
		timeout = defaultTimeout
	}
	searchCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	var match *Compose
	for offset := 0; ; {
		q := url.Values{}
		q.Set("name", name)
		q.Set("environmentId", environmentID)
		q.Set("limit", "100")
		q.Set("offset", strconv.Itoa(offset))
		var resp composeSearchResponse
		if err := c.do(searchCtx, http.MethodGet, "/api/compose.search", q, nil, &resp); err != nil {
			return nil, err
		}
		for i := range resp.Items {
			if resp.Items[i].Name == name {
				if match != nil {
					return nil, fmt.Errorf("multiple Dokploy compose apps named %q exist in environment %s", name, environmentID)
				}
				item := resp.Items[i]
				match = &item
			}
		}
		offset += len(resp.Items)
		if offset >= resp.Total {
			return match, nil
		}
		if len(resp.Items) == 0 {
			return nil, fmt.Errorf("Dokploy compose search for %q returned no candidates at offset %d of %d", name, offset, resp.Total)
		}
	}
}

func (c *Client) GetCompose(ctx context.Context, composeID string) (*Compose, error) {
	q := url.Values{}
	q.Set("composeId", composeID)
	var compose Compose
	if err := c.do(ctx, http.MethodGet, "/api/compose.one", q, nil, &compose); err != nil {
		return nil, err
	}
	return &compose, nil
}

type CreateComposeRequest struct {
	Name          string `json:"name"`
	EnvironmentID string `json:"environmentId"`
	ComposeFile   string `json:"composeFile"`
	ComposeType   string `json:"composeType"`
	SourceType    string `json:"sourceType"`
}

func (c *Client) CreateCompose(ctx context.Context, req CreateComposeRequest) (*Compose, error) {
	if existing, err := c.SearchCompose(ctx, req.Name, req.EnvironmentID); err != nil {
		return nil, err
	} else if existing != nil {
		return existing, nil
	}
	if req.ComposeType == "" {
		req.ComposeType = "docker-compose"
	}
	if req.SourceType == "" {
		req.SourceType = "raw"
	}
	var compose Compose
	if err := c.do(ctx, http.MethodPost, "/api/compose.create", nil, req, &compose); err != nil {
		return nil, markMutationResponseAmbiguous(err)
	}
	if compose.ComposeID == "" {
		fresh, err := c.SearchCompose(ctx, req.Name, req.EnvironmentID)
		if err != nil {
			return nil, err
		}
		if fresh == nil {
			return nil, fmt.Errorf("dokploy created compose %q but it was not visible in compose.search", req.Name)
		}
		return fresh, nil
	}
	return &compose, nil
}

type updateComposeRequest struct {
	ComposeID   string `json:"composeId"`
	ComposeFile string `json:"composeFile"`
	Env         string `json:"env"`
	SourceType  string `json:"sourceType"`
}

func (c *Client) UpdateCompose(ctx context.Context, composeID, composeFile, envContent string) error {
	body := updateComposeRequest{
		ComposeID:   composeID,
		ComposeFile: composeFile,
		Env:         envContent,
		SourceType:  "raw",
	}
	return markMutationResponseAmbiguous(c.do(ctx, http.MethodPost, "/api/compose.update", nil, body, nil))
}

type deployComposeRequest struct {
	ComposeID string `json:"composeId"`
	Title     string `json:"title"`
}

func (c *Client) DeployCompose(ctx context.Context, composeID, title string) error {
	body := deployComposeRequest{ComposeID: composeID, Title: title}
	return markMutationResponseAmbiguous(c.do(ctx, http.MethodPost, "/api/compose.deploy", nil, body, nil))
}

type Domain struct {
	DomainID        string `json:"domainId,omitempty"`
	Host            string `json:"host"`
	DomainType      string `json:"domainType,omitempty"`
	ComposeID       string `json:"composeId,omitempty"`
	ServiceName     string `json:"serviceName,omitempty"`
	Port            int    `json:"port,omitempty"`
	HTTPS           bool   `json:"https"`
	CertificateType string `json:"certificateType,omitempty"`
	Path            string `json:"path,omitempty"`
	InternalPath    string `json:"internalPath,omitempty"`
}

func (c *Client) ListDomainsByCompose(ctx context.Context, composeID string) ([]Domain, error) {
	q := url.Values{}
	q.Set("composeId", composeID)
	var domains []Domain
	if err := c.do(ctx, http.MethodGet, "/api/domain.byComposeId", q, nil, &domains); err != nil {
		return nil, err
	}
	return domains, nil
}

func (c *Client) FindDomainByHost(ctx context.Context, composeID, host string) (*Domain, error) {
	domains, err := c.ListDomainsByCompose(ctx, composeID)
	if err != nil {
		return nil, err
	}
	for i := range domains {
		if strings.EqualFold(domains[i].Host, host) {
			return &domains[i], nil
		}
	}
	return nil, nil
}

type CreateDomainRequest struct {
	Host            string `json:"host"`
	DomainType      string `json:"domainType"`
	ComposeID       string `json:"composeId"`
	ServiceName     string `json:"serviceName,omitempty"`
	Port            int    `json:"port,omitempty"`
	HTTPS           bool   `json:"https"`
	CertificateType string `json:"certificateType"`
	Path            string `json:"path"`
	InternalPath    string `json:"internalPath"`
}

type UpdateDomainRequest struct {
	DomainID        string `json:"domainId"`
	Host            string `json:"host"`
	DomainType      string `json:"domainType,omitempty"`
	ServiceName     string `json:"serviceName,omitempty"`
	Port            int    `json:"port,omitempty"`
	HTTPS           bool   `json:"https"`
	CertificateType string `json:"certificateType,omitempty"`
	Path            string `json:"path,omitempty"`
	InternalPath    string `json:"internalPath,omitempty"`
}

func (c *Client) CreateDomain(ctx context.Context, req CreateDomainRequest) (*Domain, error) {
	if req.DomainType == "" {
		req.DomainType = "compose"
	}
	if req.CertificateType == "" {
		req.CertificateType = "none"
	}
	if req.Path == "" {
		req.Path = "/"
	}
	if req.InternalPath == "" {
		req.InternalPath = "/"
	}
	if existing, err := c.FindDomainByHost(ctx, req.ComposeID, req.Host); err != nil {
		return nil, err
	} else if existing != nil {
		if domainNeedsUpdate(*existing, req) {
			return c.UpdateDomain(ctx, UpdateDomainRequest{
				DomainID:        existing.DomainID,
				Host:            req.Host,
				DomainType:      req.DomainType,
				ServiceName:     req.ServiceName,
				Port:            req.Port,
				HTTPS:           req.HTTPS,
				CertificateType: req.CertificateType,
				Path:            req.Path,
				InternalPath:    req.InternalPath,
			})
		}
		return existing, nil
	}
	var domain Domain
	if err := c.do(ctx, http.MethodPost, "/api/domain.create", nil, req, &domain); err != nil {
		return nil, markMutationResponseAmbiguous(err)
	}
	return &domain, nil
}

func (c *Client) UpdateDomain(ctx context.Context, req UpdateDomainRequest) (*Domain, error) {
	if strings.TrimSpace(req.DomainID) == "" {
		return nil, fmt.Errorf("domainId is required to update dokploy domain %s", req.Host)
	}
	if strings.TrimSpace(req.Host) == "" {
		return nil, fmt.Errorf("host is required to update dokploy domain %s", req.DomainID)
	}
	var domain Domain
	if err := c.do(ctx, http.MethodPost, "/api/domain.update", nil, req, &domain); err != nil {
		return nil, markMutationResponseAmbiguous(err)
	}
	if domain.DomainID == "" {
		domain = Domain{
			DomainID:        req.DomainID,
			Host:            req.Host,
			DomainType:      req.DomainType,
			ServiceName:     req.ServiceName,
			Port:            req.Port,
			HTTPS:           req.HTTPS,
			CertificateType: req.CertificateType,
			Path:            req.Path,
			InternalPath:    req.InternalPath,
		}
	}
	return &domain, nil
}

func domainNeedsUpdate(existing Domain, req CreateDomainRequest) bool {
	if req.DomainType != "" && existing.DomainType != "" && !strings.EqualFold(existing.DomainType, req.DomainType) {
		return true
	}
	if req.ServiceName != "" && existing.ServiceName != req.ServiceName {
		return true
	}
	if req.Port > 0 && existing.Port != req.Port {
		return true
	}
	if existing.HTTPS != req.HTTPS {
		return true
	}
	if normalizeCertificateType(existing.CertificateType) != normalizeCertificateType(req.CertificateType) {
		return true
	}
	if req.Path != "" && normalizeDomainPath(existing.Path) != normalizeDomainPath(req.Path) {
		return true
	}
	if req.InternalPath != "" && normalizeDomainPath(existing.InternalPath) != normalizeDomainPath(req.InternalPath) {
		return true
	}
	return false
}

func normalizeDomainPath(path string) string {
	path = strings.TrimSpace(path)
	if path == "" {
		return "/"
	}
	return path
}

func normalizeCertificateType(value string) string {
	value = strings.TrimSpace(value)
	if value == "" {
		return "none"
	}
	return value
}
