package dokploy

import (
	"context"
	"strings"
	"testing"
)

func TestVerifySameDockerHostAcceptsPortPublishedByLocalDokployService(t *testing.T) {
	for _, mode := range []string{"vip", "dnsrr"} {
		t.Run(mode, func(t *testing.T) {
			runner := &fakeDockerRunner{outputs: map[string][]byte{
				"service inspect dokploy":                              []byte(`[{"Spec":{"EndpointSpec":{"Mode":"` + mode + `","Ports":[{"Protocol":"tcp","TargetPort":3000,"PublishedPort":3030,"PublishMode":"host"}]}}}]`),
				"node inspect self --format {{.ID}}":                   []byte("local-node\n"),
				"service ps --filter desired-state=running -q dokploy": []byte("task-id\n"),
				"inspect --type task task-id":                          []byte(`[{"NodeID":"local-node","Status":{"State":"running"}}]`),
			}}
			client := &Client{BaseURL: "http://127.0.0.1:3030", Docker: runner}
			if err := client.VerifySameDockerHost(context.Background()); err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestNormalizeTokenBaseURLPreservesExplicitLoopbackHTTPPort(t *testing.T) {
	baseURL, err := NormalizeTokenBaseURL("http://127.0.0.1:80/")
	if err != nil {
		t.Fatal(err)
	}
	if baseURL != "http://127.0.0.1:80" {
		t.Fatalf("normalized URL = %q, want explicit loopback port", baseURL)
	}
	runner := &fakeDockerRunner{outputs: map[string][]byte{
		"service inspect dokploy":                              []byte(`[{"Spec":{"EndpointSpec":{"Mode":"dnsrr","Ports":[{"Protocol":"tcp","TargetPort":3000,"PublishedPort":80,"PublishMode":"host"}]}}}]`),
		"node inspect self --format {{.ID}}":                   []byte("local-node\n"),
		"service ps --filter desired-state=running -q dokploy": []byte("task-id\n"),
		"inspect --type task task-id":                          []byte(`[{"NodeID":"local-node","Status":{"State":"running"}}]`),
	}}
	if err := (&Client{BaseURL: baseURL, Docker: runner}).VerifySameDockerHost(context.Background()); err != nil {
		t.Fatalf("verify normalized explicit port: %v", err)
	}
}

func TestNormalizeTokenBaseURLCanonicalizesNumericPorts(t *testing.T) {
	for _, test := range []struct {
		raw  string
		want string
	}{
		{raw: "http://127.0.0.1:03030/", want: "http://127.0.0.1:3030"},
		{raw: "https://dokploy.example:0443/", want: "https://dokploy.example"},
	} {
		got, err := NormalizeTokenBaseURL(test.raw)
		if err != nil {
			t.Fatalf("NormalizeTokenBaseURL(%q): %v", test.raw, err)
		}
		if got != test.want {
			t.Fatalf("NormalizeTokenBaseURL(%q)=%q, want %q", test.raw, got, test.want)
		}
	}
}

func TestVerifySameDockerHostRequiresRunningTaskOnLocalNode(t *testing.T) {
	runner := &fakeDockerRunner{outputs: map[string][]byte{
		"service inspect dokploy":                              []byte(`[{"Spec":{"EndpointSpec":{"Mode":"vip","Ports":[{"Protocol":"tcp","TargetPort":3000,"PublishedPort":3030,"PublishMode":"host"}]}}}]`),
		"node inspect self --format {{.ID}}":                   []byte("local-node\n"),
		"service ps --filter desired-state=running -q dokploy": []byte("task-id\n"),
		"inspect --type task task-id":                          []byte(`[{"NodeID":"other-node","Status":{"State":"running"}}]`),
	}}
	client := &Client{BaseURL: "http://127.0.0.1:3030", Docker: runner}
	err := client.VerifySameDockerHost(context.Background())
	if err == nil || !strings.Contains(err.Error(), "no running Dokploy service task") {
		t.Fatalf("expected remote-only service task refusal, got %v", err)
	}
}

func TestVerifySameDockerHostRejectsNonLoopbackWithoutDockerProbe(t *testing.T) {
	runner := &fakeDockerRunner{}
	client := &Client{BaseURL: "https://dokploy.example", Docker: runner}
	err := client.VerifySameDockerHost(context.Background())
	if err == nil || !strings.Contains(err.Error(), "requires a loopback Dokploy API URL") {
		t.Fatalf("expected non-loopback refusal, got %v", err)
	}
	if len(runner.outputArgs) != 0 {
		t.Fatalf("non-loopback refusal contacted Docker: %v", runner.outputArgs)
	}
}

func TestVerifySameDockerHostRejectsLoopbackPortNotPublishedByDokploy(t *testing.T) {
	runner := &fakeDockerRunner{outputs: map[string][]byte{
		"service inspect dokploy": []byte(`[{"Spec":{"EndpointSpec":{"Mode":"dnsrr","Ports":[{"Protocol":"tcp","TargetPort":3000,"PublishedPort":3030,"PublishMode":"host"}]}}}]`),
	}}
	client := &Client{BaseURL: "http://127.0.0.1:4040", Docker: runner}
	err := client.VerifySameDockerHost(context.Background())
	if err == nil || !strings.Contains(err.Error(), "does not match") {
		t.Fatalf("expected unrelated loopback listener refusal, got %v", err)
	}
}

func TestVerifySameDockerHostRejectsLoopbackProxyPathBeforeDockerProbe(t *testing.T) {
	runner := &fakeDockerRunner{}
	client := &Client{BaseURL: "http://127.0.0.1:3030/dokploy", Docker: runner}
	err := client.VerifySameDockerHost(context.Background())
	if err == nil || !strings.Contains(err.Error(), "root of the direct local HTTP endpoint") {
		t.Fatalf("expected loopback proxy path refusal, got %v", err)
	}
	if len(runner.outputArgs) != 0 {
		t.Fatalf("loopback proxy path refusal contacted Docker: %v", runner.outputArgs)
	}
}

func TestVerifyLocalDokployDatabaseRequiresSharedServiceNetwork(t *testing.T) {
	for _, test := range []struct {
		name        string
		postgresNet string
		env         string
		wantError   bool
	}{
		{name: "shared network", postgresNet: "dokploy-network", env: `[]`},
		{name: "explicit default database", postgresNet: "dokploy-network", env: `["POSTGRES_HOST=dokploy-postgres","POSTGRES_PORT=5432","POSTGRES_USER=dokploy","POSTGRES_DB=dokploy"]`},
		{name: "different network", postgresNet: "other-network", env: `[]`, wantError: true},
		{name: "database URL", postgresNet: "dokploy-network", env: `["DATABASE_URL=postgresql://external.example/dokploy"]`, wantError: true},
		{name: "custom host", postgresNet: "dokploy-network", env: `["POSTGRES_HOST=external-db"]`, wantError: true},
		{name: "custom port", postgresNet: "dokploy-network", env: `["POSTGRES_PORT=6543"]`, wantError: true},
		{name: "custom user", postgresNet: "dokploy-network", env: `["POSTGRES_USER=operator"]`, wantError: true},
		{name: "custom database", postgresNet: "dokploy-network", env: `["POSTGRES_DB=other"]`, wantError: true},
		{name: "duplicate selector", postgresNet: "dokploy-network", env: `["POSTGRES_HOST=dokploy-postgres","POSTGRES_HOST=dokploy-postgres"]`, wantError: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			runner := &fakeDockerRunner{outputs: map[string][]byte{
				"service inspect dokploy":                                       []byte(`[{"Spec":{"TaskTemplate":{"ContainerSpec":{"Env":` + test.env + `},"Networks":[{"Target":"dokploy-network"}]}}}]`),
				"service inspect dokploy-postgres":                              []byte(`[{"Spec":{"TaskTemplate":{"Networks":[{"Target":"` + test.postgresNet + `"}]}}}]`),
				"service ps --filter desired-state=running -q dokploy-postgres": []byte("postgres-task\n"),
				"inspect --type task postgres-task":                             []byte(`[{"NodeID":"local-node","Status":{"State":"running","ContainerStatus":{"ContainerID":"postgres-container-id"}}}]`),
			}}
			client := &Client{Docker: runner}
			app, err := inspectDokployService(context.Background(), runner, "dokploy")
			if err != nil {
				t.Fatal(err)
			}
			_, err = client.verifyLocalDokployDatabaseWith(context.Background(), app, "local-node")
			if (err != nil) != test.wantError {
				t.Fatalf("verifyLocalDokployDatabaseWith() error=%v, wantError=%t", err, test.wantError)
			}
		})
	}
}

func TestVerifySameDockerHostAndDatabaseReusesServiceAndNodeAttestation(t *testing.T) {
	runner := &fakeDockerRunner{outputs: map[string][]byte{
		"service inspect dokploy":                                       []byte(`[{"Spec":{"EndpointSpec":{"Mode":"dnsrr","Ports":[{"Protocol":"tcp","TargetPort":3000,"PublishedPort":3030,"PublishMode":"host"}]},"TaskTemplate":{"ContainerSpec":{"Env":["POSTGRES_HOST=dokploy-postgres"]},"Networks":[{"Target":"dokploy-network"}]}}}]`),
		"service inspect dokploy-postgres":                              []byte(`[{"Spec":{"TaskTemplate":{"Networks":[{"Target":"dokploy-network"}]}}}]`),
		"node inspect self --format {{.ID}}":                            []byte("local-node\n"),
		"service ps --filter desired-state=running -q dokploy":          []byte("dokploy-task\n"),
		"inspect --type task dokploy-task":                              []byte(`[{"NodeID":"local-node","Status":{"State":"running"}}]`),
		"service ps --filter desired-state=running -q dokploy-postgres": []byte("postgres-task\n"),
		"inspect --type task postgres-task":                             []byte(`[{"NodeID":"local-node","Status":{"State":"running","ContainerStatus":{"ContainerID":"postgres-container-id"}}}]`),
	}}
	client := &Client{BaseURL: "http://127.0.0.1:3030", Docker: runner}
	if err := client.VerifySameDockerHostAndDatabase(context.Background()); err != nil {
		t.Fatal(err)
	}
	counts := map[string]int{}
	for _, args := range runner.outputArgs {
		counts[strings.Join(args, " ")]++
	}
	for _, command := range []string{"service inspect dokploy", "node inspect self --format {{.ID}}"} {
		if counts[command] != 1 {
			t.Fatalf("combined host/database attestation ran %q %d times: %#v", command, counts[command], runner.outputArgs)
		}
	}
}

func TestLocalRunningServiceContainerIDRequiresExactlyOneLocalTask(t *testing.T) {
	for _, test := range []struct {
		name    string
		taskIDs string
		tasks   string
		want    string
	}{
		{
			name:    "no running tasks",
			taskIDs: "",
			want:    "has no running tasks",
		},
		{
			name:    "no local running task",
			taskIDs: "remote-task\n",
			tasks:   `[{"NodeID":"remote-node","Status":{"State":"running","ContainerStatus":{"ContainerID":"remote-container"}}}]`,
			want:    "has 0 running dokploy-postgres service tasks, want exactly 1",
		},
		{
			name:    "multiple local running tasks",
			taskIDs: "task-one\ntask-two\n",
			tasks:   `[{"NodeID":"local-node","Status":{"State":"running","ContainerStatus":{"ContainerID":"container-one"}}},{"NodeID":"local-node","Status":{"State":"running","ContainerStatus":{"ContainerID":"container-two"}}}]`,
			want:    "has 2 running dokploy-postgres service tasks, want exactly 1",
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			outputs := map[string][]byte{
				"service ps --filter desired-state=running -q dokploy-postgres": []byte(test.taskIDs),
			}
			if test.tasks != "" {
				outputs["inspect --type task "+strings.Join(strings.Fields(test.taskIDs), " ")] = []byte(test.tasks)
			}
			_, err := localRunningServiceContainerIDOnNode(context.Background(), &fakeDockerRunner{outputs: outputs}, "dokploy-postgres", "local-node")
			if err == nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("localRunningServiceContainerIDOnNode() error=%v, want %q", err, test.want)
			}
		})
	}
}

func TestLocalRunningServiceContainerIDIgnoresStaleNamedContainer(t *testing.T) {
	runner := &fakeDockerRunner{outputs: map[string][]byte{
		"ps --format {{.Names}}": []byte("dokploy-postgres.1.stale\n"),
		"service ps --filter desired-state=running -q dokploy-postgres": []byte("current-task\n"),
		"inspect --type task current-task":                              []byte(`[{"NodeID":"local-node","Status":{"State":"running","ContainerStatus":{"ContainerID":"current-container-id"}}}]`),
	}}
	containerID, err := localRunningServiceContainerIDOnNode(context.Background(), runner, "dokploy-postgres", "local-node")
	if err != nil {
		t.Fatal(err)
	}
	if containerID != "current-container-id" {
		t.Fatalf("resolved container ID %q, want current-container-id", containerID)
	}
	for _, args := range runner.outputArgs {
		if strings.Join(args, " ") == "ps --format {{.Names}}" {
			t.Fatalf("resolved Dokploy postgres through an unbound container name listing: %#v", runner.outputArgs)
		}
	}
}
