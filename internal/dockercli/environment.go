package dockercli

import (
	"fmt"
	"runtime"
	"strings"
)

func LocalHost() string {
	if runtime.GOOS == "windows" {
		return "npipe:////./pipe/docker_engine"
	}
	return "unix:///var/run/docker.sock"
}

func LocalEnvironment(env []string) ([]string, error) {
	host := LocalHost()
	filtered := make([]string, 0, len(env)+1)
	for _, entry := range env {
		key, value, _ := strings.Cut(entry, "=")
		dockerKey := key
		if runtime.GOOS == "windows" {
			dockerKey = strings.ToUpper(key)
		}
		switch dockerKey {
		case "DOCKER_HOST":
			if strings.TrimSpace(value) != "" && value != host {
				return nil, fmt.Errorf("Bort requires the local system Docker endpoint %s; unset DOCKER_HOST or set it to that exact value", host)
			}
			continue
		case "DOCKER_CONTEXT", "DOCKER_TLS_VERIFY", "DOCKER_CERT_PATH":
			if strings.TrimSpace(value) != "" {
				return nil, fmt.Errorf("Bort requires the local system Docker endpoint %s; unset %s", host, dockerKey)
			}
			continue
		}
		filtered = append(filtered, entry)
	}
	return append(filtered, "DOCKER_HOST="+host), nil
}
