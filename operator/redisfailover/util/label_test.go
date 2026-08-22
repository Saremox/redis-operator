package util_test

import (
	"testing"

	"github.com/stretchr/testify/assert"

	"github.com/saremox/redis-operator/operator/redisfailover/util"
)

func TestMergeLabelsNoArguments(t *testing.T) {
	res := util.MergeLabels()

	assert.NotNil(t, res)
	assert.Empty(t, res)
}

func TestMergeLabelsSingleMapIsCopy(t *testing.T) {
	input := map[string]string{"a": "1", "b": "2"}

	res := util.MergeLabels(input)

	assert.Equal(t, input, res)

	// Prove the result is a distinct map: mutating it must not mutate the input.
	res["a"] = "mutated"
	res["c"] = "3"

	assert.Equal(t, "1", input["a"], "mutating the merged result must not affect the source map")
	_, ok := input["c"]
	assert.False(t, ok, "mutating the merged result must not affect the source map")
}

func TestMergeLabelsUnionNoOverlap(t *testing.T) {
	a := map[string]string{"a": "1"}
	b := map[string]string{"b": "2"}
	c := map[string]string{"c": "3"}

	res := util.MergeLabels(a, b, c)

	assert.Equal(t, map[string]string{"a": "1", "b": "2", "c": "3"}, res)
}

func TestMergeLabelsLaterArgumentWins(t *testing.T) {
	first := map[string]string{"a": "1"}
	second := map[string]string{"a": "2"}

	res := util.MergeLabels(first, second)

	assert.Equal(t, map[string]string{"a": "2"}, res)
}

func TestMergeLabelsLaterArgumentWinsAmongMany(t *testing.T) {
	res := util.MergeLabels(
		map[string]string{"a": "1", "shared": "first"},
		map[string]string{"b": "2", "shared": "second"},
		map[string]string{"c": "3", "shared": "third"},
	)

	assert.Equal(t, map[string]string{
		"a":      "1",
		"b":      "2",
		"c":      "3",
		"shared": "third",
	}, res)
}

func TestMergeLabelsNilMapsAreSkipped(t *testing.T) {
	res := util.MergeLabels(nil, map[string]string{"a": "1"}, nil)

	assert.Equal(t, map[string]string{"a": "1"}, res)
}

func TestMergeLabelsAllNilMaps(t *testing.T) {
	res := util.MergeLabels(nil, nil)

	assert.NotNil(t, res)
	assert.Empty(t, res)
}

func TestMergeAnnotationsNoArguments(t *testing.T) {
	res := util.MergeAnnotations()

	assert.NotNil(t, res)
	assert.Empty(t, res)
}

func TestMergeAnnotationsSingleMapIsCopy(t *testing.T) {
	input := map[string]string{"a": "1", "b": "2"}

	res := util.MergeAnnotations(input)

	assert.Equal(t, input, res)

	// Prove the result is a distinct map: mutating it must not mutate the input.
	res["a"] = "mutated"
	res["c"] = "3"

	assert.Equal(t, "1", input["a"], "mutating the merged result must not affect the source map")
	_, ok := input["c"]
	assert.False(t, ok, "mutating the merged result must not affect the source map")
}

func TestMergeAnnotationsUnionNoOverlap(t *testing.T) {
	a := map[string]string{"a": "1"}
	b := map[string]string{"b": "2"}
	c := map[string]string{"c": "3"}

	res := util.MergeAnnotations(a, b, c)

	assert.Equal(t, map[string]string{"a": "1", "b": "2", "c": "3"}, res)
}

func TestMergeAnnotationsLaterArgumentWins(t *testing.T) {
	first := map[string]string{"a": "1"}
	second := map[string]string{"a": "2"}

	res := util.MergeAnnotations(first, second)

	assert.Equal(t, map[string]string{"a": "2"}, res)
}

func TestMergeAnnotationsNilMapsAreSkipped(t *testing.T) {
	res := util.MergeAnnotations(nil, map[string]string{"a": "1"}, nil)

	assert.Equal(t, map[string]string{"a": "1"}, res)
}

func TestMergeAnnotationsAllNilMaps(t *testing.T) {
	res := util.MergeAnnotations(nil, nil)

	assert.NotNil(t, res)
	assert.Empty(t, res)
}
