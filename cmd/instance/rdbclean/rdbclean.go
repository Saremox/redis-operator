// Package rdbclean removes the stale .rdb files of a Redis data directory. A
// BGSAVE writes temp-<pid>.rdb, and a crash during the save leaves the file.
// The files collect until the disk is full, and then each BGSAVE fails.
package rdbclean

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// Kind is what an Event reports.
type Kind int

const (
	// Missing means that the data directory does not exist. There is nothing to clean.
	Missing Kind = iota
	// InfoFailed means that the size of a file is unknown, so the file stays.
	InfoFailed
	// RemoveFailed means that the file stays.
	RemoveFailed
	// Removed means that the file is gone.
	Removed
	// WouldRemove is Removed in a dry run.
	WouldRemove
)

// Event reports one step of Clean. The two commands print it in their own text.
type Event struct {
	Kind Kind
	// Name is the file name, and Path the name with the directory.
	Name, Path string
	Size       int64
	// Err is the error of InfoFailed and RemoveFailed.
	Err error
}

// Clean removes the .rdb files of dir except keep. It reports each file to
// report, and returns the number and the size of the files that it removed, or
// would remove in a dry run. readDir is a parameter so that a test can inject
// a read fault.
func Clean(dir, keep string, dryRun bool, readDir func(string) ([]os.DirEntry, error), report func(Event)) (int, int64, error) {
	entries, err := list(dir, readDir, report)
	if err != nil {
		return 0, 0, err
	}
	var cleaned int
	var total int64
	for _, entry := range entries {
		if size, ok := cleanEntry(entry, dir, keep, dryRun, report); ok {
			cleaned++
			total += size
		}
	}
	return cleaned, total, nil
}

// list returns the entries of dir. A missing directory is not an error. It is
// reported and has no entries.
func list(dir string, readDir func(string) ([]os.DirEntry, error), report func(Event)) ([]os.DirEntry, error) {
	info, err := os.Stat(dir)
	if os.IsNotExist(err) {
		report(Event{Kind: Missing, Path: dir})
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("failed to stat data directory: %w", err)
	}
	if !info.IsDir() {
		return nil, fmt.Errorf("%s is not a directory", dir)
	}
	entries, err := readDir(dir)
	if err != nil {
		return nil, fmt.Errorf("failed to read data directory: %w", err)
	}
	return entries, nil
}

// stale reports whether the entry is a .rdb file other than keep.
func stale(entry os.DirEntry, keep string) bool {
	return !entry.IsDir() && strings.HasSuffix(entry.Name(), ".rdb") && entry.Name() != keep
}

// cleanEntry removes the entry when it is a stale .rdb file. It returns the
// size of the file, and whether the file counts as cleaned.
func cleanEntry(entry os.DirEntry, dir, keep string, dryRun bool, report func(Event)) (int64, bool) {
	name := entry.Name()
	if !stale(entry, keep) {
		return 0, false
	}
	path := filepath.Join(dir, name)
	fileInfo, err := entry.Info()
	if err != nil {
		report(Event{Kind: InfoFailed, Name: name, Path: path, Err: err})
		return 0, false
	}
	kind := WouldRemove
	if !dryRun {
		if err := os.Remove(path); err != nil {
			report(Event{Kind: RemoveFailed, Name: name, Path: path, Err: err})
			return 0, false
		}
		kind = Removed
	}
	report(Event{Kind: kind, Name: name, Path: path, Size: fileInfo.Size()})
	return fileInfo.Size(), true
}
