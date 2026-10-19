package service_test

import (
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	redisfailoverv1 "github.com/saremox/redis-operator/api/redisfailover/v1"
	rfservice "github.com/saremox/redis-operator/operator/redisfailover/service"
)

func TestSentinelMasterNameIsTheDefault(t *testing.T) {
	rf := generateRF()
	assert.Equal(t, "mymaster", rfservice.SentinelMasterName(rf))
	assert.Equal(t, redisfailoverv1.DefaultSentinelMasterName, rfservice.SentinelMasterName(rf))
}

// TestSentinelMasterNameHasOneDefinition finds each Go string literal with
// the master name in the non-test sources of the root module. A copy of the
// name outside DefaultSentinelMasterName does not follow a change of the name.
// Comments, docs, the example manifests and the soak tester, a module of its
// own, are not Go code of the operator.
func TestSentinelMasterNameHasOneDefinition(t *testing.T) {
	root, err := filepath.Abs(filepath.Join("..", "..", ".."))
	require.NoError(t, err)
	require.FileExists(t, filepath.Join(root, "go.mod"))

	var found []string
	err = filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			if path == root {
				return nil
			}
			// A nested module, for example test/soak or a git worktree, is
			// not part of the operator.
			if strings.HasPrefix(d.Name(), ".") || fileExists(filepath.Join(path, "go.mod")) {
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
			return nil
		}
		file, err := parser.ParseFile(token.NewFileSet(), path, nil, parser.SkipObjectResolution)
		if err != nil {
			return err
		}
		rel, _ := filepath.Rel(root, path)
		ast.Inspect(file, func(n ast.Node) bool {
			lit, ok := n.(*ast.BasicLit)
			if !ok || lit.Kind != token.STRING {
				return true
			}
			if value, err := strconv.Unquote(lit.Value); err == nil && strings.Contains(value, "mymaster") {
				found = append(found, filepath.ToSlash(rel))
			}
			return true
		})
		return nil
	})
	require.NoError(t, err)
	assert.Equal(t, []string{"api/redisfailover/v1/defaults.go"}, found)
}

func fileExists(path string) bool {
	_, err := os.Stat(path)
	return err == nil
}
