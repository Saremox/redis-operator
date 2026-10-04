package k8s

import (
	"fmt"

	redisfailoverv1 "github.com/saremox/redis-operator/api/redisfailover/v1"
	"github.com/saremox/redis-operator/metrics"
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

func (e *ControlledByOtherError) Error() string {
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

func recordMetrics(namespace string, kind string, object string, operation string, err error, metricsRecorder metrics.Recorder) {
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
