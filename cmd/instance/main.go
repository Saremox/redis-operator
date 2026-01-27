// Package main provides the redis-instance binary, which serves as the instance
// manager for Redis pods managed by redis-operator.
//
// This follows the CloudNativePG (CNPG) model where the instance manager runs as
// PID 1 and manages the database process. This architecture has proven reliable
// at scale in production Kubernetes environments.
//
// See: https://cloudnative-pg.io/documentation/current/instance_manager/
package main

import (
	"fmt"
	"os"

	"github.com/spf13/cobra"

	"github.com/saremox/redis-operator/cmd/instance/cleanup"
	"github.com/saremox/redis-operator/cmd/instance/run"
)

var rootCmd = &cobra.Command{
	Use:   "redis-instance",
	Short: "Redis instance manager for redis-operator",
	Long: `Redis instance manager handles lifecycle operations for Redis instances
managed by redis-operator.

This tool follows the CloudNativePG (CNPG) model where the instance manager
runs as PID 1 in the container and manages the Redis process as a child.
This architecture provides:

  - Full lifecycle control over the Redis process
  - Clean signal handling and graceful shutdown
  - Startup tasks (RDB cleanup) before Redis starts
  - Foundation for health checks, metrics, and monitoring

See: https://cloudnative-pg.io/documentation/current/instance_manager/`,
	SilenceUsage: true,
}

func init() {
	rootCmd.AddCommand(cleanup.NewCmd())
	rootCmd.AddCommand(run.NewCmd())
}

func main() {
	if err := rootCmd.Execute(); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}
