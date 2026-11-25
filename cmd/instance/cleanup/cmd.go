package cleanup

import (
	"fmt"
	"os"

	"github.com/spf13/cobra"

	"github.com/saremox/redis-operator/cmd/instance/rdbclean"
)

const (
	defaultDataDir    = "/data"
	defaultDBFilename = "dump.rdb"
)

// readDir is a variable so that tests can inject directory read faults.
var readDir = os.ReadDir

var (
	dataDir    string
	dbFilename string
	dryRun     bool
)

func NewCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "cleanup",
		Short: "Remove old RDB tempfiles",
		Long: `Remove old RDB tempfiles before Redis starts.

A BGSAVE writes temp-<pid>.rdb. When Redis stops during a BGSAVE, the file
stays. The files collect until the disk is full, and then each BGSAVE fails.

This command removes all the .rdb files in the data directory, except the
--db-filename file (default dump.rdb).`,
		RunE: runCleanup,
	}

	cmd.Flags().StringVar(&dataDir, "data-dir", defaultDataDir, "Redis data directory")
	cmd.Flags().StringVar(&dbFilename, "db-filename", defaultDBFilename, "Main RDB filename to preserve")
	cmd.Flags().BoolVar(&dryRun, "dry-run", false, "Print files that would be deleted without deleting them")

	return cmd
}

func runCleanup(_ *cobra.Command, _ []string) error {
	missing := false
	cleaned, totalSize, err := rdbclean.Clean(dataDir, dbFilename, dryRun, readDir, func(e rdbclean.Event) {
		switch e.Kind {
		case rdbclean.Missing:
			missing = true
			fmt.Printf("Data directory %s does not exist, skipping cleanup\n", e.Path)
		case rdbclean.InfoFailed:
			fmt.Printf("Warning: failed to get info for %s: %v\n", e.Name, e.Err)
		case rdbclean.RemoveFailed:
			fmt.Printf("Warning: failed to remove %s: %v\n", e.Path, e.Err)
		case rdbclean.WouldRemove:
			fmt.Printf("Would delete: %s (%d bytes)\n", e.Path, e.Size)
		case rdbclean.Removed:
			fmt.Printf("Deleted: %s (%d bytes)\n", e.Path, e.Size)
		}
	})
	if err != nil || missing {
		return err
	}

	if cleaned > 0 {
		action := "Deleted"
		if dryRun {
			action = "Would delete"
		}
		fmt.Printf("%s %d stale RDB file(s), freed %d bytes\n", action, cleaned, totalSize)
	} else {
		fmt.Println("No stale RDB files found")
	}

	return nil
}
