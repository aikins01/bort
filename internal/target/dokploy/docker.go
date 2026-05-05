package dokploy

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
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

const (
	dockerStopTimeout        = 30 * time.Second
	dockerStopFallbackGrace  = "2"
	dockerStopFallbackWindow = 30 * time.Second
	dockerStartTimeout       = 3 * time.Minute
)

func stopContainer(ctx context.Context, runner dockerRunner, id string) error {
	stopCtx, cancel := context.WithTimeout(ctx, dockerStopTimeout)
	defer cancel()
	if _, err := runner.Output(stopCtx, "stop", id); err != nil {
		if !errors.Is(stopCtx.Err(), context.DeadlineExceeded) {
			return err
		}
		shortStopCtx, cancelShortStop := context.WithTimeout(context.Background(), dockerStopFallbackWindow)
		shortStopErr := func() error {
			defer cancelShortStop()
			_, shortStopErr := runner.Output(shortStopCtx, "stop", "-t", dockerStopFallbackGrace, id)
			return shortStopErr
		}()
		if shortStopErr == nil || containerStoppedAfterStopFailure(runner, id) {
			return nil
		}
		killCtx, cancelKill := context.WithTimeout(context.Background(), 15*time.Second)
		defer cancelKill()
		if _, killErr := runner.Output(killCtx, "kill", id); killErr != nil {
			if containerStoppedAfterStopFailure(runner, id) {
				return nil
			}
			return fmt.Errorf("docker stop %s timed out and kill failed: %w", id, killErr)
		}
	}
	return nil
}

func containerStoppedAfterStopFailure(runner dockerRunner, id string) bool {
	inspectCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	container, err := inspectContainer(inspectCtx, runner, id)
	return err == nil && !container.State.Running
}

func startContainer(ctx context.Context, runner dockerRunner, id string) error {
	startCtx, cancel := context.WithTimeout(ctx, dockerStartTimeout)
	defer cancel()
	if _, err := runner.Output(startCtx, "start", id); err != nil {
		if containerRunningAfterStartFailure(runner, id) {
			return nil
		}
		return err
	}
	return nil
}

func containerRunningAfterStartFailure(runner dockerRunner, id string) bool {
	inspectCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	container, err := inspectContainer(inspectCtx, runner, id)
	return err == nil && container.State.Running
}

type dockerContainer struct {
	ID     string
	Name   string
	Image  string
	Config struct {
		Image  string            `json:"Image"`
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
	Image  string `json:"Image"`
	Config struct {
		Image  string            `json:"Image"`
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
		Image:  r.Image,
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
	if len(ids) == 0 {
		return nil, nil
	}
	args := append([]string{"inspect", "--type", "container"}, ids...)
	raw, err := runner.Output(ctx, args...)
	if err != nil {
		return nil, fmt.Errorf("docker inspect ids=%v: %w", ids, err)
	}
	var decoded []dockerInspectRaw
	if err := json.Unmarshal(raw, &decoded); err != nil {
		return nil, fmt.Errorf("decode docker inspect batch: %w", err)
	}
	containers := make([]dockerContainer, 0, len(decoded))
	for _, r := range decoded {
		containers = append(containers, dockerContainer{
			ID:     r.ID,
			Name:   strings.TrimPrefix(r.Name, "/"),
			Image:  r.Image,
			Config: r.Config,
			State:  r.State,
			Mounts: r.Mounts,
		})
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
// another by attaching both to a one-shot container. both volumes must
// already exist; otherwise docker would silently auto-create an empty
// volume and the copy would clobber the target with nothing.
func copyNamedVolume(ctx context.Context, runner dockerRunner, src, dst string) error {
	if src == "" || dst == "" {
		return fmt.Errorf("copyNamedVolume: src=%q dst=%q", src, dst)
	}
	if src == dst {
		return nil
	}
	if err := requireDockerVolume(ctx, runner, src); err != nil {
		return err
	}
	if err := requireDockerVolume(ctx, runner, dst); err != nil {
		return err
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

func requireDockerVolume(ctx context.Context, runner dockerRunner, name string) error {
	if strings.TrimSpace(name) == "" {
		return fmt.Errorf("docker volume name is empty")
	}
	if _, err := runner.Output(ctx, "volume", "inspect", name); err != nil {
		return fmt.Errorf("docker volume %q not found: %w", name, err)
	}
	return nil
}

// rsyncImage carries an alpine rsync binary so we don't depend on the
// host having rsync installed. pinned to a concrete revision tag so a
// future upstream rebuild can't change the migration's behavior.
const rsyncImage = "instrumentisto/rsync-ssh:alpine3.23-r3"

// rsyncBindMount mirrors one host directory into another by attaching
// both to a one-shot rsync container. it uses --mount instead of -v
// because --mount fails fast when the source path is missing; -v would
// silently auto-create an empty directory and --delete would then
// clobber the target with nothing.
//
// archive flags preserve permissions, hardlinks, ACLs, and xattrs.
// security.selinux is filtered out because labels are host- and path-
// specific; copying source labels to a dokploy-managed path would lock
// the target app out. label=disable also stops docker from relabeling
// the host bind sources on enforcing systems.
func rsyncBindMount(ctx context.Context, runner dockerRunner, src, dst string) error {
	if err := validateBindPath("source", src); err != nil {
		return err
	}
	if err := validateBindPath("target", dst); err != nil {
		return err
	}
	if src == dst {
		return nil
	}
	args := []string{
		"run", "--rm", "--network", "none",
		"--security-opt", "label=disable",
		"--mount", "type=bind,src=" + src + ",dst=/from,readonly",
		"--mount", "type=bind,src=" + dst + ",dst=/to",
		rsyncImage,
		"rsync", "-aHAX", "--numeric-ids",
		"--filter=-x security.selinux",
		"--delete", "/from/", "/to/",
	}
	return runner.Run(ctx, nil, nil, args...)
}

// validateBindPath rejects paths that would be unsafe to bind into the
// rsync container. relative paths can resolve against the container's
// cwd; "/" or empty would let --delete wipe the host root; commas and
// quotes break docker's csv --mount syntax with no portable escaping.
func validateBindPath(role, path string) error {
	if path == "" {
		return fmt.Errorf("rsyncBindMount: %s path is empty", role)
	}
	if !filepath.IsAbs(path) {
		return fmt.Errorf("rsyncBindMount: %s path must be absolute (got %q)", role, path)
	}
	cleaned := filepath.Clean(path)
	if cleaned == "/" {
		return fmt.Errorf("rsyncBindMount: refusing to operate on host root (%s=%q)", role, path)
	}
	if strings.ContainsAny(path, ",\"") {
		return fmt.Errorf("rsyncBindMount: %s path contains unsupported character (got %q)", role, path)
	}
	return nil
}
