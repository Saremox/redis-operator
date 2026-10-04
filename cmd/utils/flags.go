package utils

import (
	"flag"
	"fmt"
	"path/filepath"
	"regexp"

	"github.com/saremox/redis-operator/operator/redisfailover"
	"k8s.io/client-go/util/homedir"
)

// CMDFlags holds the command-line flags of the operator.
type CMDFlags struct {
	KubeConfig                  string
	SupportedNamespacesRegex    string
	Development                 bool
	ListenAddr                  string
	MetricsPath                 string
	K8sQueriesPerSecond         int
	K8sQueriesBurstable         int
	Concurrency                 int
	SyncInterval                int
	LogLevel                    string
	DisconnectClientsOnDemotion bool
	WatchAuthSecrets            bool
	EnablePprof                 bool
}

// Init initializes and parse the flags
func (c *CMDFlags) Init() {
	kubehome := filepath.Join(homedir.HomeDir(), ".kube", "config")
	// register flags
	flag.StringVar(&c.KubeConfig, "kubeconfig", kubehome, "kubernetes configuration path, only used when development mode enabled")
	flag.StringVar(&c.SupportedNamespacesRegex, "supported-namespaces-regex", ".*", "Reconcile only the RedisFailovers in a namespace that matches this regex. The match is not anchored, so use ^ and $ for an exact name. The operator still watches all namespaces")
	flag.BoolVar(&c.Development, "development", false, "development flag will allow to run the operator outside a kubernetes cluster")
	flag.StringVar(&c.ListenAddr, "listen-address", ":9710", "Address to listen on for metrics.")
	flag.StringVar(&c.MetricsPath, "metrics-path", "/metrics", "Path to serve the metrics.")
	flag.IntVar(&c.K8sQueriesPerSecond, "k8s-cli-qps-limit", 100, "Number of allowed queries per second by kubernetes client without client side throttling")
	flag.IntVar(&c.K8sQueriesBurstable, "k8s-cli-burstable-limit", 100, "Number of allowed burst requests by kubernetes client without client side throttling")
	// The controller also uses 3 for a concurrency of 0 or less.
	flag.IntVar(&c.Concurrency, "concurrency", 3, "Number of concurrent workers that reconcile the RedisFailovers")
	flag.IntVar(&c.SyncInterval, "sync-interval", 30, "Seconds between two periodic resyncs of all RedisFailovers. A change of a RedisFailover or its pods starts a reconcile at once. 0 or less means 180. Above 300, the metrics cleanup can delete series between two resyncs")
	flag.StringVar(&c.LogLevel, "log-level", "info", "set log level")
	flag.BoolVar(&c.DisconnectClientsOnDemotion, "disconnect-clients-on-demotion", true, "Close a redis pod's normal and pub/sub client connections when it stops being the master, so clients reconnect to the new master instead of staying on a replica")
	flag.BoolVar(&c.WatchAuthSecrets, "watch-auth-secrets", false, "Apply an auth Secret (spec.auth.secretPath) change immediately, not at the next sync. This needs list and watch on secrets in all namespaces, which can read every Secret")
	flag.BoolVar(&c.EnablePprof, "enable-pprof", false, "Serve the Go profiler on /debug/pprof/ at the listen address. The profiler has no authentication, so allow only trusted users to reach the listen address.")
	flag.Parse()

	if _, err := regexp.Compile(c.SupportedNamespacesRegex); err != nil {
		panic(fmt.Errorf("supported namespaces Regex is not valid: %w", err))
	}
}

// ToRedisOperatorConfig convert the flags to redisfailover config
func (c *CMDFlags) ToRedisOperatorConfig() redisfailover.Config {
	return redisfailover.Config{
		ListenAddress:            c.ListenAddr,
		MetricsPath:              c.MetricsPath,
		Concurrency:              c.Concurrency,
		SyncInterval:             c.SyncInterval,
		SupportedNamespacesRegex: c.SupportedNamespacesRegex,
		KeepClientsOnDemotion:    !c.DisconnectClientsOnDemotion,
		WatchAuthSecrets:         c.WatchAuthSecrets,
	}
}
