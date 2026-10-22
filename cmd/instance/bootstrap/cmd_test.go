package bootstrap

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
)

func TestBootstrapCopiesTheRunningBinary(t *testing.T) {
	dest := filepath.Join(t.TempDir(), "redis-instance")

	cmd := NewCmd()
	cmd.SetArgs([]string{dest})
	if err := cmd.Execute(); err != nil {
		t.Fatalf("bootstrap returned %v, want nil", err)
	}

	self, err := os.Executable()
	if err != nil {
		t.Fatalf("os.Executable returned %v", err)
	}
	want, err := os.ReadFile(self)
	if err != nil {
		t.Fatalf("read source: %v", err)
	}
	got, err := os.ReadFile(dest)
	if err != nil {
		t.Fatalf("read destination: %v", err)
	}
	if len(got) != len(want) {
		t.Errorf("copied %d bytes, want %d", len(got), len(want))
	}
}

func TestBootstrapMakesTheCopyExecutable(t *testing.T) {
	dest := filepath.Join(t.TempDir(), "redis-instance")

	cmd := NewCmd()
	cmd.SetArgs([]string{dest})
	if err := cmd.Execute(); err != nil {
		t.Fatalf("bootstrap returned %v, want nil", err)
	}

	info, err := os.Stat(dest)
	if err != nil {
		t.Fatalf("stat destination: %v", err)
	}
	if info.Mode().Perm()&0o111 == 0 {
		t.Errorf("destination mode is %v, want the execute bits set", info.Mode().Perm())
	}
}

func TestBootstrapLeavesNoFileWhenTheSourceIsUnknown(t *testing.T) {
	dir := t.TempDir()
	dest := filepath.Join(dir, "redis-instance")

	executable = func() (string, error) { return "", errors.New("no executable") }
	t.Cleanup(func() { executable = os.Executable })

	cmd := NewCmd()
	cmd.SetArgs([]string{dest})
	if err := cmd.Execute(); err == nil {
		t.Fatal("bootstrap returned nil, want an error")
	}

	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("read dir: %v", err)
	}
	if len(entries) != 0 {
		t.Errorf("the directory holds %d entries, want 0", len(entries))
	}
}

func TestBootstrapFailsWhenTheDestinationDirectoryIsMissing(t *testing.T) {
	dest := filepath.Join(t.TempDir(), "missing", "redis-instance")

	cmd := NewCmd()
	cmd.SetArgs([]string{dest})
	if err := cmd.Execute(); err == nil {
		t.Fatal("bootstrap returned nil, want an error")
	}
}

func TestBootstrapNeedsExactlyOneArgument(t *testing.T) {
	cmd := NewCmd()
	cmd.SetArgs(nil)
	if err := cmd.Execute(); err == nil {
		t.Fatal("bootstrap returned nil, want an error")
	}
}

func TestBootstrapFailsWhenTheSourceIsGone(t *testing.T) {
	dir := t.TempDir()
	dest := filepath.Join(dir, "redis-instance")

	// A path that resolves but does not exist: os.Executable succeeds and the
	// open fails.
	executable = func() (string, error) { return filepath.Join(dir, "gone"), nil }
	t.Cleanup(func() { executable = os.Executable })

	cmd := NewCmd()
	cmd.SetArgs([]string{dest})
	if err := cmd.Execute(); err == nil {
		t.Fatal("bootstrap returned nil, want an error")
	}

	if _, err := os.Stat(dest); !os.IsNotExist(err) {
		t.Errorf("stat destination returned %v, want it to be absent", err)
	}
}

func TestBootstrapFailsWhenTheSourceCannotBeRead(t *testing.T) {
	dir := t.TempDir()
	dest := filepath.Join(dir, "redis-instance")

	// Opening a directory succeeds and reading it does not, so this reaches
	// the copy itself rather than the open above it.
	source := filepath.Join(dir, "a-directory")
	if err := os.Mkdir(source, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	executable = func() (string, error) { return source, nil }
	t.Cleanup(func() { executable = os.Executable })

	cmd := NewCmd()
	cmd.SetArgs([]string{dest})
	if err := cmd.Execute(); err == nil {
		t.Fatal("bootstrap returned nil, want an error")
	}

	if _, err := os.Stat(dest); !os.IsNotExist(err) {
		t.Errorf("stat destination returned %v, want it to be absent", err)
	}
}

func TestBootstrapFailsWhenTheDestinationIsADirectory(t *testing.T) {
	dir := t.TempDir()
	dest := filepath.Join(dir, "redis-instance")
	if err := os.Mkdir(dest, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}

	cmd := NewCmd()
	cmd.SetArgs([]string{dest})
	if err := cmd.Execute(); err == nil {
		t.Fatal("bootstrap returned nil, want an error")
	}
}

func TestBootstrapLeavesNoTemporaryFileBehind(t *testing.T) {
	dir := t.TempDir()
	dest := filepath.Join(dir, "redis-instance")

	cmd := NewCmd()
	cmd.SetArgs([]string{dest})
	if err := cmd.Execute(); err != nil {
		t.Fatalf("bootstrap returned %v, want nil", err)
	}

	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("read dir: %v", err)
	}
	if len(entries) != 1 {
		t.Errorf("the directory holds %d entries, want only the copy", len(entries))
	}
}
