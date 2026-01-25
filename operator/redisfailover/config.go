package redisfailover

// Config is the configuration for the redis operator.
type Config struct {
	ListenAddress            string
	MetricsPath              string
	Concurrency              int
	SyncInterval             int
	SupportedNamespacesRegex string
	// InstanceManagerImage is the image used for Redis instance management init containers.
	// This should be the same image as the operator, which contains the redis-instance binary.
	InstanceManagerImage string
}
