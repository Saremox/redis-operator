package k8s_test

import (
	"context"
	"errors"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	discoveryv1 "k8s.io/api/discovery/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/kubernetes/fake"
	kubetesting "k8s.io/client-go/testing"

	"github.com/saremox/redis-operator/log"
	"github.com/saremox/redis-operator/metrics"
	"github.com/saremox/redis-operator/service/k8s"
)

func slice(name, service string, addresses ...string) *discoveryv1.EndpointSlice {
	return &discoveryv1.EndpointSlice{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "ns", Labels: map[string]string{discoveryv1.LabelServiceName: service}},
		Endpoints:  []discoveryv1.Endpoint{{Addresses: addresses}},
	}
}

func TestEndpointSliceServiceGetEndpointAddresses(t *testing.T) {
	cli := fake.NewClientset(slice("a", "master", "1.1.1.1"), slice("b", "master", "2.2.2.2", "3.3.3.3"), slice("c", "other", "9.9.9.9"))
	service := k8s.NewEndpointSliceService(cli, log.Dummy, metrics.Dummy)

	addresses, err := service.GetEndpointAddresses(context.Background(), "ns", "master")

	require.NoError(t, err)
	assert.ElementsMatch(t, []string{"1.1.1.1", "2.2.2.2", "3.3.3.3"}, addresses)
}

func TestEndpointSliceServiceWrapsTheListError(t *testing.T) {
	cli := fake.NewClientset()
	cli.PrependReactor("list", "endpointslices", func(kubetesting.Action) (bool, runtime.Object, error) {
		return true, nil, errors.New("boom")
	})
	service := k8s.NewEndpointSliceService(cli, log.Dummy, metrics.Dummy)

	_, err := service.GetEndpointAddresses(context.Background(), "ns", "master")

	require.ErrorContains(t, err, "list endpointslices of service ns/master: boom")
}
