package dokploy

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/aikins01/bort/internal/gateway"
	"github.com/aikins01/bort/internal/preparer"
	syncplan "github.com/aikins01/bort/internal/sync"
)

func TestPlanFromArtifactsAddsProxySwapAfterRoutes(t *testing.T) {
	prepare := preparer.Result{Apps: []preparer.AppPlan{{Name: "api"}}}
	syncResult := syncplan.Result{Apps: []syncplan.AppPlan{{Name: "api"}}}
	cutover := gateway.Result{Apps: []gateway.AppPlan{{
		Name:   "api",
		Routes: []gateway.Route{{Host: "app.example.com"}},
	}}}

	plan := PlanFromArtifacts(prepare, syncResult, cutover)
	got := []StepKind{}
	for _, step := range plan.Steps {
		switch step.Kind {
		case StepInstallGateway, StepStopCoolifyProxy, StepStartDokployProxy:
			got = append(got, step.Kind)
		}
	}
	want := []StepKind{StepInstallGateway, StepStopCoolifyProxy, StepStartDokployProxy}
	if len(got) != len(want) {
		t.Fatalf("expected %v, got %v (full=%v)", want, got, plan.Steps)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("step %d: expected %s, got %s", i, want[i], got[i])
		}
	}
}

func TestPlanFromArtifactsOmitsProxySwapWhenNoRoutes(t *testing.T) {
	prepare := preparer.Result{Apps: []preparer.AppPlan{{Name: "api"}}}
	syncResult := syncplan.Result{Apps: []syncplan.AppPlan{{Name: "api"}}}
	plan := PlanFromArtifacts(prepare, syncResult, gateway.Result{})
	for _, step := range plan.Steps {
		if step.Kind == StepStopCoolifyProxy || step.Kind == StepStartDokployProxy {
			t.Fatalf("did not expect proxy swap without routes, got %v", plan.Steps)
		}
	}
}

func TestApplyStopCoolifyProxyStopsRunning(t *testing.T) {
	runner := &fakeDockerRunner{
		outputs: map[string][]byte{
			"inspect --type container coolify-proxy": []byte(`[{"Id":"cp-id","Name":"/coolify-proxy","State":{"Running":true,"Status":"running"}}]`),
			"stop cp-id":                             []byte("cp-id\n"),
		},
	}
	client := &Client{Docker: runner}
	if err := client.applyStopCoolifyProxy(context.Background(), &applyContext{}, Step{Kind: StepStopCoolifyProxy, Ref: coolifyProxyContainer}); err != nil {
		t.Fatalf("applyStopCoolifyProxy: %v", err)
	}
}

func TestApplyStopCoolifyProxyNoopWhenAlreadyStopped(t *testing.T) {
	runner := &fakeDockerRunner{
		outputs: map[string][]byte{
			"inspect --type container coolify-proxy": []byte(`[{"Id":"cp-id","Name":"/coolify-proxy","State":{"Running":false,"Status":"exited"}}]`),
		},
	}
	client := &Client{Docker: runner}
	// stub omits "stop cp-id" — applyStopCoolifyProxy must skip docker stop
	// for an already-stopped proxy or fakeDockerRunner errors out.
	if err := client.applyStopCoolifyProxy(context.Background(), &applyContext{}, Step{Kind: StepStopCoolifyProxy, Ref: coolifyProxyContainer}); err != nil {
		t.Fatalf("applyStopCoolifyProxy: %v", err)
	}
}

func TestApplyStopCoolifyProxyIgnoresMissingContainer(t *testing.T) {
	client := &Client{Docker: &fakeDockerRunner{outputs: map[string][]byte{}}}
	// fakeDockerRunner returns "docker output not stubbed: ..." which is
	// neither "no such container" nor "no results" — confirm only those
	// canonical missing-container messages are swallowed.
	err := client.applyStopCoolifyProxy(context.Background(), &applyContext{}, Step{Kind: StepStopCoolifyProxy, Ref: coolifyProxyContainer})
	if err == nil || !strings.Contains(err.Error(), "inspect proxy container") {
		t.Fatalf("expected unstubbed inspect error to bubble up, got %v", err)
	}

	// real docker emits "no such container: coolify-proxy" when missing;
	// that case must be swallowed so stop is idempotent.
	if !isContainerMissingErr(errors.New("docker inspect coolify-proxy: Error: No such container: coolify-proxy")) {
		t.Fatal("expected no-such-container error to be classified as missing")
	}
	if !isContainerMissingErr(errors.New("Error: No such object: coolify-proxy")) {
		t.Fatal("expected no-such-object error to be classified as missing")
	}
	if !isContainerMissingErr(errors.New("docker inspect coolify-proxy: no results")) {
		t.Fatal("expected no-results error to be classified as missing")
	}
	// bare "not found" must not match — that is a common substring in
	// missing-binary / missing-image errors and would mask real failures.
	if isContainerMissingErr(errors.New("exec: \"docker\": executable file not found in $PATH")) {
		t.Fatal("expected missing-binary error to NOT be classified as missing container")
	}
}

func TestApplyStartDokployProxyStartsStopped(t *testing.T) {
	runner := &fakeDockerRunner{
		outputs: map[string][]byte{
			"inspect --type container dokploy-traefik": []byte(`[{"Id":"dp-id","Name":"/dokploy-traefik","State":{"Running":false,"Status":"exited"}}]`),
			"start dp-id":                              []byte("dp-id\n"),
		},
	}
	client := &Client{Docker: runner}
	if err := client.applyStartDokployProxy(context.Background(), &applyContext{}, Step{Kind: StepStartDokployProxy, Ref: dokployProxyContainer}); err != nil {
		t.Fatalf("applyStartDokployProxy: %v", err)
	}
}

func TestApplyStartDokployProxyErrorsWhenMissing(t *testing.T) {
	client := &Client{Docker: &fakeDockerRunner{outputs: map[string][]byte{}}}
	// missing dokploy-traefik must error: it means dokploy itself is not
	// installed, which init-target --install (workstream b.3) covers.
	err := client.applyStartDokployProxy(context.Background(), &applyContext{}, Step{Kind: StepStartDokployProxy, Ref: dokployProxyContainer})
	if err == nil {
		t.Fatal("expected error when dokploy-traefik is missing")
	}
}
