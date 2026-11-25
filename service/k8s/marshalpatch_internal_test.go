package k8s

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestMarshalPatch(t *testing.T) {
	data, err := marshalPatch(map[string]any{"metadata": map[string]any{"labels": map[string]string{"a": "b"}}})
	require.NoError(t, err)
	assert.JSONEq(t, `{"metadata":{"labels":{"a":"b"}}}`, string(data))

	// A value that JSON cannot hold is an error, not an empty patch.
	data, err = marshalPatch(map[string]any{"bad": make(chan int)})
	require.ErrorContains(t, err, "marshal the patch")
	assert.Nil(t, data)
}
