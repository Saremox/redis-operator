// Package main provides the redis-instance binary, an instance manager that
// runs as PID 1 and starts redis-server as a child process. The operator
// image contains the binary. A Redis pod runs it only when the RedisFailover
// opts in: an init container then copies the binary onto a shared volume.
// Otherwise the pod runs redis-server from the Redis image.
package main

import (
	"fmt"
	"os"

	"github.com/spf13/cobra"

	"github.com/saremox/redis-operator/cmd/instance/cleanup"
	"github.com/saremox/redis-operator/cmd/instance/run"
	"github.com/saremox/redis-operator/version"
)

var rootCmd = &cobra.Command{
	Use:     "redis-instance",
	Version: version.Version,
	Short:   "Redis instance manager for redis-operator",
	Long: `The Redis instance manager runs as PID 1 in a container and starts
redis-server as a child process. It removes old RDB tempfiles before Redis
starts, serves health endpoints, forwards the stop signals and reaps zombie
processes.

A Redis pod runs this binary only when the RedisFailover opts in. Otherwise
the pod runs redis-server from the Redis image.`,
	SilenceUsage: true,
}

func init() {
	rootCmd.AddCommand(cleanup.NewCmd())
	rootCmd.AddCommand(run.NewCmd())
}

// exit is a variable so that tests can see the exit code without exiting.
var exit = os.Exit

func main() {
	if err := rootCmd.Execute(); err != nil {
		fmt.Fprintln(os.Stderr, err)
		exit(1)
	}
}
