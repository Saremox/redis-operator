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

// readDir is a variable so that tests can inject directory read faults.
var readDir = os.ReadDir

var (
	dataDir    string
	dbFilename string
	dryRun     bool
)

// NewCmd creates the cleanup command.
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
	info, err := os.Stat(dataDir)
	if err != nil {
		if os.IsNotExist(err) {
			fmt.Printf("Data directory %s does not exist, skipping cleanup\n", dataDir)
			return nil
		}
		return fmt.Errorf("failed to stat data directory: %w", err)
	}
	if !info.IsDir() {
		return fmt.Errorf("%s is not a directory", dataDir)
	}

	entries, err := readDir(dataDir)
	if err != nil {
		return fmt.Errorf("failed to read data directory: %w", err)
	}

	var cleaned int
	var totalSize int64

	for _, entry := range entries {
		if size, ok := cleanEntry(entry); ok {
			cleaned++
			totalSize += size
		}
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

// cleanEntry removes the entry if it is a stale RDB file, or prints it in a
// dry run. It returns the size of the file, and whether the file counts as
// cleaned.
func cleanEntry(entry os.DirEntry) (int64, bool) {
	if entry.IsDir() {
		return 0, false
	}

	name := entry.Name()

	if !strings.HasSuffix(name, ".rdb") {
		return 0, false
	}

	if name == dbFilename {
		return 0, false
	}

	filePath := filepath.Join(dataDir, name)

	fileInfo, err := entry.Info()
	if err != nil {
		fmt.Printf("Warning: failed to get info for %s: %v\n", name, err)
		return 0, false
	}

	if dryRun {
		fmt.Printf("Would delete: %s (%d bytes)\n", filePath, fileInfo.Size())
	} else {
		if err := os.Remove(filePath); err != nil {
			fmt.Printf("Warning: failed to remove %s: %v\n", filePath, err)
			return 0, false
		}
		fmt.Printf("Deleted: %s (%d bytes)\n", filePath, fileInfo.Size())
	}

	return fileInfo.Size(), true
}
