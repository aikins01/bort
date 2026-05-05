package dokploy

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
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
	baseURL := strings.TrimRight(strings.TrimSpace(os.Getenv(EnvBaseURL)), "/")
	token := strings.TrimSpace(os.Getenv(EnvToken))
	if baseURL == "" {
		return nil, fmt.Errorf("%s is required for live mode", EnvBaseURL)
	}
	if token == "" {
		return nil, fmt.Errorf("%s is required for live mode", EnvToken)
	}
	return &Client{
		BaseURL:    baseURL,
		Token:      token,
		HTTPClient: &http.Client{Timeout: defaultTimeout},
	}, nil
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

	resp, err := c.HTTPClient.Do(req)
	if err != nil {
		return fmt.Errorf("dokploy %s %s: %w", method, path, err)
	}
	defer resp.Body.Close()

	data, err := io.ReadAll(resp.Body)
	if err != nil {
		return fmt.Errorf("read %s response: %w", path, err)
	}
	if resp.StatusCode >= 400 {
		apiErr := &APIError{Status: resp.StatusCode}
		_ = json.Unmarshal(data, apiErr)
		if apiErr.Message == "" {
			apiErr.Message = strings.TrimSpace(string(data))
		}
		return apiErr
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

func (c *Client) GetCurrentUserOrg(ctx context.Context, sessionCookie string) (string, error) {
	headers := map[string]string{"Cookie": "better-auth.session_token=" + sessionCookie}
	resp, data, err := c.sessionRequest(ctx, http.MethodGet, "/api/trpc/user.get?batch=1", nil, headers)
	if err != nil {
		return "", err
	}
	if resp.StatusCode >= 400 {
		return "", &APIError{Status: resp.StatusCode, Message: strings.TrimSpace(string(data))}
	}
	var batch []struct {
		Result struct {
			Data struct {
				JSON struct {
					OrganizationID string `json:"organizationId"`
				} `json:"json"`
			} `json:"data"`
		} `json:"result"`
	}
	if err := json.Unmarshal(data, &batch); err != nil {
		return "", fmt.Errorf("decode user.get response: %w", err)
	}
	if len(batch) == 0 || batch[0].Result.Data.JSON.OrganizationID == "" {
		return "", fmt.Errorf("dokploy user.get response missing organizationId: %s", strings.TrimSpace(string(data)))
	}
	return batch[0].Result.Data.JSON.OrganizationID, nil
}

func (c *Client) CreateAPIKey(ctx context.Context, sessionCookie, keyName, organizationID string) (string, error) {
	headers := map[string]string{"Cookie": "better-auth.session_token=" + sessionCookie}
	// better-auth's api-key plugin defaults to 10 requests / 24h, which is
	// far too low for a migration tool. disable rate limiting at creation.
	body := map[string]any{
		"0": map[string]any{
			"json": map[string]any{
				"name":             keyName,
				"metadata":         map[string]string{"organizationId": organizationID},
				"rateLimitEnabled": false,
			},
		},
	}
	resp, data, err := c.sessionRequest(ctx, http.MethodPost, "/api/trpc/user.createApiKey?batch=1", body, headers)
	if err != nil {
		return "", err
	}
	if resp.StatusCode >= 400 {
		return "", &APIError{Status: resp.StatusCode, Message: strings.TrimSpace(string(data))}
	}
	var batch []struct {
		Result struct {
			Data struct {
				JSON struct {
					Key string `json:"key"`
				} `json:"json"`
			} `json:"data"`
		} `json:"result"`
	}
	if err := json.Unmarshal(data, &batch); err != nil {
		return "", fmt.Errorf("decode createApiKey response: %w", err)
	}
	if len(batch) == 0 || batch[0].Result.Data.JSON.Key == "" {
		return "", fmt.Errorf("dokploy createApiKey response missing key: %s", strings.TrimSpace(string(data)))
	}
	return batch[0].Result.Data.JSON.Key, nil
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
		return nil, err
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
	q := url.Values{}
	q.Set("name", name)
	q.Set("environmentId", environmentID)
	q.Set("limit", "20")
	var resp composeSearchResponse
	if err := c.do(ctx, http.MethodGet, "/api/compose.search", q, nil, &resp); err != nil {
		return nil, err
	}
	for i := range resp.Items {
		if resp.Items[i].Name == name {
			return &resp.Items[i], nil
		}
	}
	return nil, nil
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
		return nil, err
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
	return c.do(ctx, http.MethodPost, "/api/compose.update", nil, body, nil)
}

type deployComposeRequest struct {
	ComposeID string `json:"composeId"`
	Title     string `json:"title"`
}

func (c *Client) DeployCompose(ctx context.Context, composeID, title string) error {
	body := deployComposeRequest{ComposeID: composeID, Title: title}
	return c.do(ctx, http.MethodPost, "/api/compose.deploy", nil, body, nil)
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
		return nil, err
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
		return nil, err
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
