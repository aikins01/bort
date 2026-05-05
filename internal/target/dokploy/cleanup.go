package dokploy

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

type StalePlatformProject struct {
	ProjectID string `json:"projectId"`
	Name      string `json:"name"`
}

type StalePlatformCleanupOptions struct {
	ProjectNames []string
	ProjectIDs   map[string]string
	BackupDir    string
	BackupPrefix string
}

type StalePlatformCleanupResult struct {
	BackupPath string                 `json:"backupPath"`
	Deleted    []StalePlatformProject `json:"deleted"`
}

func (c *Client) CleanupStalePlatformProjects(ctx context.Context, opts StalePlatformCleanupOptions) (StalePlatformCleanupResult, error) {
	names := cleanupProjectNames(opts.ProjectNames)
	if len(names) == 0 {
		return StalePlatformCleanupResult{}, fmt.Errorf("at least one project name is required")
	}
	runner := c.dockerRunner()
	pg, err := findDokployPostgresContainer(ctx, runner)
	if err != nil {
		return StalePlatformCleanupResult{}, err
	}
	backupPath, err := backupDokployDatabase(ctx, runner, pg, opts.BackupDir, opts.BackupPrefix)
	if err != nil {
		return StalePlatformCleanupResult{}, err
	}
	deleted, err := deleteStalePlatformProjects(ctx, runner, pg, names, opts.ProjectIDs)
	if err != nil {
		return StalePlatformCleanupResult{}, fmt.Errorf("delete stale Dokploy platform metadata after backup %s: %w", backupPath, err)
	}
	return StalePlatformCleanupResult{BackupPath: backupPath, Deleted: deleted}, nil
}

func cleanupProjectNames(names []string) []string {
	seen := map[string]struct{}{}
	cleaned := []string{}
	for _, name := range names {
		name = strings.TrimSpace(name)
		if name == "" {
			continue
		}
		if _, ok := seen[name]; ok {
			continue
		}
		seen[name] = struct{}{}
		cleaned = append(cleaned, name)
	}
	sort.Strings(cleaned)
	return cleaned
}

func findDokployPostgresContainer(ctx context.Context, runner dockerRunner) (string, error) {
	out, err := runner.Output(ctx, "ps", "--format", "{{.Names}}")
	if err != nil {
		return "", fmt.Errorf("list docker containers for Dokploy postgres: %w", err)
	}
	for _, line := range strings.Split(string(out), "\n") {
		name := strings.TrimSpace(line)
		if name == "dokploy-postgres" || strings.HasPrefix(name, "dokploy-postgres.") || strings.HasPrefix(name, "dokploy-postgres-") {
			return name, nil
		}
	}
	return "", fmt.Errorf("dokploy postgres container was not found; cleanup must run on the Dokploy host")
}

func backupDokployDatabase(ctx context.Context, runner dockerRunner, pg, backupDir, backupPrefix string) (string, error) {
	backupDir = strings.TrimSpace(backupDir)
	if backupDir == "" {
		backupDir = filepath.Join(".bort", "backups")
	}
	if backupPrefix = strings.TrimSpace(backupPrefix); backupPrefix == "" {
		backupPrefix = "dokploy-cleanup"
	}
	if err := os.MkdirAll(backupDir, 0o700); err != nil {
		return "", err
	}
	if err := os.Chmod(backupDir, 0o700); err != nil {
		return "", err
	}
	path := filepath.Join(backupDir, fmt.Sprintf("%s-%s.sql", backupPrefix, time.Now().UTC().Format("20060102-150405.000000000")))
	file, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return "", err
	}
	if err := runner.Run(ctx, nil, file, "exec", pg, "pg_dump", "-U", "dokploy", "-d", "dokploy"); err != nil {
		_ = file.Close()
		return "", fmt.Errorf("backup dokploy database to %s: %w", path, err)
	}
	if err := file.Chmod(0o600); err != nil {
		_ = file.Close()
		return "", err
	}
	if err := file.Close(); err != nil {
		return "", fmt.Errorf("close dokploy database backup %s: %w", path, err)
	}
	return path, nil
}

func deleteStalePlatformProjects(ctx context.Context, runner dockerRunner, pg string, names []string, projectIDs map[string]string) ([]StalePlatformProject, error) {
	var out bytes.Buffer
	if err := runner.Run(ctx, strings.NewReader(stalePlatformCleanupSQL(names, projectIDs)), &out,
		"exec", "-i", pg, "psql", "-U", "dokploy", "-d", "dokploy", "-v", "ON_ERROR_STOP=1", "-At", "-F", "|"); err != nil {
		return nil, err
	}
	return parseDeletedStalePlatformProjects(out.String()), nil
}

func stalePlatformCleanupSQL(names []string, projectIDs map[string]string) string {
	values := make([]string, 0, len(names))
	for _, name := range names {
		id := "null"
		if projectID := strings.TrimSpace(projectIDs[name]); projectID != "" {
			id = sqlStringLiteral(projectID)
		}
		values = append(values, "("+sqlStringLiteral(name)+", "+id+")")
	}
	return fmt.Sprintf(`begin;
create temporary table bort_stale_platform_project(project_name text primary key, project_id text) on commit drop;
insert into bort_stale_platform_project(project_name, project_id) values
  %s;

do $$
declare
  bad text;
begin
  select string_agg(p.name || ' compose=' || coalesce(c.compose_count, 0) || ' domains=' || coalesce(d.domain_count, 0), ', ' order by p.name)
    into bad
  from project p
  join bort_stale_platform_project x on x.project_name = p.name
   and (x.project_id is null or x.project_id = p."projectId")
  left join lateral (
    select count(*) as compose_count
    from environment e
    join compose cmp on cmp."environmentId" = e."environmentId"
    where e."projectId" = p."projectId"
  ) c on true
  left join lateral (
    select count(*) as domain_count
    from environment e
    join compose cmp on cmp."environmentId" = e."environmentId"
    join domain dom on dom."composeId" = cmp."composeId"
    where e."projectId" = p."projectId"
  ) d on true
  where coalesce(c.compose_count, 0) <> 0 or coalesce(d.domain_count, 0) <> 0;
  if bad is not null then
    raise exception 'refusing to delete stale platform projects with attached Dokploy resources: %%', bad;
  end if;
end $$;

with deleted as (
  delete from project p
  using bort_stale_platform_project x
  where p.name = x.project_name
    and (x.project_id is null or x.project_id = p."projectId")
  returning p."projectId", p.name
)
select 'deleted', name, "projectId" from deleted order by name;
commit;
`, strings.Join(values, ",\n  "))
}

func sqlStringLiteral(value string) string {
	return "'" + strings.ReplaceAll(value, "'", "''") + "'"
}

func parseDeletedStalePlatformProjects(output string) []StalePlatformProject {
	deleted := []StalePlatformProject{}
	for _, line := range strings.Split(output, "\n") {
		parts := strings.Split(line, "|")
		if len(parts) != 3 || parts[0] != "deleted" {
			continue
		}
		deleted = append(deleted, StalePlatformProject{Name: parts[1], ProjectID: parts[2]})
	}
	return deleted
}
