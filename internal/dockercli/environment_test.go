package dockercli

import (
	"runtime"
	"strings"
	"testing"
)

func TestLocalEnvironmentPinsLocalDaemon(t *testing.T) {
	host := "unix:///var/run/docker.sock"
	if runtime.GOOS == "windows" {
		host = "npipe:////./pipe/docker_engine"
	}
	env, err := LocalEnvironment([]string{"PATH=/usr/bin", "DOCKER_HOST=" + host})
	if err != nil {
		t.Fatal(err)
	}
	joined := strings.Join(env, "\n")
	if !strings.Contains(joined, "DOCKER_HOST="+host) || !strings.Contains(joined, "PATH=/usr/bin") {
		t.Fatalf("local Docker environment=%v", env)
	}
	for _, input := range [][]string{{"PATH=/usr/bin"}, {"PATH=/usr/bin", "DOCKER_HOST="}} {
		env, err := LocalEnvironment(input)
		if err != nil {
			t.Fatal(err)
		}
		pinned := 0
		for _, entry := range env {
			if strings.HasPrefix(entry, "DOCKER_HOST=") {
				if entry != "DOCKER_HOST="+host {
					t.Fatalf("local Docker environment for %v=%v", input, env)
				}
				pinned++
			}
		}
		if pinned != 1 {
			t.Fatalf("local Docker environment for %v=%v", input, env)
		}
	}
	for _, setting := range []string{"DOCKER_HOST=ssh://remote.example", "DOCKER_CONTEXT=production", "DOCKER_TLS_VERIFY=1", "DOCKER_CERT_PATH=/tmp/remote-certs"} {
		_, err := LocalEnvironment([]string{setting})
		if err == nil || !strings.Contains(err.Error(), strings.SplitN(setting, "=", 2)[0]) {
			t.Fatalf("expected %s refusal, got %v", setting, err)
		}
	}
	if runtime.GOOS == "windows" {
		if _, err := LocalEnvironment([]string{"docker_context=production"}); err == nil || !strings.Contains(err.Error(), "DOCKER_CONTEXT") {
			t.Fatalf("expected lowercase Windows Docker context refusal, got %v", err)
		}
	}
}
