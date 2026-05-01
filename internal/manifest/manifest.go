package manifest

import "time"

const APIVersion = "bort.io/v1alpha1"

type Manifest struct {
	APIVersion  string    `json:"apiVersion"`
	Kind        string    `json:"kind"`
	GeneratedAt time.Time `json:"generatedAt"`
	Source      Source    `json:"source"`
	Apps        []App     `json:"apps"`
	Volumes     []Volume  `json:"volumes,omitempty"`
	Networks    []Network `json:"networks,omitempty"`
	Warnings    []Warning `json:"warnings,omitempty"`
}

type Source struct {
	Platform string `json:"platform"`
	Hostname string `json:"hostname,omitempty"`
}

type App struct {
	ID       string            `json:"id"`
	Name     string            `json:"name"`
	Platform string            `json:"platform,omitempty"`
	Runtime  string            `json:"runtime,omitempty"`
	Labels   map[string]string `json:"labels,omitempty"`
	Services []Service         `json:"services"`
	Routes   []Route           `json:"routes,omitempty"`
	Warnings []Warning         `json:"warnings,omitempty"`
}

type Service struct {
	ID          string            `json:"id"`
	Name        string            `json:"name"`
	Image       string            `json:"image,omitempty"`
	Status      string            `json:"status,omitempty"`
	Environment []EnvVar          `json:"environment,omitempty"`
	Mounts      []Mount           `json:"mounts,omitempty"`
	Ports       []Port            `json:"ports,omitempty"`
	Networks    []ServiceNetwork  `json:"networks,omitempty"`
	Labels      map[string]string `json:"labels,omitempty"`
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
