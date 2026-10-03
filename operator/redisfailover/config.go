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
	// WatchAuthSecrets applies an auth Secret change immediately, not at the
	// next sync. It needs list and watch on secrets, which can read every Secret.
	WatchAuthSecrets bool
}
