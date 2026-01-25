package cleanup

import (
	"os"
	"path/filepath"
	"testing"
)

func TestRunCleanup(t *testing.T) {
	tests := []struct {
		name           string
		files          []string
		expectedKept   []string
		expectedRemove []string
		dbFilename     string
	}{
		{
			name:           "removes temp rdb files",
			files:          []string{"dump.rdb", "temp-1234.rdb", "temp-5678.rdb"},
			expectedKept:   []string{"dump.rdb"},
			expectedRemove: []string{"temp-1234.rdb", "temp-5678.rdb"},
			dbFilename:     "dump.rdb",
		},
		{
			name:           "preserves non-rdb files",
			files:          []string{"dump.rdb", "temp-1234.rdb", "appendonly.aof", "nodes.conf"},
			expectedKept:   []string{"dump.rdb", "appendonly.aof", "nodes.conf"},
			expectedRemove: []string{"temp-1234.rdb"},
			dbFilename:     "dump.rdb",
		},
		{
			name:           "handles custom db filename",
			files:          []string{"custom.rdb", "dump.rdb", "temp-1234.rdb"},
			expectedKept:   []string{"custom.rdb"},
			expectedRemove: []string{"dump.rdb", "temp-1234.rdb"},
			dbFilename:     "custom.rdb",
		},
		{
			name:           "handles empty directory",
			files:          []string{},
			expectedKept:   []string{},
			expectedRemove: []string{},
			dbFilename:     "dump.rdb",
		},
		{
			name:           "handles only main db file",
			files:          []string{"dump.rdb"},
			expectedKept:   []string{"dump.rdb"},
			expectedRemove: []string{},
			dbFilename:     "dump.rdb",
		},
		{
			name:           "removes all rdb variants except main",
			files:          []string{"dump.rdb", "backup.rdb", "old.rdb", "temp-123.rdb"},
			expectedKept:   []string{"dump.rdb"},
			expectedRemove: []string{"backup.rdb", "old.rdb", "temp-123.rdb"},
			dbFilename:     "dump.rdb",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			// Create temp directory
			tmpDir, err := os.MkdirTemp("", "redis-cleanup-test")
			if err != nil {
				t.Fatalf("failed to create temp dir: %v", err)
			}
			defer os.RemoveAll(tmpDir)

			// Create test files
			for _, f := range tt.files {
				filePath := filepath.Join(tmpDir, f)
				if err := os.WriteFile(filePath, []byte("test content"), 0644); err != nil {
					t.Fatalf("failed to create test file %s: %v", f, err)
				}
			}

			// Set package variables for the test
			dataDir = tmpDir
			dbFilename = tt.dbFilename
			dryRun = false

			// Run cleanup
			if err := runCleanup(nil, nil); err != nil {
				t.Fatalf("runCleanup failed: %v", err)
			}

			// Verify expected files are kept
			for _, f := range tt.expectedKept {
				filePath := filepath.Join(tmpDir, f)
				if _, err := os.Stat(filePath); os.IsNotExist(err) {
					t.Errorf("expected file %s to be kept, but it was removed", f)
				}
			}

			// Verify expected files are removed
			for _, f := range tt.expectedRemove {
				filePath := filepath.Join(tmpDir, f)
				if _, err := os.Stat(filePath); !os.IsNotExist(err) {
					t.Errorf("expected file %s to be removed, but it still exists", f)
				}
			}
		})
	}
}

func TestRunCleanupDryRun(t *testing.T) {
	// Create temp directory
	tmpDir, err := os.MkdirTemp("", "redis-cleanup-test")
	if err != nil {
		t.Fatalf("failed to create temp dir: %v", err)
	}
	defer os.RemoveAll(tmpDir)

	// Create test files
	files := []string{"dump.rdb", "temp-1234.rdb"}
	for _, f := range files {
		filePath := filepath.Join(tmpDir, f)
		if err := os.WriteFile(filePath, []byte("test content"), 0644); err != nil {
			t.Fatalf("failed to create test file %s: %v", f, err)
		}
	}

	// Set package variables for the test
	dataDir = tmpDir
	dbFilename = "dump.rdb"
	dryRun = true

	// Run cleanup
	if err := runCleanup(nil, nil); err != nil {
		t.Fatalf("runCleanup failed: %v", err)
	}

	// In dry-run mode, all files should still exist
	for _, f := range files {
		filePath := filepath.Join(tmpDir, f)
		if _, err := os.Stat(filePath); os.IsNotExist(err) {
			t.Errorf("dry-run should not remove files, but %s was removed", f)
		}
	}
}

func TestRunCleanupNonExistentDir(t *testing.T) {
	// Set package variables for the test
	dataDir = "/nonexistent/path/that/does/not/exist"
	dbFilename = "dump.rdb"
	dryRun = false

	// Run cleanup - should not error, just skip
	if err := runCleanup(nil, nil); err != nil {
		t.Fatalf("runCleanup should not fail for non-existent dir: %v", err)
	}
}

func TestRunCleanupNotADirectory(t *testing.T) {
	// Create a temp file (not a directory)
	tmpFile, err := os.CreateTemp("", "redis-cleanup-test")
	if err != nil {
		t.Fatalf("failed to create temp file: %v", err)
	}
	defer os.Remove(tmpFile.Name())
	tmpFile.Close()

	// Set package variables for the test
	dataDir = tmpFile.Name()
	dbFilename = "dump.rdb"
	dryRun = false

	// Run cleanup - should error because it's not a directory
	if err := runCleanup(nil, nil); err == nil {
		t.Fatal("runCleanup should fail when dataDir is not a directory")
	}
}
