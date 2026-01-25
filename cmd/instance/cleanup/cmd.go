package cleanup

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/spf13/cobra"
)

const (
	defaultDataDir    = "/data"
	defaultDBFilename = "dump.rdb"
)

var (
	dataDir    string
	dbFilename string
	dryRun     bool
)

// NewCmd creates the cleanup command
func NewCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "cleanup",
		Short: "Clean up stale RDB tempfiles",
		Long: `Clean up stale RDB tempfiles before Redis starts.

During BGSAVE operations, Redis creates temporary files named temp-<pid>.rdb.
If Redis crashes during a BGSAVE, these files are left behind and can accumulate,
eventually filling the disk and causing further failures.

This command removes all .rdb files except the main database file (default: dump.rdb)
from the data directory.`,
		RunE: runCleanup,
	}

	cmd.Flags().StringVar(&dataDir, "data-dir", defaultDataDir, "Redis data directory")
	cmd.Flags().StringVar(&dbFilename, "db-filename", defaultDBFilename, "Main RDB filename to preserve")
	cmd.Flags().BoolVar(&dryRun, "dry-run", false, "Print files that would be deleted without deleting them")

	return cmd
}

func runCleanup(cmd *cobra.Command, args []string) error {
	// Validate data directory exists
	info, err := os.Stat(dataDir)
	if err != nil {
		if os.IsNotExist(err) {
			// Data directory doesn't exist yet, nothing to clean
			fmt.Printf("Data directory %s does not exist, skipping cleanup\n", dataDir)
			return nil
		}
		return fmt.Errorf("failed to stat data directory: %w", err)
	}
	if !info.IsDir() {
		return fmt.Errorf("%s is not a directory", dataDir)
	}

	// Find and remove stale RDB files
	entries, err := os.ReadDir(dataDir)
	if err != nil {
		return fmt.Errorf("failed to read data directory: %w", err)
	}

	var cleaned int
	var totalSize int64

	for _, entry := range entries {
		if entry.IsDir() {
			continue
		}

		name := entry.Name()

		// Skip non-RDB files
		if !strings.HasSuffix(name, ".rdb") {
			continue
		}

		// Preserve the main database file
		if name == dbFilename {
			continue
		}

		filePath := filepath.Join(dataDir, name)

		// Get file size for reporting
		fileInfo, err := entry.Info()
		if err != nil {
			fmt.Printf("Warning: failed to get info for %s: %v\n", name, err)
			continue
		}

		if dryRun {
			fmt.Printf("Would delete: %s (%d bytes)\n", filePath, fileInfo.Size())
		} else {
			if err := os.Remove(filePath); err != nil {
				fmt.Printf("Warning: failed to remove %s: %v\n", filePath, err)
				continue
			}
			fmt.Printf("Deleted: %s (%d bytes)\n", filePath, fileInfo.Size())
		}

		cleaned++
		totalSize += fileInfo.Size()
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
