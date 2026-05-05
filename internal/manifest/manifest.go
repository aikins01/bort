package manifest

import "time"

const APIVersion = "bort.io/v1alpha1"

type Manifest struct {
	APIVersion     string          `json:"apiVersion"`
	Kind           string          `json:"kind"`
	GeneratedAt    time.Time       `json:"generatedAt"`
	Source         Source          `json:"source"`
	Apps           []App           `json:"apps"`
	Volumes        []Volume        `json:"volumes,omitempty"`
	Networks       []Network       `json:"networks,omitempty"`
	ProxyArtifacts []ProxyArtifact `json:"proxyArtifacts,omitempty"`
	Warnings       []Warning       `json:"warnings,omitempty"`
}

type ProxyArtifact struct {
	Source  string `json:"source"`
	Path    string `json:"path"`
	Content string `json:"content,omitempty"`
}

type Source struct {
	Platform string `json:"platform"`
	Hostname string `json:"hostname,omitempty"`
}

type App struct {
	ID          string            `json:"id"`
	Name        string            `json:"name"`
	Platform    string            `json:"platform,omitempty"`
	Runtime     string            `json:"runtime,omitempty"`
	BuildPack   string            `json:"buildPack,omitempty"`
	Status      string            `json:"status,omitempty"`
	Git         *GitSource        `json:"git,omitempty"`
	Compose     *ComposeSource    `json:"compose,omitempty"`
	Environment []EnvVar          `json:"environment,omitempty"`
	Storages    []Storage         `json:"storages,omitempty"`
	Metadata    map[string]string `json:"metadata,omitempty"`
	Labels      map[string]string `json:"labels,omitempty"`
	Services    []Service         `json:"services"`
	Routes      []Route           `json:"routes,omitempty"`
	Warnings    []Warning         `json:"warnings,omitempty"`
}

type GitSource struct {
	Repository         string `json:"repository,omitempty"`
	Branch             string `json:"branch,omitempty"`
	CommitSHA          string `json:"commitSha,omitempty"`
	BaseDirectory      string `json:"baseDirectory,omitempty"`
	DockerfileLocation string `json:"dockerfileLocation,omitempty"`
	ComposeLocation    string `json:"composeLocation,omitempty"`
	Provider           string `json:"provider,omitempty"`
	SourceType         string `json:"sourceType,omitempty"`
	SourceID           string `json:"sourceId,omitempty"`
	PrivateKeyID       string `json:"privateKeyId,omitempty"`
	RepositoryID       string `json:"repositoryId,omitempty"`
}

type ComposeSource struct {
	Raw      string                   `json:"raw,omitempty"`
	Resolved string                   `json:"resolved,omitempty"`
	Domains  map[string]ComposeDomain `json:"domains,omitempty"`
}

type ComposeDomain struct {
	Domain string `json:"domain,omitempty"`
}

type Service struct {
	ID          string            `json:"id"`
	Name        string            `json:"name"`
	Image       string            `json:"image,omitempty"`
	ImageID     string            `json:"imageID,omitempty"`
	ImageDigest string            `json:"imageDigest,omitempty"`
	Status      string            `json:"status,omitempty"`
	Healthcheck *Healthcheck      `json:"healthcheck,omitempty"`
	Environment []EnvVar          `json:"environment,omitempty"`
	Mounts      []Mount           `json:"mounts,omitempty"`
	Ports       []Port            `json:"ports,omitempty"`
	Networks    []ServiceNetwork  `json:"networks,omitempty"`
	Labels      map[string]string `json:"labels,omitempty"`
}

type Healthcheck struct {
	Test        []string `json:"test,omitempty"`
	Interval    string   `json:"interval,omitempty"`
	Timeout     string   `json:"timeout,omitempty"`
	Retries     int      `json:"retries,omitempty"`
	StartPeriod string   `json:"startPeriod,omitempty"`
}

type EnvVar struct {
	Name       string `json:"name"`
	Value      string `json:"value,omitempty"`
	ValueKnown bool   `json:"valueKnown"`
	Sensitive  bool   `json:"sensitive,omitempty"`
}

type Mount struct {
	Type   string `json:"type"`
	Name   string `json:"name,omitempty"`
	Source string `json:"source,omitempty"`
	Target string `json:"target"`
	RW     bool   `json:"rw"`
}

type Storage struct {
	ID        string            `json:"id,omitempty"`
	Name      string            `json:"name,omitempty"`
	Type      string            `json:"type,omitempty"`
	Source    string            `json:"source,omitempty"`
	Target    string            `json:"target,omitempty"`
	Content   string            `json:"content,omitempty"`
	Directory bool              `json:"directory,omitempty"`
	Metadata  map[string]string `json:"metadata,omitempty"`
}

type Port struct {
	ContainerPort string `json:"containerPort"`
	HostIP        string `json:"hostIP,omitempty"`
	HostPort      string `json:"hostPort,omitempty"`
}

type ServiceNetwork struct {
	Name      string `json:"name"`
	NetworkID string `json:"networkID,omitempty"`
	IPAddress string `json:"ipAddress,omitempty"`
}

type Route struct {
	Host        string `json:"host"`
	ServiceName string `json:"serviceName,omitempty"`
	Port        string `json:"port,omitempty"`
	Source      string `json:"source,omitempty"`
}

type Volume struct {
	Name       string            `json:"name"`
	Driver     string            `json:"driver,omitempty"`
	Mountpoint string            `json:"mountpoint,omitempty"`
	Scope      string            `json:"scope,omitempty"`
	SizeBytes  int64             `json:"sizeBytes,omitempty"`
	FileCount  int64             `json:"fileCount,omitempty"`
	Labels     map[string]string `json:"labels,omitempty"`
	Options    map[string]string `json:"options,omitempty"`
	UsedBy     []string          `json:"usedBy,omitempty"`
}

type Network struct {
	ID       string            `json:"id"`
	Name     string            `json:"name"`
	Driver   string            `json:"driver,omitempty"`
	Scope    string            `json:"scope,omitempty"`
	Internal bool              `json:"internal,omitempty"`
	Labels   map[string]string `json:"labels,omitempty"`
}

type Warning struct {
	Code    string `json:"code"`
	Message string `json:"message"`
}

func New(source Source, generatedAt time.Time) Manifest {
	return Manifest{
		APIVersion:  APIVersion,
		Kind:        "MigrationManifest",
		GeneratedAt: generatedAt.UTC(),
		Source:      source,
	}
}
