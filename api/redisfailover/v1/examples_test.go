package v1

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"regexp"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"sigs.k8s.io/yaml"
)

// TestExamplesAreValid decodes every RedisFailover in the examples as the
// API server would: without type coercion and with unknown fields rejected.
func TestExamplesAreValid(t *testing.T) {
	files, err := filepath.Glob("../../../example/redisfailover/*.yaml")
	require.NoError(t, err)
	require.NotEmpty(t, files)

	for _, f := range files {
		t.Run(filepath.Base(f), func(t *testing.T) {
			data, err := os.ReadFile(f)
			require.NoError(t, err)

			found := false
			for _, doc := range regexp.MustCompile(`(?m)^---\s*$`).Split(string(data), -1) {
				// No target type, so a number stays a number.
				var raw map[string]any
				require.NoError(t, yaml.Unmarshal([]byte(doc), &raw))
				if raw["kind"] != "RedisFailover" {
					continue
				}
				found = true
				assert.Contains(t, raw, "spec", "the CRD requires spec")

				js, err := json.Marshal(raw)
				require.NoError(t, err)
				dec := json.NewDecoder(bytes.NewReader(js))
				dec.DisallowUnknownFields()
				var rf RedisFailover
				require.NoError(t, dec.Decode(&rf))
				assert.NoError(t, rf.Validate())
			}
			assert.True(t, found, "no RedisFailover in the file")
		})
	}
}
