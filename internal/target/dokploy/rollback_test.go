package dokploy

import (
	"context"
	"errors"
	"fmt"
	"io"
	"strings"
	"testing"
	"time"

	"github.com/aikins01/bort/internal/gateway"
	"github.com/aikins01/bort/internal/preparer"
)

func rollbackPrepareFixture() preparer.Result {
	api := preparer.AppPlan{Name: "api"}
	api.Resources.SourceServices = []preparer.SourceServiceRef{
		{ServiceName: "web", ContainerID: "api-web-id"},
	}
	return preparer.Result{Apps: []preparer.AppPlan{api, {Name: "assets"}}}
}

func rollbackCutoverFixture() gateway.Result {
	return gateway.Result{Apps: []gateway.AppPlan{{
		Name:   "api",
		Routes: []gateway.Route{{Host: "app.example.com"}},
	}}}
}

func stepKinds(steps []Step) []StepKind {
	kinds := make([]StepKind, 0, len(steps))
	for _, step := range steps {
		kinds = append(kinds, step.Kind)
	}
	return kinds
}

func TestPlanForRollbackOrdersSteps(t *testing.T) {
	plan := PlanForRollback(rollbackPrepareFixture(), rollbackCutoverFixture())
	want := []StepKind{
		StepResumeSource,
		StepVerifySourceHealth,
		StepStopDokployProxy,
		StepStartCoolifyProxy,
		StepObserveRollback,
	}
	got := stepKinds(plan.Steps)
	if len(got) != len(want) {
		t.Fatalf("expected steps %v, got %v", want, got)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("expected steps %v, got %v", want, got)
		}
	}
	for _, step := range plan.Steps {
		if step.Kind == StepVerifySourceHealth || step.Kind == StepObserveRollback {
			if step.App != "api" {
				t.Fatalf("expected app-scoped step for api, got %v", step)
			}
		}
	}
}

func TestPlanForRollbackSkipsProxySwapWithoutRoutes(t *testing.T) {
	plan := PlanForRollback(rollbackPrepareFixture(), gateway.Result{})
	for _, step := range plan.Steps {
		switch step.Kind {
		case StepStopDokployProxy, StepStartCoolifyProxy, StepObserveRollback:
			t.Fatalf("did not expect proxy swap or observation without routes, got %v", plan.Steps)
		}
	}
	got := stepKinds(plan.Steps)
	if len(got) != 2 || got[0] != StepResumeSource || got[1] != StepVerifySourceHealth {
		t.Fatalf("expected resume and verify only, got %v", got)
	}
}

type dependentSourceRunner struct {
	fakeDockerRunner
	running map[string]bool
}

func (r *dependentSourceRunner) Output(_ context.Context, args ...string) ([]byte, error) {
	if args[0] == "start" {
		r.running[args[1]] = true
		return nil, nil
	}
	id := args[len(args)-1]
	health := "healthy"
	if id == "api-web-id" && !r.running["worker-id"] {
		health = "unhealthy"
	}
	return []byte(fmt.Sprintf(`[{"Id":%q,"State":{"Running":%t,"Health":{"Status":%q}}}]`, id, r.running[id], health)), nil
}

func TestRollbackStartsDependenciesBeforeCheckingHealth(t *testing.T) {
	prepare := rollbackPrepareFixture()
	worker := preparer.AppPlan{Name: "worker"}
	worker.Resources.SourceServices = []preparer.SourceServiceRef{{ServiceName: "worker", ContainerID: "worker-id"}}
	prepare.Apps = append(prepare.Apps, worker)
	runner := &dependentSourceRunner{running: map[string]bool{}}
	client := &Client{Docker: runner}
	if err := client.Apply(context.Background(), PlanForRollback(prepare, gateway.Result{})); err != nil {
		t.Fatalf("expected dependent apps to resume before health checks: %v", err)
	}
	if !runner.running["api-web-id"] || !runner.running["worker-id"] {
		t.Fatalf("expected both sources running, got %v", runner.running)
	}
}

func TestPlanForRollbackSkimsAppsWithoutQuiesceTargets(t *testing.T) {
	prepare := preparer.Result{Apps: []preparer.AppPlan{{Name: "assets"}}}
	plan := PlanForRollback(prepare, rollbackCutoverFixture())
	got := stepKinds(plan.Steps)
	if len(got) != 2 || got[0] != StepStopDokployProxy || got[1] != StepStartCoolifyProxy {
		t.Fatalf("expected proxy swap only for a run that stopped no source containers, got %v", got)
	}
}

type flappingHealthRunner struct {
	fakeDockerRunner
	calls int
}

type deadlineInspectRunner struct {
	t                   *testing.T
	waitForCancellation bool
	calls               int
}

func (r *deadlineInspectRunner) Output(ctx context.Context, _ ...string) ([]byte, error) {
	r.calls++
	if _, ok := ctx.Deadline(); !ok {
		r.t.Error("expected inspect context to have a deadline")
	}
	if r.waitForCancellation {
		<-ctx.Done()
		return nil, ctx.Err()
	}
	return []byte(`[{"Id":"api-web-id","Name":"/api-web","State":{"Running":true,"Status":"running"}}]`), nil
}

func (r *deadlineInspectRunner) Run(context.Context, io.Reader, io.Writer, ...string) error {
	return errors.New("unexpected docker run")
}

func TestVerifySourceContainerHealthyBoundsInitialInspection(t *testing.T) {
	runner := &deadlineInspectRunner{t: t}
	err := verifySourceContainerHealthy(context.Background(), runner, commitTargetRef{id: "api-web-id"})
	if err != nil {
		t.Fatalf("verifySourceContainerHealthy: %v", err)
	}
	if runner.calls != 1 {
		t.Fatalf("expected one inspection, got %d", runner.calls)
	}
}

func TestVerifySourceContainerHealthyPropagatesCancellation(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	runner := &deadlineInspectRunner{t: t, waitForCancellation: true}
	err := verifySourceContainerHealthy(ctx, runner, commitTargetRef{id: "api-web-id"})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("expected cancellation, got %v", err)
	}
}

func TestRollbackBoundsEveryStepInspection(t *testing.T) {
	oldSettle := rollbackObserveSettle
	rollbackObserveSettle = 0
	t.Cleanup(func() { rollbackObserveSettle = oldSettle })
	plan := PlanForRollback(rollbackPrepareFixture(), rollbackCutoverFixture())
	if plan.stepTimeout != dockerStartTimeout {
		t.Fatalf("expected bounded rollback steps, got %s", plan.stepTimeout)
	}
	for _, step := range plan.Steps {
		t.Run(string(step.Kind), func(t *testing.T) {
			runner := &deadlineInspectRunner{t: t, waitForCancellation: true}
			client := &Client{Docker: runner}
			stepPlan := plan
			stepPlan.Steps = []Step{step}
			stepPlan.stepTimeout = 10 * time.Millisecond
			if err := client.Apply(context.Background(), stepPlan); !errors.Is(err, context.DeadlineExceeded) {
				t.Fatalf("expected timed out inspection for %s, got %v", step.Kind, err)
			}
			if runner.calls == 0 {
				t.Fatal("expected a Docker inspection")
			}
		})
	}
}

func (f *flappingHealthRunner) Output(ctx context.Context, args ...string) ([]byte, error) {
	f.calls++
	health := "starting"
	if f.calls > 1 {
		health = "healthy"
	}
	return []byte(`[{"Id":"api-web-id","Name":"/api-web","State":{"Running":true,"Status":"running","Health":{"Status":"` + health + `"}}}]`), nil
}

func TestApplyVerifySourceHealthAcceptsRunningContainer(t *testing.T) {
	runner := &fakeDockerRunner{
		outputs: map[string][]byte{
			"inspect --type container api-web-id": []byte(`[{"Id":"api-web-id","Name":"/api-web","State":{"Running":true,"Status":"running"}}]`),
		},
	}
	client := &Client{Docker: runner}
	actx := &applyContext{cache: map[string]*appCache{}, plan: Plan{Prepare: rollbackPrepareFixture()}}
	if err := client.applyVerifySourceHealth(context.Background(), actx, Step{Kind: StepVerifySourceHealth, App: "api"}); err != nil {
		t.Fatalf("applyVerifySourceHealth: %v", err)
	}
}

func TestApplyVerifySourceHealthRefusesWhenNotRunning(t *testing.T) {
	runner := &fakeDockerRunner{
		outputs: map[string][]byte{
			"inspect --type container api-web-id": []byte(`[{"Id":"api-web-id","Name":"/api-web","State":{"Running":false,"Status":"exited"}}]`),
		},
	}
	client := &Client{Docker: runner}
	actx := &applyContext{cache: map[string]*appCache{}, plan: Plan{Prepare: rollbackPrepareFixture()}}
	err := client.applyVerifySourceHealth(context.Background(), actx, Step{Kind: StepVerifySourceHealth, App: "api"})
	if err == nil || !strings.Contains(err.Error(), "not returning traffic to the source") {
		t.Fatalf("expected refusal when the source did not restart, got %v", err)
	}
}

func TestApplyVerifySourceHealthRefusesUnhealthy(t *testing.T) {
	runner := &fakeDockerRunner{
		outputs: map[string][]byte{
			"inspect --type container api-web-id": []byte(`[{"Id":"api-web-id","Name":"/api-web","State":{"Running":true,"Status":"running","Health":{"Status":"unhealthy"}}}]`),
		},
	}
	client := &Client{Docker: runner}
	actx := &applyContext{cache: map[string]*appCache{}, plan: Plan{Prepare: rollbackPrepareFixture()}}
	err := client.applyVerifySourceHealth(context.Background(), actx, Step{Kind: StepVerifySourceHealth, App: "api"})
	if err == nil || !strings.Contains(err.Error(), "reports unhealthy") {
		t.Fatalf("expected refusal for an unhealthy container, got %v", err)
	}
}

func TestApplyVerifySourceHealthPollsUntilHealthy(t *testing.T) {
	oldInterval := sourceHealthPollInterval
	sourceHealthPollInterval = 0
	defer func() { sourceHealthPollInterval = oldInterval }()

	runner := &flappingHealthRunner{}
	client := &Client{Docker: runner}
	actx := &applyContext{cache: map[string]*appCache{}, plan: Plan{Prepare: rollbackPrepareFixture()}}
	if err := client.applyVerifySourceHealth(context.Background(), actx, Step{Kind: StepVerifySourceHealth, App: "api"}); err != nil {
		t.Fatalf("expected starting health to be polled until healthy, got %v", err)
	}
	if runner.calls < 2 {
		t.Fatalf("expected at least two health polls, calls=%d", runner.calls)
	}
}

func TestApplyObserveRollbackPassesWhenAllRunning(t *testing.T) {
	oldSettle := rollbackObserveSettle
	rollbackObserveSettle = 0
	defer func() { rollbackObserveSettle = oldSettle }()

	runner := &fakeDockerRunner{
		outputs: map[string][]byte{
			"inspect --type container api-web-id":    []byte(`[{"Id":"api-web-id","Name":"/api-web","State":{"Running":true,"Status":"running"}}]`),
			"inspect --type container coolify-proxy": []byte(`[{"Id":"cp-id","Name":"/coolify-proxy","State":{"Running":true,"Status":"running"}}]`),
		},
	}
	client := &Client{Docker: runner}
	actx := &applyContext{cache: map[string]*appCache{}, plan: Plan{Prepare: rollbackPrepareFixture()}}
	if err := client.applyObserveRollback(context.Background(), actx, Step{Kind: StepObserveRollback, App: "api"}); err != nil {
		t.Fatalf("applyObserveRollback: %v", err)
	}
}

func TestApplyObserveRollbackReportsStoppedContainer(t *testing.T) {
	oldSettle := rollbackObserveSettle
	rollbackObserveSettle = 0
	defer func() { rollbackObserveSettle = oldSettle }()

	runner := &fakeDockerRunner{
		outputs: map[string][]byte{
			"inspect --type container api-web-id": []byte(`[{"Id":"api-web-id","Name":"/api-web","State":{"Running":false,"Status":"exited"}}]`),
		},
	}
	client := &Client{Docker: runner}
	actx := &applyContext{cache: map[string]*appCache{}, plan: Plan{Prepare: rollbackPrepareFixture()}}
	err := client.applyObserveRollback(context.Background(), actx, Step{Kind: StepObserveRollback, App: "api"})
	if err == nil || !strings.Contains(err.Error(), "stopped after rollback") {
		t.Fatalf("expected observation to report a stopped source container, got %v", err)
	}
}

func TestApplyObserveRollbackReportsStoppedProxy(t *testing.T) {
	oldSettle := rollbackObserveSettle
	rollbackObserveSettle = 0
	defer func() { rollbackObserveSettle = oldSettle }()

	runner := &fakeDockerRunner{
		outputs: map[string][]byte{
			"inspect --type container api-web-id":    []byte(`[{"Id":"api-web-id","Name":"/api-web","State":{"Running":true,"Status":"running"}}]`),
			"inspect --type container coolify-proxy": []byte(`[{"Id":"cp-id","Name":"/coolify-proxy","State":{"Running":false,"Status":"exited"}}]`),
		},
	}
	client := &Client{Docker: runner}
	actx := &applyContext{cache: map[string]*appCache{}, plan: Plan{Prepare: rollbackPrepareFixture()}}
	err := client.applyObserveRollback(context.Background(), actx, Step{Kind: StepObserveRollback, App: "api"})
	if err == nil || !strings.Contains(err.Error(), "source traffic is not being served") {
		t.Fatalf("expected observation to report a stopped source proxy, got %v", err)
	}
}

func TestApplyObserveRollbackRejectsUnhealthyRunningSource(t *testing.T) {
	oldSettle := rollbackObserveSettle
	rollbackObserveSettle = 0
	t.Cleanup(func() { rollbackObserveSettle = oldSettle })
	runner := &fakeDockerRunner{outputs: map[string][]byte{
		"inspect --type container api-web-id": []byte(`[{"Id":"api-web-id","State":{"Running":true,"Health":{"Status":"healthy"}}}]`),
	}}
	client := &Client{Docker: runner}
	actx := &applyContext{plan: Plan{Prepare: rollbackPrepareFixture()}}
	if err := client.applyVerifySourceHealth(context.Background(), actx, Step{App: "api"}); err != nil {
		t.Fatal(err)
	}
	runner.outputs["inspect --type container api-web-id"] = []byte(`[{"Id":"api-web-id","State":{"Running":true,"Health":{"Status":"unhealthy"}}}]`)
	if err := client.applyObserveRollback(context.Background(), actx, Step{App: "api"}); err == nil || !strings.Contains(err.Error(), "unhealthy after rollback") {
		t.Fatalf("expected post-swap health failure, got %v", err)
	}
}

func TestApplyStartCoolifyProxyStartsStoppedAndVerifiesRunning(t *testing.T) {
	runner := &fakeDockerRunner{
		outputs: map[string][]byte{
			"inspect --type container coolify-proxy": []byte(`[{"Id":"cp-id","Name":"/coolify-proxy","State":{"Running":false,"Status":"exited"}}]`),
			"start cp-id":                            []byte("cp-id\n"),
		},
	}
	client := &Client{Docker: runner}
	err := client.applyStartCoolifyProxy(context.Background(), &applyContext{}, Step{Kind: StepStartCoolifyProxy, Ref: coolifyProxyContainer})
	if err == nil || !strings.Contains(err.Error(), "did not stay running") {
		t.Fatalf("expected refusal when the proxy does not stay running, got %v", err)
	}
}

func TestApplyStartCoolifyProxyAcceptsRunningProxy(t *testing.T) {
	runner := &fakeDockerRunner{
		outputs: map[string][]byte{
			"inspect --type container coolify-proxy": []byte(`[{"Id":"cp-id","Name":"/coolify-proxy","State":{"Running":true,"Status":"running"}}]`),
		},
	}
	client := &Client{Docker: runner}
	if err := client.applyStartCoolifyProxy(context.Background(), &applyContext{}, Step{Kind: StepStartCoolifyProxy, Ref: coolifyProxyContainer}); err != nil {
		t.Fatalf("applyStartCoolifyProxy: %v", err)
	}
}

func TestApplyStartCoolifyProxyErrorsWhenMissing(t *testing.T) {
	client := &Client{Docker: &fakeDockerRunner{outputs: map[string][]byte{}}}
	err := client.applyStartCoolifyProxy(context.Background(), &applyContext{}, Step{Kind: StepStartCoolifyProxy, Ref: coolifyProxyContainer})
	if err == nil {
		t.Fatal("expected error when coolify-proxy is missing: bort cannot restore source traffic without it")
	}
}
