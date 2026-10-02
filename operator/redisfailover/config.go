package redisfailover

// Config is the configuration for the redis operator.
type Config struct {
	ListenAddress            string
	MetricsPath              string
	Concurrency              int
	SyncInterval             int
	SupportedNamespacesRegex string
	// KeepClientsOnDemotion is negative so the zero Config disconnects.
	KeepClientsOnDemotion bool
	// WatchAuthSecrets applies an auth Secret change at once rather than on
	// the next resync. It needs list and watch on secrets.
	WatchAuthSecrets bool
}
