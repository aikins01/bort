package sync

type Strategy string

const (
	StrategyNone                Strategy = "none"
	StrategyDockerVolumeArchive Strategy = "docker_volume_archive"
	StrategyRsync               Strategy = "rsync"
	StrategyPostgresLogical     Strategy = "postgres_logical"
	StrategyMySQLBinlog         Strategy = "mysql_binlog"
	StrategyManual              Strategy = "manual"
)

type Plan struct {
	ResourceName  string
	Strategy      Strategy
	RequiresPause bool
}
