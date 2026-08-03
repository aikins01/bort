package dokploy

import (
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestCleanupStalePlatformProjectsBacksUpThenDeletesMetadata(t *testing.T) {
	backupDir := dokployBackupTestDir(t)
	if err := os.Chmod(backupDir, 0o700); err != nil {
		t.Fatal(err)
	}
	runner := &fakeDockerRunner{
		outputs: map[string][]byte{
			"ps --format {{.Names}}": []byte("dokploy-postgres.1.task\n"),
		},
		runOutputs: map[string][]byte{
			"exec -i dokploy-postgres.1.task psql -U dokploy -d dokploy -v ON_ERROR_STOP=1 -At -F |": []byte("BEGIN\ndeleted|proxy|p1\nCOMMIT\n"),
		},
	}
	client := &Client{Docker: runner}

	result, err := client.CleanupStalePlatformProjects(context.Background(), StalePlatformCleanupOptions{
		ProjectNames: []string{"proxy", "source", "proxy"},
		BackupDir:    backupDir,
		BackupPrefix: "cleanup-test",
	})
	if err != nil {
		t.Fatalf("CleanupStalePlatformProjects: %v", err)
	}
	if len(result.Deleted) != 1 || result.Deleted[0].Name != "proxy" || result.Deleted[0].ProjectID != "p1" {
		t.Fatalf("unexpected deleted projects: %#v", result.Deleted)
	}
	if !strings.HasPrefix(result.BackupPath, filepath.Join(backupDir, "cleanup-test-")) {
		t.Fatalf("unexpected backup path: %s", result.BackupPath)
	}
	backup, err := os.ReadFile(result.BackupPath)
	if err != nil {
		t.Fatalf("read backup: %v", err)
	}
	if string(backup) != "dump-bytes" {
		t.Fatalf("expected pg_dump backup contents, got %q", string(backup))
	}
	dirInfo, err := os.Stat(backupDir)
	if err != nil {
		t.Fatal(err)
	}
	if dirInfo.Mode().Perm() != 0o700 {
		t.Fatalf("existing backup directory mode changed to %o", dirInfo.Mode().Perm())
	}
	if len(runner.runs) != 2 {
		t.Fatalf("expected pg_dump and psql runs, got %#v", runner.runs)
	}
	if got := strings.Join(runner.runs[0].Args, " "); !strings.Contains(got, "pg_dump") {
		t.Fatalf("expected backup before delete, got first run %q", got)
	}
	if got := strings.Join(runner.runs[1].Args, " "); !strings.Contains(got, "psql") {
		t.Fatalf("expected psql delete second, got %q", got)
	}
	sql := string(runner.runs[1].Stdin)
	for _, want := range []string{"('proxy', null)", "('source', null)", "project_id text", "refusing to delete stale platform projects with attached Dokploy resources", "compose_count", "delete from project"} {
		if !strings.Contains(sql, want) {
			t.Fatalf("expected cleanup sql to contain %q, got:\n%s", want, sql)
		}
	}
}

func TestBackupDokployDatabaseRejectsPermissiveDirectoryWithoutChangingMode(t *testing.T) {
	backupDir := dokployBackupTestDir(t)
	if err := os.Chmod(backupDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if _, err := backupDokployDatabase(context.Background(), &fakeDockerRunner{}, "dokploy-postgres", backupDir, "cleanup"); err == nil {
		t.Fatal("expected permissive backup directory to be rejected")
	}
	info, err := os.Stat(backupDir)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o755 {
		t.Fatalf("existing backup directory mode changed to %o", info.Mode().Perm())
	}
	entries, err := os.ReadDir(backupDir)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 0 {
		t.Fatalf("permissive backup directory was modified: %#v", entries)
	}
}

func TestBackupDokployDatabaseRejectsSymlinkDirectory(t *testing.T) {
	root := dokployBackupTestDir(t)
	target := filepath.Join(root, "target")
	if err := os.Mkdir(target, 0o755); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(root, "backups")
	if err := os.Symlink(target, link); err != nil {
		t.Skipf("create directory symlink: %v", err)
	}
	if _, err := backupDokployDatabase(context.Background(), &fakeDockerRunner{}, "dokploy-postgres", link, "cleanup"); err == nil {
		t.Fatal("expected symlinked backup directory to be rejected")
	}
	entries, err := os.ReadDir(target)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 0 {
		t.Fatalf("symlink target was modified: %#v", entries)
	}
}

type closingBackupRunner struct{}

func (closingBackupRunner) Output(context.Context, ...string) ([]byte, error) {
	return nil, errors.New("unexpected output call")
}

func (closingBackupRunner) Run(_ context.Context, _ io.Reader, stdout io.Writer, _ ...string) error {
	file, ok := stdout.(*os.File)
	if !ok {
		return errors.New("backup output is not a file")
	}
	if _, err := file.WriteString("incomplete"); err != nil {
		return err
	}
	return file.Close()
}

func TestBackupDokployDatabaseRemovesFileAfterFinalizationFailure(t *testing.T) {
	backupDir := dokployBackupTestDir(t)
	if _, err := backupDokployDatabase(context.Background(), closingBackupRunner{}, "dokploy-postgres", backupDir, "cleanup"); err == nil {
		t.Fatal("expected backup finalization to fail")
	}
	entries, err := os.ReadDir(backupDir)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 0 {
		t.Fatalf("incomplete backup remained visible: %#v", entries)
	}
}

func dokployBackupTestDir(t *testing.T) string {
	t.Helper()
	cacheDir, err := os.UserCacheDir()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(cacheDir, 0o700); err != nil {
		t.Fatal(err)
	}
	cacheDir, err = filepath.EvalSymlinks(cacheDir)
	if err != nil {
		t.Fatal(err)
	}
	dir, err := os.MkdirTemp(cacheDir, "bort-dokploy-cleanup-test-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := os.RemoveAll(dir); err != nil {
			t.Errorf("remove Dokploy backup test directory: %v", err)
		}
	})
	if err := os.Chmod(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	return dir
}

func TestPurgeSourceResourcesRemovesDockerResources(t *testing.T) {
	fullNetworkID := "123456789abc" + strings.Repeat("d", 52)
	runner := &fakeDockerRunner{outputs: map[string][]byte{
		"inspect --type container cid123": []byte(`[{"Id":"cid123","Name":"/web","Config":{"Labels":{"coolify.managed":"true"}},"State":{"Running":false,"Status":"exited"}}]`),
		"rm -f cid123":                    []byte("cid123\n"),
		"network inspect api-net":         []byte(`[{"Id":"` + fullNetworkID + `","Name":"api-net"}]`),
		"network rm " + fullNetworkID:     []byte(fullNetworkID + "\n"),
	}}
	client := &Client{Docker: runner}

	options, err := client.IdentifySourcePurgeResources(context.Background(), SourcePurgeOptions{
		Containers: []SourcePurgeContainer{{App: "api", Service: "web", ContainerID: "cid123", ContainerName: "web"}},
		Networks:   []SourcePurgeNetwork{{App: "api", Name: "api-net", DiscoveredIdentity: fullNetworkID}},
	})
	if err != nil {
		t.Fatalf("IdentifySourcePurgeResources: %v", err)
	}
	result, err := client.PurgeSourceResources(context.Background(), options)
	if err != nil {
		t.Fatalf("PurgeSourceResources: %v", err)
	}
	if len(result.Containers) != 1 || result.Containers[0].Status != "removed" || result.Containers[0].Ref != "web" {
		t.Fatalf("unexpected container result: %#v", result.Containers)
	}
	if len(result.Networks) != 1 || result.Networks[0].Status != "removed" {
		t.Fatalf("unexpected network result: %#v", result.Networks)
	}
	if result.Networks[0].Identity != fullNetworkID {
		t.Fatalf("expected canonical network identity in result, got %#v", result.Networks[0])
	}
	gotArgs := []string{}
	for _, args := range runner.outputArgs {
		gotArgs = append(gotArgs, strings.Join(args, " "))
	}
	for _, want := range []string{"inspect --type container cid123", "rm -f cid123", "network inspect api-net", "network rm " + fullNetworkID} {
		found := false
		for _, got := range gotArgs {
			if got == want {
				found = true
				break
			}
		}
		if !found {
			t.Fatalf("expected docker command %q in %#v", want, gotArgs)
		}
	}
}

func TestIdentifySourcePurgeResourcesRejectsNameOnlyDuplicate(t *testing.T) {
	client := &Client{Docker: &fakeDockerRunner{}}
	_, err := client.IdentifySourcePurgeResources(context.Background(), SourcePurgeOptions{Containers: []SourcePurgeContainer{
		{ContainerID: "cid123", ContainerName: "web"},
		{ContainerName: "web"},
	}})
	if err == nil || !strings.Contains(err.Error(), "without a stable container ID") {
		t.Fatalf("expected name-only coordinate to remain unresolved, got %v", err)
	}
}

func TestIdentifySourcePurgeResourcesRejectsConflictingDuplicateContainerID(t *testing.T) {
	client := &Client{Docker: &fakeDockerRunner{}}
	for _, containers := range [][]SourcePurgeContainer{
		{{Service: "web", ContainerID: "cid123", ContainerName: "web"}, {Service: "web", ContainerID: "cid123", ContainerName: "replacement"}},
		{{Service: "web", ContainerID: "cid123", ContainerName: "web"}, {Service: "worker", ContainerID: "cid123", ContainerName: "web"}},
	} {
		if _, err := client.IdentifySourcePurgeResources(context.Background(), SourcePurgeOptions{Containers: containers}); err == nil || !strings.Contains(err.Error(), "conflicting") {
			t.Fatalf("expected duplicate container ID conflict, got %v", err)
		}
	}
}

func TestIdentifySourcePurgeResourcesCanonicalizesReviewedShortContainerID(t *testing.T) {
	shortID := "123456789abc"
	fullID := shortID + strings.Repeat("d", 52)
	runner := &fakeDockerRunner{outputs: map[string][]byte{
		"inspect --type container " + shortID: []byte(`[{"Id":"` + fullID + `","Name":"/web","State":{"Running":false,"Status":"exited"}}]`),
	}}
	client := &Client{Docker: runner}
	identified, err := client.IdentifySourcePurgeResources(context.Background(), SourcePurgeOptions{
		Containers: []SourcePurgeContainer{{ContainerID: shortID, ContainerName: "web"}},
	})
	if err != nil {
		t.Fatalf("IdentifySourcePurgeResources: %v", err)
	}
	if len(identified.Containers) != 1 || identified.Containers[0].ContainerID != fullID {
		t.Fatalf("expected canonical reviewed container ID %q, got %#v", fullID, identified.Containers)
	}
}

func TestCleanupSourcePurgeContainersDeduplicatesShortAndCanonicalIDs(t *testing.T) {
	shortID := "123456789abc"
	fullID := shortID + strings.Repeat("d", 52)
	for _, ids := range [][2]string{{shortID, fullID}, {fullID, shortID}} {
		containers, err := cleanupSourcePurgeContainers([]SourcePurgeContainer{
			{Service: "web", ContainerID: ids[0], ContainerName: "web"},
			{Service: "web", ContainerID: ids[1], ContainerName: "web"},
		})
		if err != nil {
			t.Fatal(err)
		}
		if len(containers) != 1 || containers[0].ContainerID != fullID {
			t.Fatalf("expected one canonical container coordinate for order %#v, got %#v", ids, containers)
		}
	}
}

func TestCleanupSourcePurgeNetworksRejectsConflictingIdentityState(t *testing.T) {
	for name, networks := range map[string][]SourcePurgeNetwork{
		"discovered identity": {
			{Name: "api-net", DiscoveredIdentity: strings.Repeat("a", 64)},
			{Name: "api-net", DiscoveredIdentity: strings.Repeat("b", 64)},
		},
		"expected identity": {
			{Name: "api-net", ExpectedIdentity: strings.Repeat("a", 64)},
			{Name: "api-net", ExpectedIdentity: strings.Repeat("b", 64)},
		},
		"expected absence": {
			{Name: "api-net", ExpectedAbsent: true},
			{Name: "api-net"},
		},
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := cleanupSourcePurgeNetworks(networks); err == nil || !strings.Contains(err.Error(), "conflicting") {
				t.Fatalf("expected conflicting duplicate networks to fail closed, got %v", err)
			}
		})
	}
	legacyID := "123456789abc"
	networks, err := cleanupSourcePurgeNetworks([]SourcePurgeNetwork{
		{App: "api", Name: "api-net", DiscoveredIdentity: legacyID},
		{App: "worker", Name: "api-net", DiscoveredIdentity: legacyID},
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(networks) != 1 || networks[0].DiscoveredIdentity != legacyID {
		t.Fatalf("identical legacy network IDs were not deduplicated: %#v", networks)
	}
	fullID := "123456789abc" + strings.Repeat("d", 52)
	networks, err = cleanupSourcePurgeNetworks([]SourcePurgeNetwork{
		{App: "api", Name: "api-net", DiscoveredIdentity: fullID, ExpectedIdentity: fullID},
		{App: "worker", Name: "api-net", DiscoveredIdentity: fullID, ExpectedIdentity: fullID},
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(networks) != 1 || networks[0].DiscoveredIdentity != fullID || networks[0].ExpectedIdentity != fullID {
		t.Fatalf("equivalent duplicate network IDs were not canonicalized: %#v", networks)
	}
}

func TestPurgeSourceResourcesSkipsAbsentAllowedPath(t *testing.T) {
	client := &Client{Docker: &fakeDockerRunner{}}
	options, err := client.IdentifySourcePurgeResources(context.Background(), SourcePurgeOptions{Paths: []SourcePurgePath{{Path: "/data/coolify/applications/app-1"}}})
	if err != nil {
		t.Fatalf("IdentifySourcePurgeResources: %v", err)
	}
	result, err := client.PurgeSourceResources(context.Background(), options)
	if err != nil {
		t.Fatalf("PurgeSourceResources: %v", err)
	}
	if len(result.Paths) != 1 || result.Paths[0].Status != "skipped" || !strings.Contains(result.Paths[0].Message, "remains absent") {
		t.Fatalf("unexpected path result: %#v", result.Paths)
	}
}

func TestPurgeSourceResourcesDoesNotFallbackFromStaleIDToName(t *testing.T) {
	runner := &fakeDockerRunner{outputs: map[string][]byte{
		"inspect --type container web": []byte(`[{"Id":"new-id","Name":"/web","State":{"Running":true,"Status":"running"}}]`),
		"rm -f new-id":                 []byte("new-id\n"),
	}}
	client := &Client{Docker: runner}

	result, err := client.PurgeSourceResources(context.Background(), SourcePurgeOptions{Containers: []SourcePurgeContainer{{App: "api", ContainerID: "missing-id", ContainerName: "web"}}})
	if err == nil || !strings.Contains(err.Error(), "docker inspect missing-id") {
		t.Fatalf("expected stale ID inspect error, got err=%v result=%#v", err, result)
	}
	for _, args := range runner.outputArgs {
		if strings.Join(args, " ") == "inspect --type container web" || strings.Join(args, " ") == "rm -f new-id" {
			t.Fatalf("purge fell back to name after stale ID: %#v", runner.outputArgs)
		}
	}
}

func TestPurgeSourceResourcesPreservesReplacementAfterReviewedContainerDisappears(t *testing.T) {
	runner := &cleanupDockerRunner{
		fakeDockerRunner: &fakeDockerRunner{outputs: map[string][]byte{
			"inspect --type container web": []byte(`[{"Id":"replacement-id","Name":"/web","State":{"Running":true,"Status":"running"}}]`),
		}},
		outputErrors: map[string]error{
			"inspect --type container reviewed-id": errors.New("Error: No such container: reviewed-id"),
		},
	}
	client := &Client{Docker: runner}
	result, err := client.PurgeSourceResources(context.Background(), SourcePurgeOptions{Containers: []SourcePurgeContainer{{ContainerID: "reviewed-id", ContainerName: "web"}}})
	if err == nil || !strings.Contains(err.Error(), "now refers to container ID") {
		t.Fatalf("expected replacement container to block purge, got result=%#v err=%v", result, err)
	}
	if len(result.Containers) != 1 || !strings.Contains(result.Containers[0].Message, "now refers to container ID") {
		t.Fatalf("expected persisted resource result to include the operation error, got %#v", result.Containers)
	}
	for _, args := range runner.outputArgs {
		if strings.HasPrefix(strings.Join(args, " "), "rm -f ") {
			t.Fatalf("replacement container was removed: %#v", runner.outputArgs)
		}
	}
}

func TestPurgeSourceResourcesRejectsMismatchedInspectedContainerID(t *testing.T) {
	runner := &fakeDockerRunner{outputs: map[string][]byte{
		"inspect --type container reviewed-id": []byte(`[{"Id":"replacement-id","Name":"/web","State":{"Running":true,"Status":"running"}}]`),
		"rm -f replacement-id":                 []byte("replacement-id\n"),
	}}
	client := &Client{Docker: runner}
	result, err := client.PurgeSourceResources(context.Background(), SourcePurgeOptions{Containers: []SourcePurgeContainer{{ContainerID: "reviewed-id", ContainerName: "web"}}})
	if err == nil || !strings.Contains(err.Error(), "not the reviewed ID") {
		t.Fatalf("expected mismatched inspected ID to block purge, got result=%#v err=%v", result, err)
	}
	for _, args := range runner.outputArgs {
		if strings.HasPrefix(strings.Join(args, " "), "rm -f ") {
			t.Fatalf("mismatched inspected container was removed: %#v", runner.outputArgs)
		}
	}
}

func TestPurgeSourceNetworkPreservesReplacementAfterReviewedIDDisappears(t *testing.T) {
	reviewedID := "aaaaaaaaaaaa" + strings.Repeat("a", 52)
	replacementID := strings.Repeat("b", 64)
	runner := &cleanupDockerRunner{
		fakeDockerRunner: &fakeDockerRunner{outputs: map[string][]byte{
			"network inspect api-net": []byte(`[{"Id":"` + replacementID + `","Name":"api-net"}]`),
		}},
		outputErrors: map[string]error{
			"network rm " + reviewedID: errors.New("Error response from daemon: network not found"),
		},
	}
	client := &Client{Docker: runner}
	result, err := client.PurgeSourceResources(context.Background(), SourcePurgeOptions{
		Networks: []SourcePurgeNetwork{{Name: "api-net", DiscoveredIdentity: reviewedID, ExpectedIdentity: reviewedID}},
	})
	if err == nil || !strings.Contains(err.Error(), "appeared after confirmation") {
		t.Fatalf("expected replacement network to be preserved, got result=%#v err=%v", result, err)
	}
	for _, args := range runner.outputArgs {
		if strings.Join(args, " ") == "network rm "+replacementID {
			t.Fatalf("replacement network was removed: %#v", runner.outputArgs)
		}
	}
}

func TestIdentifySourcePurgeResourcesRejectsNetworkReplacementSinceDiscovery(t *testing.T) {
	discoveredID := "aaaaaaaaaaaa" + strings.Repeat("a", 52)
	replacementID := "aaaaaaaaaaaa" + strings.Repeat("b", 52)
	runner := &fakeDockerRunner{outputs: map[string][]byte{
		"network inspect api-net": []byte(`[{"Id":"` + replacementID + `","Name":"api-net"}]`),
	}}
	client := &Client{Docker: runner}
	_, err := client.IdentifySourcePurgeResources(context.Background(), SourcePurgeOptions{
		Networks: []SourcePurgeNetwork{{Name: "api-net", DiscoveredIdentity: discoveredID}},
	})
	if err == nil || !strings.Contains(err.Error(), "does not match discovered ID") {
		t.Fatalf("expected replacement network to be rejected, got %v", err)
	}
	for _, args := range runner.outputArgs {
		if len(args) > 1 && args[0] == "network" && args[1] == "rm" {
			t.Fatalf("replacement network was removed: %#v", runner.outputArgs)
		}
	}
}

func TestIdentifySourcePurgeResourcesRequiresDiscoveredNetworkIdentity(t *testing.T) {
	runner := &fakeDockerRunner{outputs: map[string][]byte{
		"network inspect api-net": []byte(`[{"Id":"` + strings.Repeat("a", 64) + `","Name":"api-net"}]`),
	}}
	client := &Client{Docker: runner}
	for name, test := range map[string]struct {
		identity string
		want     string
	}{
		"missing": {want: "has no stable ID from discovery"},
		"legacy short": {
			identity: "aaaaaaaaaaaa",
			want:     "has non-canonical discovered ID",
		},
	} {
		t.Run(name, func(t *testing.T) {
			_, err := client.IdentifySourcePurgeResources(context.Background(), SourcePurgeOptions{
				Networks: []SourcePurgeNetwork{{Name: "api-net", DiscoveredIdentity: test.identity}},
			})
			if err == nil || !strings.Contains(err.Error(), test.want) || !strings.Contains(err.Error(), "remove it manually") {
				t.Fatalf("expected discovered identity to require manual removal, got %v", err)
			}
		})
	}
}

func TestPurgeSourceNetworkRejectsShortConfirmedIdentity(t *testing.T) {
	client := &Client{Docker: &fakeDockerRunner{}}
	progressCalls := 0
	result, err := client.PurgeSourceResources(context.Background(), SourcePurgeOptions{
		Networks: []SourcePurgeNetwork{{Name: "api-net", DiscoveredIdentity: "123456789abc", ExpectedIdentity: "123456789abc"}},
		OnProgress: func(SourcePurgeResult) error {
			progressCalls++
			return nil
		},
	})
	if err == nil || !strings.Contains(err.Error(), "non-canonical confirmed ID") {
		t.Fatalf("expected short confirmed identity to block mutation, got result=%#v err=%v", result, err)
	}
	if progressCalls != 0 {
		t.Fatalf("invalid confirmed identity was published before rejection: %d progress calls", progressCalls)
	}
}

func TestPurgeSourceNetworkRejectsInvalidIdentityTupleBeforeProgress(t *testing.T) {
	confirmedID := strings.Repeat("a", 64)
	for name, network := range map[string]SourcePurgeNetwork{
		"missing confirmed":  {Name: "api-net", DiscoveredIdentity: confirmedID},
		"missing discovered": {Name: "api-net", ExpectedIdentity: confirmedID},
		"invalid discovered": {Name: "api-net", DiscoveredIdentity: "abcdef", ExpectedIdentity: confirmedID},
		"short discovered":   {Name: "api-net", DiscoveredIdentity: confirmedID[:12], ExpectedIdentity: confirmedID},
		"mismatched IDs":     {Name: "api-net", DiscoveredIdentity: strings.Repeat("b", 64), ExpectedIdentity: confirmedID},
	} {
		t.Run(name, func(t *testing.T) {
			runner := &fakeDockerRunner{}
			progressCalls := 0
			result, err := (&Client{Docker: runner}).PurgeSourceResources(context.Background(), SourcePurgeOptions{
				Networks: []SourcePurgeNetwork{network},
				OnProgress: func(SourcePurgeResult) error {
					progressCalls++
					return nil
				},
			})
			if err == nil {
				t.Fatalf("expected invalid identity tuple to fail, got %#v", result)
			}
			if progressCalls != 0 || len(runner.outputArgs) != 0 {
				t.Fatalf("invalid identity tuple reached progress or Docker: progress=%d args=%#v", progressCalls, runner.outputArgs)
			}
		})
	}
}

func TestPurgeSourceResourcesReturnsPartialResults(t *testing.T) {
	canonicalID := "aaaaaaaaaaaa" + strings.Repeat("a", 52)
	runner := &fakeDockerRunner{outputs: map[string][]byte{
		"inspect --type container cid123": []byte(`[{"Id":"cid123","Name":"/web","State":{"Running":false,"Status":"exited"}}]`),
		"rm -f cid123":                    []byte("cid123\n"),
		"network inspect api-net":         []byte(`[{"Id":"` + canonicalID + `","Name":"api-net"}]`),
	}}
	client := &Client{Docker: runner}
	options, err := client.IdentifySourcePurgeResources(context.Background(), SourcePurgeOptions{
		Containers: []SourcePurgeContainer{{App: "api", ContainerID: "cid123", ContainerName: "web"}},
		Networks:   []SourcePurgeNetwork{{App: "api", Name: "api-net", DiscoveredIdentity: canonicalID}},
	})
	if err != nil {
		t.Fatalf("IdentifySourcePurgeResources: %v", err)
	}
	result, err := client.PurgeSourceResources(context.Background(), options)
	if err == nil {
		t.Fatal("expected purge to stop on the network failure")
	}
	if len(result.Containers) != 1 || result.Containers[0].Status != "removed" || len(result.Networks) != 1 || result.Networks[0].Status != "error" {
		t.Fatalf("expected completed and failed resource results, got %#v", result)
	}
}

func TestSourcePurgeRequiresNamedVolumeToRemainAbsent(t *testing.T) {
	runner := &fakeDockerRunner{outputs: map[string][]byte{
		"volume inspect api-data": []byte(`[{"Name":"api-data"}]`),
	}}
	client := &Client{Docker: runner}
	if _, err := client.IdentifySourcePurgeResources(context.Background(), SourcePurgeOptions{Volumes: []SourcePurgeVolume{{Name: "api-data"}}}); err == nil || !strings.Contains(err.Error(), "remove it manually") {
		t.Fatalf("expected existing named volume to block before confirmation, got %v", err)
	}
	result, err := client.PurgeSourceResources(context.Background(), SourcePurgeOptions{
		Volumes: []SourcePurgeVolume{{Name: "api-data", ExpectedAbsent: true}},
	})
	if err == nil || !strings.Contains(err.Error(), "appeared after confirmation") {
		t.Fatalf("expected recreated volume to remain preserved, got result=%#v err=%v", result, err)
	}
	for _, args := range runner.outputArgs {
		if strings.Join(args, " ") == "volume rm api-data" {
			t.Fatalf("changed volume was removed: %#v", runner.outputArgs)
		}
	}
}

func TestSourcePurgeRechecksAbsenceBeforeRemovingContainers(t *testing.T) {
	runner := &fakeDockerRunner{outputs: map[string][]byte{
		"volume inspect api-data":         []byte(`[{"Name":"api-data"}]`),
		"inspect --type container cid123": []byte(`[{"Id":"cid123","Name":"/web","State":{"Running":false,"Status":"exited"}}]`),
		"rm -f cid123":                    []byte("cid123\n"),
	}}
	client := &Client{Docker: runner}
	result, err := client.PurgeSourceResources(context.Background(), SourcePurgeOptions{
		Containers: []SourcePurgeContainer{{ContainerID: "cid123", ContainerName: "web"}},
		Volumes:    []SourcePurgeVolume{{Name: "api-data", ExpectedAbsent: true}},
	})
	if err == nil || !strings.Contains(err.Error(), "appeared after confirmation") {
		t.Fatalf("expected recreated volume to block before container removal, got result=%#v err=%v", result, err)
	}
	for _, args := range runner.outputArgs {
		if strings.Join(args, " ") == "rm -f cid123" {
			t.Fatalf("container was removed before prerequisites passed: %#v", runner.outputArgs)
		}
	}
}

func TestSourcePurgeWithAbsencePrerequisitesRequiresManualContainerRemoval(t *testing.T) {
	runner := &stagedPurgeRunner{fakeDockerRunner: fakeDockerRunner{outputs: map[string][]byte{
		"inspect --type container cid123": []byte(`[{"Id":"cid123","Name":"/web","State":{"Running":false,"Status":"exited"}}]`),
		"rm -f cid123":                    []byte("cid123\n"),
	}}}
	client := &Client{Docker: runner}
	result, err := client.PurgeSourceResources(context.Background(), SourcePurgeOptions{
		Containers: []SourcePurgeContainer{{ContainerID: "cid123", ContainerName: "web"}},
		Volumes:    []SourcePurgeVolume{{Name: "api-data", ExpectedAbsent: true}},
	})
	if err == nil || !strings.Contains(err.Error(), "remove it manually") {
		t.Fatalf("expected manual-completion strategy to block automatic removal, got result=%#v err=%v", result, err)
	}
	for _, args := range runner.outputArgs {
		if strings.Join(args, " ") == "rm -f cid123" {
			t.Fatalf("container was removed after a prerequisite reappeared: %#v", runner.outputArgs)
		}
	}
}

func TestIdentifySourcePurgeResourcesRejectsMixedAutomaticAndAbsenceOnlyPurge(t *testing.T) {
	shortID := "123456789abc"
	fullID := shortID + strings.Repeat("d", 52)
	runner := &cleanupDockerRunner{
		fakeDockerRunner: &fakeDockerRunner{outputs: map[string][]byte{
			"inspect --type container " + shortID: []byte(`[{"Id":"` + fullID + `","Name":"/web","State":{"Running":false,"Status":"exited"}}]`),
		}},
		outputErrors: map[string]error{
			"volume inspect api-data": errors.New("Error response from daemon: get api-data: no such volume"),
		},
	}
	client := &Client{Docker: runner}
	_, err := client.IdentifySourcePurgeResources(context.Background(), SourcePurgeOptions{
		Containers: []SourcePurgeContainer{{ContainerID: shortID, ContainerName: "web"}},
		Volumes:    []SourcePurgeVolume{{Name: "api-data"}},
	})
	if err == nil || !strings.Contains(err.Error(), "remove all listed containers and networks manually") {
		t.Fatalf("expected mixed automatic and absence-only purge to fail before confirmation, got %v", err)
	}
	if strings.Contains(err.Error(), "not the reviewed ID") {
		t.Fatalf("short reviewed ID was treated as a different container: %v", err)
	}
	for _, args := range runner.outputArgs {
		if strings.HasPrefix(strings.Join(args, " "), "rm -f ") {
			t.Fatalf("container was removed while identifying an ineligible purge: %#v", runner.outputArgs)
		}
	}
}

func TestSourcePurgeManualCompletionPreservesReplacementContainerByName(t *testing.T) {
	runner := &cleanupDockerRunner{
		fakeDockerRunner: &fakeDockerRunner{outputs: map[string][]byte{
			"inspect --type container web": []byte(`[{"Id":"replacement-id","Name":"/web","State":{"Running":true,"Status":"running"}}]`),
		}},
		outputErrors: map[string]error{
			"volume inspect api-data":           errors.New("Error response from daemon: get api-data: no such volume"),
			"inspect --type container reviewed": errors.New("Error: No such container: reviewed"),
		},
	}
	client := &Client{Docker: runner}
	result, err := client.PurgeSourceResources(context.Background(), SourcePurgeOptions{
		Containers: []SourcePurgeContainer{{ContainerID: "reviewed", ContainerName: "web"}},
		Volumes:    []SourcePurgeVolume{{Name: "api-data", ExpectedAbsent: true}},
	})
	if err == nil || !strings.Contains(err.Error(), "now refers to container ID") {
		t.Fatalf("expected replacement container to block absence verification, got result=%#v err=%v", result, err)
	}
	for _, args := range runner.outputArgs {
		if strings.HasPrefix(strings.Join(args, " "), "rm -f ") {
			t.Fatalf("replacement container was removed: %#v", runner.outputArgs)
		}
	}
}

func TestPurgeSourceResourcesStopsBeforeNextMutationWhenProgressPersistenceFails(t *testing.T) {
	runner := &fakeDockerRunner{outputs: map[string][]byte{
		"inspect --type container cid1": []byte(`[{"Id":"cid1","Name":"/one","State":{"Running":false,"Status":"exited"}}]`),
		"rm -f cid1":                    []byte("cid1\n"),
		"inspect --type container cid2": []byte(`[{"Id":"cid2","Name":"/two","State":{"Running":false,"Status":"exited"}}]`),
		"rm -f cid2":                    []byte("cid2\n"),
	}}
	var durable SourcePurgeResult
	client := &Client{Docker: runner}
	result, err := client.PurgeSourceResources(context.Background(), SourcePurgeOptions{
		Containers: []SourcePurgeContainer{{ContainerID: "cid1", ContainerName: "one"}, {ContainerID: "cid2", ContainerName: "two"}},
		OnProgress: func(progress SourcePurgeResult) error {
			latest := progress.Containers[len(progress.Containers)-1]
			if latest.Ref == "cid2" && latest.Status == "started" {
				return errors.New("backup unavailable")
			}
			durable = progress
			return nil
		},
	})
	if err == nil || !strings.Contains(err.Error(), "persist source purge progress") {
		t.Fatalf("expected progress persistence failure, got result=%#v err=%v", result, err)
	}
	if len(durable.Containers) != 1 || durable.Containers[0].Status != "removed" {
		t.Fatalf("expected first outcome to remain durable, got %#v", durable)
	}
	for _, args := range runner.outputArgs {
		if strings.Join(args, " ") == "inspect --type container cid2" || strings.Join(args, " ") == "rm -f cid2" {
			t.Fatalf("second resource mutated after persistence failure: %#v", runner.outputArgs)
		}
	}
}

type stagedPurgeRunner struct {
	fakeDockerRunner
	volumeInspects int
}

func (r *stagedPurgeRunner) Output(ctx context.Context, args ...string) ([]byte, error) {
	if strings.Join(args, " ") == "volume inspect api-data" {
		r.outputArgs = append(r.outputArgs, append([]string{}, args...))
		r.volumeInspects++
		if r.volumeInspects == 1 {
			return nil, errors.New("Error response from daemon: get api-data: no such volume")
		}
		return []byte(`[{"Name":"api-data"}]`), nil
	}
	return r.fakeDockerRunner.Output(ctx, args...)
}

func TestSourcePurgeAcceptsNamedVolumeThatRemainsAbsent(t *testing.T) {
	runner := &cleanupDockerRunner{
		fakeDockerRunner: &fakeDockerRunner{},
		outputErrors: map[string]error{
			"volume inspect api-data": errors.New("Error response from daemon: get api-data: no such volume"),
		},
	}
	client := &Client{Docker: runner}
	options, err := client.IdentifySourcePurgeResources(context.Background(), SourcePurgeOptions{
		Volumes: []SourcePurgeVolume{{Name: "api-data"}},
	})
	if err != nil {
		t.Fatalf("IdentifySourcePurgeResources: %v", err)
	}
	result, err := client.PurgeSourceResources(context.Background(), options)
	if err != nil {
		t.Fatalf("PurgeSourceResources: %v", err)
	}
	if len(result.Volumes) != 1 || result.Volumes[0].Status != "skipped" || !strings.Contains(result.Volumes[0].Message, "remains absent") {
		t.Fatalf("unexpected volume result: %#v", result.Volumes)
	}
}

func TestSourcePurgeManualNetworkResultRetainsConfirmedIdentity(t *testing.T) {
	canonicalID := strings.Repeat("a", 64)
	runner := &cleanupDockerRunner{
		fakeDockerRunner: &fakeDockerRunner{},
		outputErrors: map[string]error{
			"volume inspect api-data": errors.New("Error response from daemon: get api-data: no such volume"),
			"network inspect api-net": errors.New("Error response from daemon: network api-net not found"),
		},
	}
	client := &Client{Docker: runner}
	result, err := client.PurgeSourceResources(context.Background(), SourcePurgeOptions{
		Volumes:  []SourcePurgeVolume{{Name: "api-data", ExpectedAbsent: true}},
		Networks: []SourcePurgeNetwork{{Name: "api-net", DiscoveredIdentity: canonicalID, ExpectedIdentity: canonicalID}},
	})
	if err != nil {
		t.Fatalf("PurgeSourceResources: %v", err)
	}
	if len(result.Networks) != 1 || result.Networks[0].Status != "skipped" || result.Networks[0].Identity != canonicalID {
		t.Fatalf("confirmed network identity was lost from manual result: %#v", result.Networks)
	}
}

type cleanupDockerRunner struct {
	*fakeDockerRunner
	outputErrors map[string]error
}

func (r *cleanupDockerRunner) Output(ctx context.Context, args ...string) ([]byte, error) {
	if err, ok := r.outputErrors[strings.Join(args, " ")]; ok {
		return nil, err
	}
	return r.fakeDockerRunner.Output(ctx, args...)
}

func TestDockerMissingResourceClassifierRejectsInfrastructureErrors(t *testing.T) {
	if isDockerVolumeOrNetworkMissingErr(errors.New(`exec: "docker": executable file not found in $PATH`)) {
		t.Fatal("missing Docker executable was classified as an absent resource")
	}
	for _, err := range []error{
		errors.New("Error response from daemon: remove api-data: no such volume"),
		errors.New("Error response from daemon: network api-net not found"),
	} {
		if !isDockerVolumeOrNetworkMissingErr(err) {
			t.Fatalf("expected missing Docker resource classification for %v", err)
		}
	}
}

func TestProtectedSourcePurgeNetworkNamesAreCaseSensitive(t *testing.T) {
	for _, name := range []string{"bridge", "host", "none", "ingress"} {
		if !IsProtectedSourcePurgeNetwork(name) {
			t.Fatalf("expected %q to be protected", name)
		}
	}
	for _, name := range []string{"Bridge", "Proxy", "HOST", "coolify", "coolify-proxy", "proxy"} {
		if IsProtectedSourcePurgeNetwork(name) {
			t.Fatalf("expected distinct user network %q not to be protected", name)
		}
	}
}

func TestPurgeSourceResourcesRejectsProtectedHostPath(t *testing.T) {
	client := &Client{Docker: &fakeDockerRunner{}}
	_, err := client.PurgeSourceResources(context.Background(), SourcePurgeOptions{Paths: []SourcePurgePath{{Path: "/data"}}})
	if err == nil || !strings.Contains(err.Error(), "protected host path") {
		t.Fatalf("expected protected path error, got %v", err)
	}
}

func TestPathAbsentNoFollowRejectsAncestorSymlink(t *testing.T) {
	if os.PathSeparator == '\\' {
		t.Skip("windows refuses source path purge")
	}
	root := t.TempDir()
	outside := t.TempDir()
	outsideTarget := filepath.Join(outside, "target")
	if err := os.Mkdir(outsideTarget, 0o700); err != nil {
		t.Fatal(err)
	}
	sentinel := filepath.Join(outsideTarget, "sentinel")
	if err := os.WriteFile(sentinel, []byte("keep"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(root, "swapped")); err != nil {
		t.Fatal(err)
	}

	if _, err := pathAbsentNoFollow(filepath.Join(root, "swapped", "target")); err == nil {
		t.Fatal("expected ancestor symlink to block absence validation")
	}
	if contents, err := os.ReadFile(sentinel); err != nil || string(contents) != "keep" {
		t.Fatalf("outside path changed through ancestor symlink: contents=%q err=%v", contents, err)
	}
}

func TestPathAbsentNoFollowPreservesExistingDirectoryTree(t *testing.T) {
	if os.PathSeparator == '\\' {
		t.Skip("windows refuses source path purge")
	}
	root, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	target := filepath.Join(root, "target")
	if err := os.MkdirAll(filepath.Join(target, "nested"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(target, "nested", "file"), []byte("remove"), 0o600); err != nil {
		t.Fatal(err)
	}
	absent, err := pathAbsentNoFollow(target)
	if err != nil {
		t.Fatalf("check no-follow path absence: %v", err)
	}
	if absent {
		t.Fatal("expected existing target tree not to be reported absent")
	}
	if contents, err := os.ReadFile(filepath.Join(target, "nested", "file")); err != nil || string(contents) != "remove" {
		t.Fatalf("absence validation changed the target tree: contents=%q err=%v", contents, err)
	}
}

func TestFindDokployPostgresContainerRequiresDokployPostgres(t *testing.T) {
	runner := &fakeDockerRunner{outputs: map[string][]byte{"ps --format {{.Names}}": []byte("postgres\n")}}
	if _, err := findDokployPostgresContainer(context.Background(), runner); err == nil {
		t.Fatal("expected missing dokploy postgres to error")
	}
}
