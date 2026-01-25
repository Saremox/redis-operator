package utils

import (
	"flag"
	"fmt"
	"path/filepath"
	"regexp"

	"github.com/saremox/redis-operator/operator/redisfailover"
	"k8s.io/client-go/util/homedir"
)

// CMDFlags are the flags used by the cmd
// TODO: improve flags.
type CMDFlags struct {
	KubeConfig               string
	SupportedNamespacesRegex string
	Development              bool
	ListenAddr               string
	MetricsPath              string
	K8sQueriesPerSecond      int
	K8sQueriesBurstable      int
	Concurrency              int
	SyncInterval             int
	LogLevel                 string
	InstanceManagerImage     string
}

// Init initializes and parse the flags
func (c *CMDFlags) Init() {
	kubehome := filepath.Join(homedir.HomeDir(), ".kube", "config")
	// register flags
	flag.StringVar(&c.KubeConfig, "kubeconfig", kubehome, "kubernetes configuration path, only used when development mode enabled")
	flag.StringVar(&c.SupportedNamespacesRegex, "supported-namespaces-regex", ".*", "To limit the namespaces this operator looks into")
	flag.BoolVar(&c.Development, "development", false, "development flag will allow to run the operator outside a kubernetes cluster")
	flag.StringVar(&c.ListenAddr, "listen-address", ":9710", "Address to listen on for metrics.")
	flag.StringVar(&c.MetricsPath, "metrics-path", "/metrics", "Path to serve the metrics.")
	flag.IntVar(&c.K8sQueriesPerSecond, "k8s-cli-qps-limit", 100, "Number of allowed queries per second by kubernetes client without client side throttling")
	flag.IntVar(&c.K8sQueriesBurstable, "k8s-cli-burstable-limit", 100, "Number of allowed burst requests by kubernetes client without client side throttling")
	// default is 3 for conccurency because kooper also defines 3 as default
	// reference: https://github.com/spotahome/kooper/blob/master/controller/controller.go#L89
	flag.IntVar(&c.Concurrency, "concurrency", 3, "Number of conccurent workers meant to process events")
	flag.IntVar(&c.SyncInterval, "sync-interval", 30, "Number of seconds between checks")
	flag.StringVar(&c.LogLevel, "log-level", "info", "set log level")
	flag.StringVar(&c.InstanceManagerImage, "instance-manager-image", "", "Image containing the redis-instance binary for init containers (defaults to empty, which disables RDB cleanup)")
	// Parse flags
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
		InstanceManagerImage:     c.InstanceManagerImage,
	}
}
