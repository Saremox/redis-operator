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
}
