//go:build integration

package redisfailover_test

import (
	"context"
	"fmt"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
)

// waitForNamespaceActive polls until the namespace exists and is Active,
// instead of blindly sleeping and hoping the API server caught up by then.
func waitForNamespaceActive(k8sClient kubernetes.Interface, name string, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		ns, err := k8sClient.CoreV1().Namespaces().Get(context.Background(), name, metav1.GetOptions{})
		if err == nil && ns.Status.Phase == corev1.NamespaceActive {
			return nil
		}
		time.Sleep(250 * time.Millisecond)
	}
	return fmt.Errorf("timed out waiting for namespace %q to become Active", name)
}

// waitForOperatorStartup returns an error when the operator exits before the
// timeout, for example with a bad kubeconfig or without the CRD. The operator
// in the test has no health endpoint to poll. A nil return means only that
// the operator did not exit. The pod readiness checks after it show that the
// operator reconciles.
func waitForOperatorStartup(errC <-chan error, timeout time.Duration) error {
	select {
	case err := <-errC:
		return fmt.Errorf("operator exited during startup: %w", err)
	case <-time.After(timeout):
		return nil
	}
}
