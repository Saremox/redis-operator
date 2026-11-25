package k8s

import (
	"context"
	"fmt"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/saremox/redis-operator/log"
	"github.com/saremox/redis-operator/metrics"
)

// namespacedAPI is the part of a typed client-go client that the services use.
// Each typed client of a namespaced kind has these four methods.
type namespacedAPI[T metav1.Object] interface {
	Get(ctx context.Context, name string, opts metav1.GetOptions) (T, error)
	Create(ctx context.Context, object T, opts metav1.CreateOptions) (T, error)
	Update(ctx context.Context, object T, opts metav1.UpdateOptions) (T, error)
	Delete(ctx context.Context, name string, opts metav1.DeleteOptions) error
}

// kindClient holds the Get, Create, Update, Delete and CreateOrUpdate calls that
// every simple kind needs in the same way. One copy keeps the metrics, the
// error text and the ownership check equal for all kinds.
type kindClient[T metav1.Object] struct {
	// kind is the Kind, as in metrics and in ControlledByOtherError.
	kind string
	// noun is the lower-case kind in error text.
	noun string
	// field is the log field of the object name.
	field string
	// nameFromSpec is true when a user can choose the object name, so the name
	// must not be a metric label.
	nameFromSpec bool
	deleteOpts   metav1.DeleteOptions
	api          func(namespace string) namespacedAPI[T]
	upToDate     func(stored, desired T) bool
	// keep, when set, copies to desired what an update must not remove.
	keep    func(stored, desired T)
	logger  log.Logger
	metrics metrics.Recorder
}

func (r *kindClient[T]) get(namespace, name string) (T, error) {
	object, err := r.api(namespace).Get(context.TODO(), name, metav1.GetOptions{})
	label := name
	if r.nameFromSpec {
		label = metrics.NOT_APPLICABLE
	}
	recordMetrics(namespace, r.kind, label, "GET", err, r.metrics)
	if err != nil {
		var none T
		return none, fmt.Errorf("get %s %s/%s: %w", r.noun, namespace, name, err)
	}
	return object, nil
}

func (r *kindClient[T]) create(namespace string, object T) error {
	_, err := r.api(namespace).Create(context.TODO(), object, metav1.CreateOptions{})
	recordMetrics(namespace, r.kind, object.GetName(), "CREATE", err, r.metrics)
	return writeResult(err, r.logger, namespace, r.field, object.GetName(), "create "+r.noun, r.field+" created")
}

func (r *kindClient[T]) update(namespace string, object T) error {
	_, err := r.api(namespace).Update(context.TODO(), object, metav1.UpdateOptions{})
	recordMetrics(namespace, r.kind, object.GetName(), "UPDATE", err, r.metrics)
	return writeResult(err, r.logger, namespace, r.field, object.GetName(), "update "+r.noun, r.field+" updated")
}

func (r *kindClient[T]) delete(namespace, name string) error {
	err := r.api(namespace).Delete(context.TODO(), name, r.deleteOpts)
	recordMetrics(namespace, r.kind, name, "DELETE", err, r.metrics)
	if err != nil {
		return fmt.Errorf("delete %s %s/%s: %w", r.noun, namespace, name, err)
	}
	return nil
}

// createOrUpdate creates desired, or updates the stored object when it is not
// up to date. It returns a ControlledByOtherError when another controller
// controls the stored object.
func (r *kindClient[T]) createOrUpdate(namespace string, desired T) error {
	return createOrUpdate(namespace, desired, objectCalls[T]{
		kind: r.kind, get: r.get, create: r.create, update: r.update, upToDate: r.upToDate, keep: r.keep,
		skipped: func() {
			r.logger.WithField("namespace", namespace).WithField(r.field, desired.GetName()).Debugf("%s already up to date, skipping update", r.noun)
		},
	})
}
