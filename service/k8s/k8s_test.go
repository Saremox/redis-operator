package k8s_test

import (
	"testing"

	"github.com/stretchr/testify/assert"
	apiextensionsfake "k8s.io/apiextensions-apiserver/pkg/client/clientset/clientset/fake"
	kubernetes "k8s.io/client-go/kubernetes/fake"

	redisfailoverfake "github.com/saremox/redis-operator/client/k8s/clientset/versioned/fake"
	"github.com/saremox/redis-operator/log"
	"github.com/saremox/redis-operator/metrics"
	"github.com/saremox/redis-operator/service/k8s"
)

// TestNew proves every sub-service New wires up is actually reachable through
// the returned Services value - a nil embedded field here (e.g. the
// ServiceAccount gap #43 originally left unwired) would panic on first use
// instead of failing to compile, since Services is composed of interfaces.
func TestNew(t *testing.T) {
	assertTest := assert.New(t)

	kubecli := kubernetes.NewSimpleClientset()
	crdcli := redisfailoverfake.NewSimpleClientset()
	apiextcli := apiextensionsfake.NewSimpleClientset()

	svc := k8s.New(kubecli, crdcli, apiextcli, log.Dummy, metrics.Dummy)
	assertTest.NotNil(svc)

	_, err := svc.ListServiceAccounts("default")
	assertTest.NoError(err)

	_, err = svc.GetStatefulSetPods("default", "does-not-exist")
	assertTest.Error(err)
}
