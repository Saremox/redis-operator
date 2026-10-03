package cleanup

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
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
			defer func() { _ = os.RemoveAll(tmpDir) }()

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
	defer func() { _ = os.RemoveAll(tmpDir) }()

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
	defer func() { _ = os.Remove(tmpFile.Name()) }()
	_ = tmpFile.Close()

	// Set package variables for the test
	dataDir = tmpFile.Name()
	dbFilename = "dump.rdb"
	dryRun = false

	// Run cleanup - should error because it's not a directory
	if err := runCleanup(nil, nil); err == nil {
		t.Fatal("runCleanup should fail when dataDir is not a directory")
	}
}

// fakeDirEntry is a directory entry that readDir can return. It lets a test
// inject faults that a real directory cannot give to the root user.
type fakeDirEntry struct {
	name    string
	info    fs.FileInfo
	infoErr error
}

func (e fakeDirEntry) Name() string               { return e.name }
func (e fakeDirEntry) IsDir() bool                { return false }
func (e fakeDirEntry) Type() fs.FileMode          { return 0 }
func (e fakeDirEntry) Info() (fs.FileInfo, error) { return e.info, e.infoErr }

// setCleanupFlags sets the package flags and restores them after the test.
func setCleanupFlags(t *testing.T, dir, db string, dry bool) {
	t.Helper()
	prevDataDir, prevDBFilename, prevDryRun, prevReadDir := dataDir, dbFilename, dryRun, readDir
	t.Cleanup(func() {
		dataDir, dbFilename, dryRun, readDir = prevDataDir, prevDBFilename, prevDryRun, prevReadDir
	})
	dataDir, dbFilename, dryRun = dir, db, dry
}

func TestNewCmdRegistersFlagsWithDefaults(t *testing.T) {
	cmd := NewCmd()

	assert.Equal(t, "cleanup", cmd.Use)
	assert.Equal(t, defaultDataDir, cmd.Flags().Lookup("data-dir").DefValue)
	assert.Equal(t, defaultDBFilename, cmd.Flags().Lookup("db-filename").DefValue)
	assert.Equal(t, "false", cmd.Flags().Lookup("dry-run").DefValue)
}

// TestNewCmdExecuteRemovesStaleFiles runs the command through cobra, so that
// the flags reach runCleanup.
func TestNewCmdExecuteRemovesStaleFiles(t *testing.T) {
	setCleanupFlags(t, "", "", false)
	dir := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(dir, "main.rdb"), []byte("keep"), 0o600))
	require.NoError(t, os.WriteFile(filepath.Join(dir, "temp-1.rdb"), []byte("stale"), 0o600))

	cmd := NewCmd()
	cmd.SetArgs([]string{"--data-dir", dir, "--db-filename", "main.rdb"})
	require.NoError(t, cmd.Execute())

	assert.FileExists(t, filepath.Join(dir, "main.rdb"))
	assert.NoFileExists(t, filepath.Join(dir, "temp-1.rdb"))
}

// TestRunCleanupKeepsSubdirectories makes sure that a directory with an .rdb
// name stays, because cleanup removes only files.
func TestRunCleanupKeepsSubdirectories(t *testing.T) {
	dir := t.TempDir()
	require.NoError(t, os.Mkdir(filepath.Join(dir, "backup.rdb"), 0o700))
	setCleanupFlags(t, dir, "dump.rdb", false)

	require.NoError(t, runCleanup(nil, nil))

	assert.DirExists(t, filepath.Join(dir, "backup.rdb"))
}

// TestRunCleanupReturnsStatError uses a path below a regular file. Stat
// fails with ENOTDIR, which is not "does not exist", so cleanup must fail.
func TestRunCleanupReturnsStatError(t *testing.T) {
	file := filepath.Join(t.TempDir(), "file")
	require.NoError(t, os.WriteFile(file, nil, 0o600))
	setCleanupFlags(t, filepath.Join(file, "data"), "dump.rdb", false)

	err := runCleanup(nil, nil)

	require.ErrorContains(t, err, "failed to stat data directory")
}

func TestRunCleanupReturnsReadDirError(t *testing.T) {
	setCleanupFlags(t, t.TempDir(), "dump.rdb", false)
	readErr := errors.New("injected read error")
	readDir = func(string) ([]fs.DirEntry, error) { return nil, readErr }

	err := runCleanup(nil, nil)

	require.ErrorIs(t, err, readErr)
	assert.ErrorContains(t, err, "failed to read data directory")
}

// TestRunCleanupSkipsEntriesThatFail injects an entry whose Info fails and an
// entry whose file is gone, so that Remove fails. Cleanup must skip both,
// continue, and remove the stale file that follows them.
func TestRunCleanupSkipsEntriesThatFail(t *testing.T) {
	dir := t.TempDir()
	stale := filepath.Join(dir, "temp-1.rdb")
	require.NoError(t, os.WriteFile(stale, []byte("stale"), 0o600))
	staleInfo, err := os.Stat(stale)
	require.NoError(t, err)

	setCleanupFlags(t, dir, "dump.rdb", false)
	readDir = func(string) ([]fs.DirEntry, error) {
		return []fs.DirEntry{
			fakeDirEntry{name: "no-info.rdb", infoErr: errors.New("injected info error")},
			fakeDirEntry{name: "gone.rdb", info: staleInfo},
			fakeDirEntry{name: "temp-1.rdb", info: staleInfo},
		}, nil
	}

	require.NoError(t, runCleanup(nil, nil))

	assert.NoFileExists(t, stale)
}
