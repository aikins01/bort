package gateway

type Route struct {
	Host       string
	OldTarget  string
	NewTarget  string
	HealthPath string
}

type CutoverPlan struct {
	AppName               string
	Routes                []Route
	RollbackWindowSeconds int
}
