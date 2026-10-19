package k8s

import (
	"fmt"

	redisfailoverv1 "github.com/saremox/redis-operator/api/redisfailover/v1"
	"github.com/saremox/redis-operator/log"
	"github.com/saremox/redis-operator/metrics"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
)

// ControlledByOtherError reports that a stored object has another controller
// than the desired object. The names of two RedisFailovers can generate the
// same object name, and the operator must not take over the object.
type ControlledByOtherError struct {
	Kind      string
	Name      string
	OwnerKind string
	OwnerName string
}

// Error names the object and its controller, or says that the object has no
// controller.
func (e *ControlledByOtherError) Error() string {
	if e.OwnerKind == "" {
		return fmt.Sprintf("cannot manage %s %q: it has no controller", e.Kind, e.Name)
	}
	return fmt.Sprintf("cannot manage %s %q: %s %q controls it", e.Kind, e.Name, e.OwnerKind, e.OwnerName)
}

// checkNotControlledByOther returns an error when the stored and the desired
// object have different controllers. It compares the API group, Kind and Name,
// not the version and not the UID, so a RedisFailover that is created again
// keeps its objects. An object without a controller is not checked.
func checkNotControlledByOther(kind string, stored, desired metav1.Object) error {
	want := metav1.GetControllerOf(desired)
	have := metav1.GetControllerOf(stored)
	if want == nil || have == nil || (want.Kind == have.Kind && want.Name == have.Name && controllerGroup(want) == controllerGroup(have)) {
		return nil
	}
	return &ControlledByOtherError{Kind: kind, Name: stored.GetName(), OwnerKind: have.Kind, OwnerName: have.Name}
}

// CheckSameController is checkNotControlledByOther that also refuses a stored
// object without a controller. A user can create an object with the name of an
// object that the operator only writes, and the operator must keep it.
func CheckSameController(kind string, stored, desired metav1.Object) error {
	if metav1.GetControllerOf(stored) == nil {
		return &ControlledByOtherError{Kind: kind, Name: stored.GetName()}
	}
	return checkNotControlledByOther(kind, stored, desired)
}

// objectCalls are the calls of createOrUpdate for one kind. skipped logs an
// object that is up to date. keep, when set, copies to desired what an update
// must keep from stored.
type objectCalls[T metav1.Object] struct {
	kind     string
	get      func(namespace, name string) (T, error)
	create   func(namespace string, object T) error
	update   func(namespace string, object T) error
	upToDate func(stored, desired T) bool
	keep     func(stored, desired T)
	skipped  func()
}

// createOrUpdate creates desired, or updates the stored object when it is not
// up to date. It returns a ControlledByOtherError when another controller
// controls the stored object.
func createOrUpdate[T metav1.Object](namespace string, desired T, calls objectCalls[T]) error {
	stored, err := calls.get(namespace, desired.GetName())
	if err != nil {
		if errors.IsNotFound(err) {
			return calls.create(namespace, desired)
		}
		return err
	}

	if err := checkNotControlledByOther(calls.kind, stored, desired); err != nil {
		return err
	}

	// With the stored resource version, the update fails when the object
	// changed after the read.
	desired.SetResourceVersion(stored.GetResourceVersion())
	if calls.keep != nil {
		calls.keep(stored, desired)
	}

	if calls.upToDate(stored, desired) {
		calls.skipped()
		return nil
	}

	return calls.update(namespace, desired)
}

// writeResult returns the error of a create or an update with failed and the
// object. Without an error, it logs done.
func writeResult(err error, logger log.Logger, namespace, field, name, failed, done string) error {
	if err != nil {
		return fmt.Errorf("%s %s/%s: %w", failed, namespace, name, err)
	}
	logger.WithField("namespace", namespace).WithField(field, name).Debugf("%s", done)
	return nil
}

func controllerGroup(ref *metav1.OwnerReference) string {
	gv, err := schema.ParseGroupVersion(ref.APIVersion)
	if err != nil {
		return ref.APIVersion
	}
	return gv.Group
}

// GetRedisPassword returns the password key of the auth.secretPath Secret, or
// an empty string without auth.secretPath.
func GetRedisPassword(s Services, rf *redisfailoverv1.RedisFailover) (string, error) {

	if rf.Spec.Auth.SecretPath == "" {
		return "", nil
	}

	secret, err := s.GetSecret(rf.Namespace, rf.Spec.Auth.SecretPath)
	if err != nil {
		return "", err
	}

	if password, ok := secret.Data["password"]; ok {
		return string(password), nil
	}

	return "", fmt.Errorf("secret \"%s\" does not have a password field", rf.Spec.Auth.SecretPath)
}

func recordMetrics(namespace, kind, object, operation string, err error, metricsRecorder metrics.Recorder) {
	if nil == err {
		metricsRecorder.RecordK8sOperation(namespace, kind, object, operation, metrics.SUCCESS, metrics.NOT_APPLICABLE)
	} else if errors.IsForbidden(err) {
		metricsRecorder.RecordK8sOperation(namespace, kind, object, operation, metrics.FAIL, metrics.K8S_FORBIDDEN_ERR)
	} else if errors.IsUnauthorized(err) {
		metricsRecorder.RecordK8sOperation(namespace, kind, object, operation, metrics.FAIL, metrics.K8S_UNAUTH)
	} else if errors.IsNotFound(err) {
		metricsRecorder.RecordK8sOperation(namespace, kind, object, operation, metrics.FAIL, metrics.K8S_NOT_FOUND)
	} else {
		metricsRecorder.RecordK8sOperation(namespace, kind, object, operation, metrics.FAIL, metrics.K8S_MISC)
	}
}

// setPodIPs gives the pod IPs to the metrics. The operator sends the Redis and
// Sentinel commands only to the IPs of these pod lists.
func setPodIPs(kind string, pods []corev1.Pod, metricsRecorder metrics.Recorder) {
	for i := range pods {
		if ip := pods[i].Status.PodIP; ip != "" {
			metricsRecorder.SetPodIP(kind, pods[i].Namespace, pods[i].Name, ip)
		}
	}
}
