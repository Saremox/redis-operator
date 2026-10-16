package main

import (
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// run returns the error of the config, and of the client config, before it
// starts anything.
func TestRunEarlyErrors(t *testing.T) {
	dir := t.TempDir()
	log := slog.New(slog.DiscardHandler)
	if err := run(filepath.Join(dir, "missing.yaml"), ":0", log); err == nil || !strings.Contains(err.Error(), "no such file or directory") {
		t.Errorf("missing config: %v", err)
	}
	cfg := filepath.Join(dir, "config.yaml")
	if err := os.WriteFile(cfg, []byte("instances: [{name: a}]"), 0o600); err != nil {
		t.Fatal(err)
	}
	kubeconfig := filepath.Join(dir, "kubeconfig")
	if err := os.WriteFile(kubeconfig, []byte("{"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("KUBECONFIG", kubeconfig)
	t.Setenv("KUBERNETES_SERVICE_HOST", "")
	if err := run(cfg, ":0", log); err == nil || !strings.Contains(err.Error(), kubeconfig) {
		t.Errorf("bad kubeconfig: %v", err)
	}
}
