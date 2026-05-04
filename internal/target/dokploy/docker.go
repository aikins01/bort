package dokploy

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os/exec"
	"strings"
)

// dockerRunner is the small surface bort needs to drive the local docker
// CLI. it is split from os/exec so apply tests can stub command output.
type dockerRunner interface {
	Output(ctx context.Context, args ...string) ([]byte, error)
	Run(ctx context.Context, stdin io.Reader, stdout io.Writer, args ...string) error
}

type localDockerRunner struct {
	Path string
}

func (l localDockerRunner) binary() string {
	if l.Path != "" {
		return l.Path
	}
	return "docker"
}

func (l localDockerRunner) Output(ctx context.Context, args ...string) ([]byte, error) {
	cmd := exec.CommandContext(ctx, l.binary(), args...)
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		text := strings.TrimSpace(stderr.String())
		if text != "" {
			return nil, fmt.Errorf("%w: %s", err, text)
		}
		return nil, err
	}
	return stdout.Bytes(), nil
}

func (l localDockerRunner) Run(ctx context.Context, stdin io.Reader, stdout io.Writer, args ...string) error {
	cmd := exec.CommandContext(ctx, l.binary(), args...)
	if stdin != nil {
		cmd.Stdin = stdin
	}
	if stdout != nil {
		cmd.Stdout = stdout
	}
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		text := strings.TrimSpace(stderr.String())
		if text != "" {
			return fmt.Errorf("%w: %s", err, text)
		}
		return err
	}
	return nil
}

func (c *Client) dockerRunner() dockerRunner {
	if c.Docker != nil {
		return c.Docker
	}
	return localDockerRunner{Path: c.DockerPath}
}

type dockerContainer struct {
	ID     string
	Name   string
	Config struct {
		Env    []string          `json:"Env"`
		Labels map[string]string `json:"Labels"`
	} `json:"Config"`
	State struct {
		Status  string `json:"Status"`
		Running bool   `json:"Running"`
	} `json:"State"`
	Mounts []dockerMount `json:"Mounts"`
}

type dockerMount struct {
	Type        string `json:"Type"`
	Name        string `json:"Name"`
	Source      string `json:"Source"`
	Destination string `json:"Destination"`
	RW          bool   `json:"RW"`
}

type dockerInspectRaw struct {
	ID     string `json:"Id"`
	Name   string `json:"Name"`
	Config struct {
		Env    []string          `json:"Env"`
		Labels map[string]string `json:"Labels"`
	} `json:"Config"`
	State struct {
		Status  string `json:"Status"`
		Running bool   `json:"Running"`
	} `json:"State"`
	Mounts []dockerMount `json:"Mounts"`
}

func inspectContainer(ctx context.Context, runner dockerRunner, ref string) (dockerContainer, error) {
	if strings.TrimSpace(ref) == "" {
		return dockerContainer{}, fmt.Errorf("docker inspect: empty container ref")
	}
	out, err := runner.Output(ctx, "inspect", "--type", "container", ref)
	if err != nil {
		return dockerContainer{}, fmt.Errorf("docker inspect %s: %w", ref, err)
	}
	var raw []dockerInspectRaw
	if err := json.Unmarshal(out, &raw); err != nil {
		return dockerContainer{}, fmt.Errorf("decode docker inspect %s: %w", ref, err)
	}
	if len(raw) == 0 {
		return dockerContainer{}, fmt.Errorf("docker inspect %s: no results", ref)
	}
	r := raw[0]
	return dockerContainer{
		ID:     r.ID,
		Name:   strings.TrimPrefix(r.Name, "/"),
		Config: r.Config,
		State:  r.State,
		Mounts: r.Mounts,
	}, nil
}

func listContainersByLabel(ctx context.Context, runner dockerRunner, label string) ([]dockerContainer, error) {
	out, err := runner.Output(ctx, "ps", "-a", "--filter", "label="+label, "--format", "{{.ID}}")
	if err != nil {
		return nil, fmt.Errorf("docker ps label=%s: %w", label, err)
	}
	ids := []string{}
	for _, line := range strings.Split(string(out), "\n") {
		id := strings.TrimSpace(line)
		if id != "" {
			ids = append(ids, id)
		}
	}
	containers := make([]dockerContainer, 0, len(ids))
	for _, id := range ids {
		container, err := inspectContainer(ctx, runner, id)
		if err != nil {
			return nil, err
		}
		containers = append(containers, container)
	}
	return containers, nil
}

func sourceContainer(ctx context.Context, runner dockerRunner, id, name string) (dockerContainer, error) {
	if id != "" {
		container, err := inspectContainer(ctx, runner, id)
		if err == nil {
			return container, nil
		}
		if name == "" {
			return dockerContainer{}, err
		}
	}
	if name == "" {
		return dockerContainer{}, fmt.Errorf("source container ref is empty")
	}
	return inspectContainer(ctx, runner, name)
}

func findMountByTarget(c dockerContainer, target string) (dockerMount, bool) {
	for _, mount := range c.Mounts {
		if mount.Destination == target {
			return mount, true
		}
	}
	return dockerMount{}, false
}

func envMap(env []string) map[string]string {
	values := map[string]string{}
	for _, raw := range env {
		key, value, ok := strings.Cut(raw, "=")
		if !ok {
			continue
		}
		values[key] = value
	}
	return values
}

const volumeCopyImage = "busybox:1.36"

// copyNamedVolume mirrors the contents of one named docker volume into
// another by attaching both to a one-shot container.
func copyNamedVolume(ctx context.Context, runner dockerRunner, src, dst string) error {
	if src == "" || dst == "" {
		return fmt.Errorf("copyNamedVolume: src=%q dst=%q", src, dst)
	}
	if src == dst {
		return nil
	}
	args := []string{
		"run", "--rm", "--network", "none",
		"-v", src + ":/from:ro",
		"-v", dst + ":/to",
		volumeCopyImage,
		"sh", "-c",
		"rm -rf /to/* /to/.[!.]* /to/..?* 2>/dev/null; cd /from && tar cpf - . | tar xpf - -C /to",
	}
	return runner.Run(ctx, nil, nil, args...)
}
