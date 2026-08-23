package v1_test

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"k8s.io/apimachinery/pkg/runtime"

	v1 "github.com/saremox/redis-operator/api/redisfailover/v1"
)

func TestKind(t *testing.T) {
	assertTest := assert.New(t)

	gk := v1.Kind("RedisFailover")
	assertTest.Equal(v1.SchemeGroupVersion.Group, gk.Group)
	assertTest.Equal("RedisFailover", gk.Kind)
}

func TestVersionKind(t *testing.T) {
	assertTest := assert.New(t)

	gvk := v1.VersionKind("RedisFailover")
	assertTest.Equal(v1.SchemeGroupVersion.WithKind("RedisFailover"), gvk)
}

func TestResource(t *testing.T) {
	assertTest := assert.New(t)

	gr := v1.Resource("redisfailovers")
	assertTest.Equal(v1.SchemeGroupVersion.Group, gr.Group)
	assertTest.Equal("redisfailovers", gr.Resource)
}

func TestAddToScheme(t *testing.T) {
	assertTest := assert.New(t)

	scheme := runtime.NewScheme()
	err := v1.AddToScheme(scheme)
	assertTest.NoError(err)
	assertTest.True(scheme.Recognizes(v1.SchemeGroupVersion.WithKind(v1.RFKind)))
}
