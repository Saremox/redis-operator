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

// waitForOperatorStartup replaces a blind "give the operator a moment to
// start" sleep: there's no external readiness signal to poll for (no health
// endpoint, no leader-election lease in this harness), but errC - fed by the
// goroutine running redisfailoverOperator.Run - lets us fail fast if the
// operator exits during that window (bad kubeconfig, missing CRD, ...)
// instead of waiting out the full timeout and failing confusingly later at
// pod-readiness polling. A nil return after the timeout elapses without an
// error means only that the operator didn't visibly crash; the pod-readiness
// polling that follows is what actually confirms it's reconciling.
func waitForOperatorStartup(errC <-chan error, timeout time.Duration) error {
	select {
	case err := <-errC:
		return fmt.Errorf("operator exited during startup: %w", err)
	case <-time.After(timeout):
		return nil
	}
}
