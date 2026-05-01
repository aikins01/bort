package state

type Phase string

const (
	PhaseDiscovered Phase = "discovered"
	PhasePrepared   Phase = "prepared"
	PhaseSyncing    Phase = "syncing"
	PhaseValidated  Phase = "validated"
	PhaseCutover    Phase = "cutover"
	PhaseWatching   Phase = "watching"
	PhaseCommitted  Phase = "committed"
	PhaseRolledBack Phase = "rolled_back"
)

type Migration struct {
	ID      string
	AppName string
	Phase   Phase
}
