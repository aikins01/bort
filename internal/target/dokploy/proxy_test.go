package dokploy

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
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
		case StepInstallGateway, StepActivateRoutes, StepStopCoolifyProxy, StepStartDokployProxy:
			got = append(got, step.Kind)
		}
	}
	want := []StepKind{StepInstallGateway, StepStopCoolifyProxy, StepActivateRoutes, StepStartDokployProxy}
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
		if step.Kind == StepActivateRoutes || step.Kind == StepStopCoolifyProxy || step.Kind == StepStartDokployProxy {
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

func TestApplyStopCoolifyProxyDurablyFencesAlwaysRestartPolicy(t *testing.T) {
	source := dockerContainer{ID: "coolify-id", Name: coolifyProxyContainer}
	source.State.Running = true
	source.HostConfig.RestartPolicy.Name = "always"
	runner := &proxyStateRunner{containers: map[string]dockerContainer{coolifyProxyContainer: source}}
	client := &Client{Docker: runner}
	if err := client.applyStopCoolifyProxy(context.Background(), &applyContext{}, Step{Kind: StepStopCoolifyProxy, Ref: coolifyProxyContainer}); err != nil {
		t.Fatal(err)
	}
	container := runner.containers[coolifyProxyContainer]
	if container.State.Running || container.HostConfig.RestartPolicy.Name != "unless-stopped" {
		t.Fatalf("proxy stop was not durable across daemon restart: %#v", container)
	}
	updateIndex, stopIndex := -1, -1
	for index, call := range runner.calls {
		switch call {
		case "update --restart=unless-stopped coolify-id":
			updateIndex = index
		case "stop coolify-id":
			stopIndex = index
		}
	}
	if updateIndex < 0 || stopIndex <= updateIndex {
		t.Fatalf("restart policy was not made durable before stop: %v", runner.calls)
	}
}

func TestAdoptTargetTrafficAuthorityRepairsLegacyAlwaysPolicies(t *testing.T) {
	target := dockerContainer{ID: "dokploy-id", Name: dokployProxyContainer}
	target.State.Running = true
	target.HostConfig.RestartPolicy.Name = "always"
	source := dockerContainer{ID: "coolify-id", Name: coolifyProxyContainer}
	source.HostConfig.RestartPolicy.Name = "always"
	runner := &proxyStateRunner{containers: map[string]dockerContainer{dokployProxyContainer: target, coolifyProxyContainer: source}}
	client := &Client{Docker: runner}
	if err := client.AdoptTargetTrafficAuthority(context.Background()); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{dokployProxyContainer, coolifyProxyContainer} {
		if got := runner.containers[name].HostConfig.RestartPolicy.Name; got != "unless-stopped" {
			t.Fatalf("%s restart policy = %q after adoption, want unless-stopped", name, got)
		}
	}
	if !runner.containers[dokployProxyContainer].State.Running || runner.containers[coolifyProxyContainer].State.Running {
		t.Fatalf("adoption changed proxy running state: %#v", runner.containers)
	}
	updates := 0
	for _, call := range runner.calls {
		switch {
		case strings.HasPrefix(call, "start ") || strings.HasPrefix(call, "stop "):
			t.Fatalf("adoption must not start or stop proxies, got %v", runner.calls)
		case strings.HasPrefix(call, "update --restart=unless-stopped "):
			updates++
		}
	}
	if updates != 2 {
		t.Fatalf("expected both proxy policies repaired, got calls %v", runner.calls)
	}
}

func TestAdoptTargetTrafficAuthorityRefusesRunningSourceProxyWithoutStopping(t *testing.T) {
	target := dockerContainer{ID: "dokploy-id", Name: dokployProxyContainer}
	target.State.Running = true
	target.HostConfig.RestartPolicy.Name = "unless-stopped"
	source := dockerContainer{ID: "coolify-id", Name: coolifyProxyContainer}
	source.State.Running = true
	source.HostConfig.RestartPolicy.Name = "always"
	runner := &proxyStateRunner{containers: map[string]dockerContainer{dokployProxyContainer: target, coolifyProxyContainer: source}}
	client := &Client{Docker: runner}
	err := client.AdoptTargetTrafficAuthority(context.Background())
	if err == nil || !strings.Contains(err.Error(), "coolify-proxy is still running") {
		t.Fatalf("expected running source proxy to refuse adoption, got %v", err)
	}
	if !runner.containers[coolifyProxyContainer].State.Running {
		t.Fatalf("adoption stopped the source proxy: %v", runner.calls)
	}
	for _, call := range runner.calls {
		if strings.HasPrefix(call, "stop ") || strings.HasPrefix(call, "start ") {
			t.Fatalf("adoption must not start or stop proxies, got %v", runner.calls)
		}
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
			"start dp-id": []byte("dp-id\n"),
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

func TestReconcileTargetTrafficAuthorityRepairsAndVerifiesProxyState(t *testing.T) {
	source := dockerContainer{ID: "coolify-id", Name: coolifyProxyContainer}
	source.State.Running = true
	target := dockerContainer{ID: "dokploy-id", Name: dokployProxyContainer}
	runner := &proxyStateRunner{containers: map[string]dockerContainer{
		coolifyProxyContainer: source,
		dokployProxyContainer: target,
	}}
	client := &Client{Docker: runner}
	if err := client.ReconcileTargetTrafficAuthority(context.Background()); err != nil {
		t.Fatal(err)
	}
	if runner.containers[coolifyProxyContainer].State.Running || !runner.containers[dokployProxyContainer].State.Running {
		t.Fatalf("proxy authority was not reconciled: %#v", runner.containers)
	}
	for name, container := range runner.containers {
		if container.HostConfig.RestartPolicy.Name != "unless-stopped" {
			t.Fatalf("proxy %s has restart policy %q", name, container.HostConfig.RestartPolicy.Name)
		}
	}
	for _, name := range []string{coolifyProxyContainer, dokployProxyContainer} {
		inspects := 0
		for _, call := range runner.calls {
			if call == "inspect --type container "+name {
				inspects++
			}
		}
		if inspects != 2 {
			t.Fatalf("proxy %s inspected %d times, want initial and final inspections: %v", name, inspects, runner.calls)
		}
	}
}

func TestReconcileSourceTrafficAuthorityDurablyStopsTarget(t *testing.T) {
	source := dockerContainer{ID: "coolify-id", Name: coolifyProxyContainer}
	target := dockerContainer{ID: "dokploy-id", Name: dokployProxyContainer}
	target.State.Running = true
	runner := &proxyStateRunner{containers: map[string]dockerContainer{
		coolifyProxyContainer: source,
		dokployProxyContainer: target,
	}}
	client := &Client{Docker: runner}
	if err := client.ReconcileSourceTrafficAuthority(context.Background()); err != nil {
		t.Fatal(err)
	}
	if !runner.containers[coolifyProxyContainer].State.Running || runner.containers[dokployProxyContainer].State.Running {
		t.Fatalf("source authority was not reconciled: %#v", runner.containers)
	}
	if runner.containers[dokployProxyContainer].HostConfig.RestartPolicy.Name != "unless-stopped" {
		t.Fatalf("stopped target proxy can restart after daemon restart: %#v", runner.containers[dokployProxyContainer].HostConfig.RestartPolicy)
	}
}

func TestReconcileSourceTrafficAuthorityAcceptsMissingTarget(t *testing.T) {
	source := dockerContainer{ID: "coolify-id", Name: coolifyProxyContainer}
	runner := &proxyStateRunner{containers: map[string]dockerContainer{coolifyProxyContainer: source}}
	client := &Client{Docker: runner}

	if err := client.ReconcileSourceTrafficAuthority(context.Background()); err != nil {
		t.Fatalf("missing stopped target should satisfy source authority: %v", err)
	}
	if !runner.containers[coolifyProxyContainer].State.Running {
		t.Fatalf("source proxy was not started: %#v", runner.containers)
	}
}

func TestReconcileTargetTrafficAuthorityRestoresSourceWhenTargetStartFails(t *testing.T) {
	source := dockerContainer{ID: "coolify-id", Name: coolifyProxyContainer}
	source.State.Running = true
	target := dockerContainer{ID: "dokploy-id", Name: dokployProxyContainer}
	ctx, cancel := context.WithCancel(context.Background())
	runner := &proxyStateRunner{
		containers: map[string]dockerContainer{
			coolifyProxyContainer: source,
			dokployProxyContainer: target,
		},
		failAt: map[string]int{"start dokploy-id": 1},
		beforeFailure: func(call string) {
			if call == "start dokploy-id" {
				cancel()
			}
		},
	}
	client := &Client{Docker: runner}
	err := client.ReconcileTargetTrafficAuthority(ctx)
	if err == nil || !strings.Contains(err.Error(), "restored proxy traffic authority to "+coolifyProxyContainer) {
		t.Fatalf("expected failed handoff with restored source authority, got %v", err)
	}
	if !runner.containers[coolifyProxyContainer].State.Running || runner.containers[dokployProxyContainer].State.Running {
		t.Fatalf("failed target handoff did not restore source authority: %#v", runner.containers)
	}
	assertProxyCallOrder(t, runner.calls, "stop coolify-id", "start dokploy-id", "stop dokploy-id", "start coolify-id")
}

func TestReconcileTargetTrafficAuthorityRestoresSourceAfterAmbiguousStopFailure(t *testing.T) {
	source := dockerContainer{ID: "coolify-id", Name: coolifyProxyContainer}
	source.State.Running = true
	target := dockerContainer{ID: "dokploy-id", Name: dokployProxyContainer}
	runner := &proxyStateRunner{
		containers: map[string]dockerContainer{
			coolifyProxyContainer: source,
			dokployProxyContainer: target,
		},
		failAt: map[string]int{"stop coolify-id": 1},
	}
	runner.beforeFailure = func(call string) {
		if call == "stop coolify-id" {
			container := runner.containers[coolifyProxyContainer]
			container.State.Running = false
			runner.containers[coolifyProxyContainer] = container
		}
	}
	client := &Client{Docker: runner}
	err := client.ReconcileTargetTrafficAuthority(context.Background())
	if err == nil || !strings.Contains(err.Error(), "restored proxy traffic authority to "+coolifyProxyContainer) {
		t.Fatalf("expected ambiguous stop failure with restored source authority, got %v", err)
	}
	if !runner.containers[coolifyProxyContainer].State.Running || runner.containers[dokployProxyContainer].State.Running {
		t.Fatalf("ambiguous source stop left traffic without source authority: %#v", runner.containers)
	}
	assertProxyCallOrder(t, runner.calls, "stop coolify-id", "stop dokploy-id", "start coolify-id")
}

func TestReconcileSourceTrafficAuthorityRestoresTargetAfterAmbiguousStopFailure(t *testing.T) {
	source := dockerContainer{ID: "coolify-id", Name: coolifyProxyContainer}
	target := dockerContainer{ID: "dokploy-id", Name: dokployProxyContainer}
	target.State.Running = true
	runner := &proxyStateRunner{
		containers: map[string]dockerContainer{
			coolifyProxyContainer: source,
			dokployProxyContainer: target,
		},
		failAt: map[string]int{"stop dokploy-id": 1},
	}
	runner.beforeFailure = func(call string) {
		if call == "stop dokploy-id" {
			container := runner.containers[dokployProxyContainer]
			container.State.Running = false
			runner.containers[dokployProxyContainer] = container
		}
	}
	client := &Client{Docker: runner}
	err := client.ReconcileSourceTrafficAuthority(context.Background())
	if err == nil || !strings.Contains(err.Error(), "restored proxy traffic authority to "+dokployProxyContainer) {
		t.Fatalf("expected ambiguous stop failure with restored target authority, got %v", err)
	}
	if runner.containers[coolifyProxyContainer].State.Running || !runner.containers[dokployProxyContainer].State.Running {
		t.Fatalf("ambiguous target stop left traffic without target authority: %#v", runner.containers)
	}
	assertProxyCallOrder(t, runner.calls, "stop dokploy-id", "stop coolify-id", "start dokploy-id")
}

func TestReconcileTargetTrafficAuthorityRestoresPreviouslyStoppedSourceWhenTargetStartFails(t *testing.T) {
	source := dockerContainer{ID: "coolify-id", Name: coolifyProxyContainer}
	target := dockerContainer{ID: "dokploy-id", Name: dokployProxyContainer}
	runner := &proxyStateRunner{
		containers: map[string]dockerContainer{
			coolifyProxyContainer: source,
			dokployProxyContainer: target,
		},
		failAt: map[string]int{"start dokploy-id": 1},
	}
	client := &Client{Docker: runner}
	err := client.ReconcileTargetTrafficAuthority(context.Background())
	if err == nil || !strings.Contains(err.Error(), "restored proxy traffic authority to "+coolifyProxyContainer) {
		t.Fatalf("expected failed handoff with restored source authority, got %v", err)
	}
	if !runner.containers[coolifyProxyContainer].State.Running || runner.containers[dokployProxyContainer].State.Running {
		t.Fatalf("failed target handoff left both proxies stopped: %#v", runner.containers)
	}
	assertProxyCallOrder(t, runner.calls, "start dokploy-id", "stop dokploy-id", "start coolify-id")
}

func TestReconcileSourceTrafficAuthorityRestoresTargetWhenFinalVerificationFails(t *testing.T) {
	source := dockerContainer{ID: "coolify-id", Name: coolifyProxyContainer}
	target := dockerContainer{ID: "dokploy-id", Name: dokployProxyContainer}
	target.State.Running = true
	ctx, cancel := context.WithCancel(context.Background())
	runner := &proxyStateRunner{
		containers: map[string]dockerContainer{
			coolifyProxyContainer: source,
			dokployProxyContainer: target,
		},
		failAt: map[string]int{"inspect --type container coolify-proxy": 2},
		beforeFailure: func(call string) {
			if call == "inspect --type container coolify-proxy" {
				cancel()
			}
		},
	}
	client := &Client{Docker: runner}
	err := client.ReconcileSourceTrafficAuthority(ctx)
	if err == nil || !strings.Contains(err.Error(), "restored proxy traffic authority to "+dokployProxyContainer) {
		t.Fatalf("expected failed verification with restored target authority, got %v", err)
	}
	if runner.containers[coolifyProxyContainer].State.Running || !runner.containers[dokployProxyContainer].State.Running {
		t.Fatalf("failed source handoff did not restore target authority: %#v", runner.containers)
	}
	assertProxyCallOrder(t, runner.calls, "stop dokploy-id", "start coolify-id", "stop coolify-id", "start dokploy-id")
}

func TestReconcileSourceTrafficAuthorityRestoresPreviouslyStoppedTargetWhenFinalVerificationFails(t *testing.T) {
	source := dockerContainer{ID: "coolify-id", Name: coolifyProxyContainer}
	target := dockerContainer{ID: "dokploy-id", Name: dokployProxyContainer}
	runner := &proxyStateRunner{
		containers: map[string]dockerContainer{
			coolifyProxyContainer: source,
			dokployProxyContainer: target,
		},
		failAt: map[string]int{"inspect --type container coolify-proxy": 2},
	}
	client := &Client{Docker: runner}
	err := client.ReconcileSourceTrafficAuthority(context.Background())
	if err == nil || !strings.Contains(err.Error(), "restored proxy traffic authority to "+dokployProxyContainer) {
		t.Fatalf("expected failed verification with restored target authority, got %v", err)
	}
	if runner.containers[coolifyProxyContainer].State.Running || !runner.containers[dokployProxyContainer].State.Running {
		t.Fatalf("failed source handoff left both proxies stopped: %#v", runner.containers)
	}
	assertProxyCallOrder(t, runner.calls, "start coolify-id", "stop coolify-id", "start dokploy-id")
}

func assertProxyCallOrder(t *testing.T, calls []string, expected ...string) {
	t.Helper()
	previous := -1
	for _, want := range expected {
		found := -1
		for index := previous + 1; index < len(calls); index++ {
			if calls[index] == want {
				found = index
				break
			}
		}
		if found < 0 {
			t.Fatalf("proxy call %q did not appear in order %v: %v", want, expected, calls)
		}
		previous = found
	}
}

type proxyStateRunner struct {
	fakeDockerRunner
	containers    map[string]dockerContainer
	calls         []string
	callCounts    map[string]int
	failAt        map[string]int
	beforeFailure func(string)
}

func (r *proxyStateRunner) Output(ctx context.Context, args ...string) ([]byte, error) {
	call := strings.Join(args, " ")
	r.calls = append(r.calls, call)
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if r.callCounts == nil {
		r.callCounts = map[string]int{}
	}
	r.callCounts[call]++
	if r.failAt[call] == r.callCounts[call] {
		if r.beforeFailure != nil {
			r.beforeFailure(call)
		}
		return nil, errors.New("injected proxy command failure")
	}
	if len(args) == 4 && args[0] == "inspect" {
		container, ok := r.containers[args[3]]
		if !ok {
			return nil, fmt.Errorf("no such container: %s", args[3])
		}
		return json.Marshal([]dockerContainer{container})
	}
	if len(args) >= 2 && (args[0] == "start" || args[0] == "stop") {
		ref := args[len(args)-1]
		for name, container := range r.containers {
			if container.ID != ref && name != ref {
				continue
			}
			container.State.Running = args[0] == "start"
			r.containers[name] = container
			return []byte(ref + "\n"), nil
		}
	}
	if len(args) == 3 && args[0] == "update" && args[1] == "--restart=unless-stopped" {
		for name, container := range r.containers {
			if container.ID != args[2] {
				continue
			}
			container.HostConfig.RestartPolicy.Name = "unless-stopped"
			r.containers[name] = container
			return []byte(args[2] + "\n"), nil
		}
	}
	return nil, fmt.Errorf("unexpected docker command: %s", strings.Join(args, " "))
}

func TestPlanForCommitEmitsStopPerAppPlusProxyWhenRoutesExist(t *testing.T) {
	prepare := preparer.Result{Apps: []preparer.AppPlan{{Name: "api"}, {Name: "web"}}}
	cutover := gateway.Result{Apps: []gateway.AppPlan{{
		Name:   "api",
		Routes: []gateway.Route{{Host: "app.example.com"}},
	}}}
	plan := PlanForCommit(prepare, cutover)
	got := []StepKind{}
	for _, step := range plan.Steps {
		got = append(got, step.Kind)
	}
	want := []StepKind{StepStopSourceApp, StepStopSourceApp, StepStopCoolifyProxy}
	if len(got) != len(want) {
		t.Fatalf("expected %v, got %v", want, got)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("step %d: expected %s, got %s", i, want[i], got[i])
		}
	}
}

func TestPlanForCommitOmitsProxyStopForNoRouteRuns(t *testing.T) {
	// app-scoped commits without public routes must not stop the host's
	// coolify-proxy — that's a global resource still serving unrelated
	// apps. only stop_source_app steps should be emitted.
	prepare := preparer.Result{Apps: []preparer.AppPlan{{Name: "api"}}}
	plan := PlanForCommit(prepare, gateway.Result{})
	for _, step := range plan.Steps {
		if step.Kind == StepStopCoolifyProxy {
			t.Fatalf("did not expect StepStopCoolifyProxy for no-route commit, got %v", plan.Steps)
		}
	}
}

func TestApplyStopSourceAppStopsAllSourceContainers(t *testing.T) {
	app := preparer.AppPlan{Name: "api"}
	app.Resources.SourceServices = []preparer.SourceServiceRef{
		{ServiceName: "web", ContainerID: "web-id"},
	}
	app.Resources.DataStores = []preparer.DataStoreResource{
		{Service: "db", Kind: "postgres", Strategy: "migrate", SourceContainerID: "db-id"},
	}
	runner := &statefulTargetRunner{
		fakeDockerRunner: fakeDockerRunner{
			outputs: map[string][]byte{
				"inspect --type container web-id db-id": []byte(`[{"Id":"web-id","Name":"/web","State":{"Running":true,"Status":"running"}},{"Id":"db-id","Name":"/db","State":{"Running":true,"Status":"running"}}]`),
				"inspect --type container web-id":       []byte(`[{"Id":"web-id","Name":"/web","State":{"Running":true,"Status":"running"}}]`),
				"inspect --type container db-id":        []byte(`[{"Id":"db-id","Name":"/db","State":{"Running":true,"Status":"running"}}]`),
				"stop web-id":                           []byte("web-id\n"),
				"stop db-id":                            []byte("db-id\n"),
			},
		},
		stopped: map[string]bool{},
	}
	client := &Client{Docker: runner}
	actx := &applyContext{cache: map[string]*appCache{}, plan: Plan{Prepare: preparer.Result{Apps: []preparer.AppPlan{app}}}}
	if err := client.applyStopSourceApp(context.Background(), actx, Step{Kind: StepStopSourceApp, App: "api"}); err != nil {
		t.Fatalf("applyStopSourceApp: %v", err)
	}
}

func TestApplyStopSourceAppRefusesMissingReviewedContainer(t *testing.T) {
	app := preparer.AppPlan{Name: "api"}
	app.Resources.SourceServices = []preparer.SourceServiceRef{
		{ServiceName: "web", ContainerID: "web-id"},
	}
	runner := &fakeDockerRunner{
		outputs:    map[string][]byte{},
		outputErrs: map[string]error{"inspect --type container web-id": errors.New("Error: No such object: web-id")},
	}
	client := &Client{Docker: runner}
	actx := &applyContext{cache: map[string]*appCache{}, plan: Plan{Prepare: preparer.Result{Apps: []preparer.AppPlan{app}}}}
	err := client.applyStopSourceApp(context.Background(), actx, Step{Kind: StepStopSourceApp, App: "api"})
	if err == nil || !strings.Contains(err.Error(), "web-id") {
		t.Fatalf("expected missing reviewed source identity refusal, got %v", err)
	}
}

func TestApplyStopSourceAppRefusesStaleContainerID(t *testing.T) {
	app := preparer.AppPlan{Name: "api"}
	app.Resources.SourceServices = []preparer.SourceServiceRef{
		{ServiceName: "web", ContainerID: "stale-id", ContainerName: "coolify-web"},
	}
	runner := &fakeDockerRunner{
		outputs: map[string][]byte{
			"inspect --type container coolify-web": []byte(`[{"Id":"fresh-id","Name":"/coolify-web","State":{"Running":true,"Status":"running"}}]`),
			"stop fresh-id":                        []byte("fresh-id\n"),
		},
		outputErrs: map[string]error{"inspect --type container stale-id": errors.New("Error: No such object: stale-id")},
	}
	client := &Client{Docker: runner}
	actx := &applyContext{cache: map[string]*appCache{}, plan: Plan{Prepare: preparer.Result{Apps: []preparer.AppPlan{app}}}}
	err := client.applyStopSourceApp(context.Background(), actx, Step{Kind: StepStopSourceApp, App: "api"})
	if err == nil || !strings.Contains(err.Error(), "stale-id") {
		t.Fatalf("expected stale source identity refusal, got %v", err)
	}
	if fakeOutputCalled(runner, "stop", "fresh-id") {
		t.Fatalf("stale identity stopped a replacement container, calls=%v", runner.outputArgs)
	}
}
