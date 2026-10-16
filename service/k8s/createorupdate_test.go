package k8s_test

import (
	"fmt"
	"slices"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	policyv1 "k8s.io/api/policy/v1"
	kubeerrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/kubernetes"
	kubefake "k8s.io/client-go/kubernetes/fake"
	kubetesting "k8s.io/client-go/testing"

	"github.com/saremox/redis-operator/log"
	"github.com/saremox/redis-operator/metrics"
	"github.com/saremox/redis-operator/service/k8s"
)

// recordLogger keeps the fields and the text of each debug line.
type recordLogger struct {
	log.Logger
	fields []string
	lines  *[]string
}

func (l recordLogger) With(key string, value any) log.Logger { return l.WithField(key, value) }

func (l recordLogger) WithField(key string, value any) log.Logger {
	return recordLogger{Logger: l.Logger, fields: append(slices.Clone(l.fields), fmt.Sprintf("%s=%v", key, value)), lines: l.lines}
}

func (l recordLogger) Debugf(format string, args ...any) {
	*l.lines = append(*l.lines, strings.Join(l.fields, " ")+": "+fmt.Sprintf(format, args...))
}

// recordK8sMetrics keeps each Kubernetes operation metric.
type recordK8sMetrics struct {
	metrics.Recorder
	calls []string
}

func (r *recordK8sMetrics) RecordK8sOperation(namespace, kind, object, operation, status, reason string) {
	r.calls = append(r.calls, strings.Join([]string{namespace, kind, object, operation, status, reason}, " "))
}

type createOrUpdateKind struct {
	kind, field, noun, serviceField string
	getLabel                        string // the object label of the GET metric
	created, updated, skipped       string
	object                          func(meta metav1.ObjectMeta) runtime.Object
	createOrUpdate                  func(cli kubernetes.Interface, logger log.Logger, rec metrics.Recorder, object runtime.Object) error
}

func createOrUpdateKinds() []createOrUpdateKind {
	return []createOrUpdateKind{
		{
			kind: "ConfigMap", field: "configMap", noun: "configmap", serviceField: "k8s.configMap", getLabel: metrics.NOT_APPLICABLE,
			created: "configMap created", updated: "configMap updated", skipped: "configmap already up to date, skipping update",
			object: func(meta metav1.ObjectMeta) runtime.Object { return &corev1.ConfigMap{ObjectMeta: meta} },
			createOrUpdate: func(cli kubernetes.Interface, logger log.Logger, rec metrics.Recorder, object runtime.Object) error {
				return k8s.NewConfigMapService(cli, logger, rec).CreateOrUpdateConfigMap("ns", object.(*corev1.ConfigMap))
			},
		},
		{
			kind: "Deployment", getLabel: "x", field: "deployment", noun: "deployment", serviceField: "k8s.deployment",
			created: "deployment created", updated: "deployment updated", skipped: "deployment already up to date, skipping update",
			object: func(meta metav1.ObjectMeta) runtime.Object { return &appsv1.Deployment{ObjectMeta: meta} },
			createOrUpdate: func(cli kubernetes.Interface, logger log.Logger, rec metrics.Recorder, object runtime.Object) error {
				return k8s.NewDeploymentService(cli, logger, rec).CreateOrUpdateDeployment("ns", object.(*appsv1.Deployment))
			},
		},
		{
			kind: "PodDisruptionBudget", getLabel: "x", field: "podDisruptionBudget", noun: "poddisruptionbudget", serviceField: "k8s.podDisruptionBudget",
			created: "podDisruptionBudget created", updated: "podDisruptionBudget updated", skipped: "pod disruption budget already up to date, skipping update",
			object: func(meta metav1.ObjectMeta) runtime.Object { return &policyv1.PodDisruptionBudget{ObjectMeta: meta} },
			createOrUpdate: func(cli kubernetes.Interface, logger log.Logger, rec metrics.Recorder, object runtime.Object) error {
				return k8s.NewPodDisruptionBudgetService(cli, logger, rec).CreateOrUpdatePodDisruptionBudget("ns", object.(*policyv1.PodDisruptionBudget))
			},
		},
		{
			kind: "ServiceAccount", getLabel: "x", field: "serviceAccount", noun: "serviceaccount", serviceField: "k8s.serviceAccount",
			created: "serviceAccount created", updated: "serviceAccount updated", skipped: "service account already up to date, skipping update",
			object: func(meta metav1.ObjectMeta) runtime.Object { return &corev1.ServiceAccount{ObjectMeta: meta} },
			createOrUpdate: func(cli kubernetes.Interface, logger log.Logger, rec metrics.Recorder, object runtime.Object) error {
				return k8s.NewServiceAccountService(cli, logger, rec).CreateOrUpdateServiceAccount("ns", object.(*corev1.ServiceAccount))
			},
		},
		{
			kind: "StatefulSet", getLabel: "x", field: "statefulSet", noun: "statefulset", serviceField: "k8s.statefulSet",
			created: "statefulSet created", updated: "statefulSet updated", skipped: "statefulset already up to date, skipping update",
			object: func(meta metav1.ObjectMeta) runtime.Object { return &appsv1.StatefulSet{ObjectMeta: meta} },
			createOrUpdate: func(cli kubernetes.Interface, logger log.Logger, rec metrics.Recorder, object runtime.Object) error {
				return k8s.NewStatefulSetService(cli, logger, rec).CreateOrUpdateStatefulSet("ns", object.(*appsv1.StatefulSet))
			},
		},
	}
}

func objectMeta(label string) metav1.ObjectMeta {
	controller := true
	return metav1.ObjectMeta{
		Name: "x", Namespace: "ns", Labels: map[string]string{"a": label},
		OwnerReferences: []metav1.OwnerReference{{APIVersion: "databases.spotahome.com/v1", Kind: "RedisFailover", Name: "rf", UID: "uid", Controller: &controller}},
	}
}

func verbs(cli *kubefake.Clientset) []string {
	actions := cli.Actions()
	out := make([]string, 0, len(actions))
	for _, a := range actions {
		out = append(out, a.GetVerb())
	}
	return out
}

// TestCreateOrUpdatePinsTheCallsMetricsAndLogs pins the API calls, the
// metrics and the debug lines of each CreateOrUpdate method.
func TestCreateOrUpdatePinsTheCallsMetricsAndLogs(t *testing.T) {
	for _, kind := range createOrUpdateKinds() {
		prefix := "service=" + kind.serviceField + " namespace=ns " + kind.field + "=x: "
		metric := func(operation, status, reason string) string {
			return strings.Join([]string{"ns", kind.kind, "x", operation, status, reason}, " ")
		}
		getMetric := func(status, reason string) string {
			return strings.Join([]string{"ns", kind.kind, kind.getLabel, "GET", status, reason}, " ")
		}
		getOK := getMetric(metrics.SUCCESS, metrics.NOT_APPLICABLE)
		for _, tc := range []struct {
			name        string
			stored      string
			wantVerbs   []string
			wantMetrics []string
			wantLines   []string
		}{
			{
				name: "create", wantVerbs: []string{"get", "create"},
				wantMetrics: []string{getMetric(metrics.FAIL, metrics.K8S_NOT_FOUND), metric("CREATE", metrics.SUCCESS, metrics.NOT_APPLICABLE)},
				wantLines:   []string{prefix + kind.created},
			},
			{
				name: "update", stored: "old", wantVerbs: []string{"get", "update"},
				wantMetrics: []string{getOK, metric("UPDATE", metrics.SUCCESS, metrics.NOT_APPLICABLE)},
				wantLines:   []string{prefix + kind.updated},
			},
			{
				name: "up to date", stored: "desired", wantVerbs: []string{"get"},
				wantMetrics: []string{getOK},
				wantLines:   []string{prefix + kind.skipped},
			},
		} {
			t.Run(kind.kind+" "+tc.name, func(t *testing.T) {
				cli := kubefake.NewClientset()
				if tc.stored != "" {
					cli = kubefake.NewClientset(kind.object(objectMeta(tc.stored)))
				}
				var lines []string
				rec := &recordK8sMetrics{Recorder: metrics.Dummy}

				err := kind.createOrUpdate(cli, recordLogger{Logger: log.Dummy, lines: &lines}, rec, kind.object(objectMeta("desired")))

				require.NoError(t, err)
				assert.Equal(t, tc.wantVerbs, verbs(cli))
				assert.Equal(t, tc.wantMetrics, rec.calls)
				assert.Equal(t, tc.wantLines, lines)
			})
		}
	}
}

// TestCreateOrUpdateReturnsTheGetError checks that a failed read other than
// NotFound stops CreateOrUpdate before a write.
func TestCreateOrUpdateReturnsTheGetError(t *testing.T) {
	for _, kind := range createOrUpdateKinds() {
		t.Run(kind.kind, func(t *testing.T) {
			cli := kubefake.NewClientset()
			cli.PrependReactor("get", "*", func(action kubetesting.Action) (bool, runtime.Object, error) {
				gr := schema.GroupResource{Group: action.GetResource().Group, Resource: action.GetResource().Resource}
				return true, nil, kubeerrors.NewForbidden(gr, "x", nil)
			})
			var lines []string

			err := kind.createOrUpdate(cli, recordLogger{Logger: log.Dummy, lines: &lines}, metrics.Dummy, kind.object(objectMeta("desired")))

			require.Error(t, err)
			assert.True(t, kubeerrors.IsForbidden(err), err.Error())
			assert.True(t, strings.HasPrefix(err.Error(), "get "+kind.noun+" ns/x: "), err.Error())
			assert.Equal(t, []string{"get"}, verbs(cli))
			assert.Empty(t, lines)
		})
	}
}

// TestCreateOrUpdateLogsNoFailedWrite checks the error and the metric of a
// failed create and a failed update, which log no debug line.
func TestCreateOrUpdateLogsNoFailedWrite(t *testing.T) {
	for _, kind := range createOrUpdateKinds() {
		for _, tc := range []struct{ verb, operation, stored string }{{"create", "CREATE", ""}, {"update", "UPDATE", "old"}} {
			t.Run(kind.kind+" "+tc.verb, func(t *testing.T) {
				cli := kubefake.NewClientset()
				if tc.stored != "" {
					cli = kubefake.NewClientset(kind.object(objectMeta(tc.stored)))
				}
				cli.PrependReactor(tc.verb, "*", func(action kubetesting.Action) (bool, runtime.Object, error) {
					gr := schema.GroupResource{Group: action.GetResource().Group, Resource: action.GetResource().Resource}
					return true, nil, kubeerrors.NewConflict(gr, "x", nil)
				})
				var lines []string
				rec := &recordK8sMetrics{Recorder: metrics.Dummy}

				err := kind.createOrUpdate(cli, recordLogger{Logger: log.Dummy, lines: &lines}, rec, kind.object(objectMeta("desired")))

				require.Error(t, err)
				assert.True(t, kubeerrors.IsConflict(err), err.Error())
				assert.True(t, strings.HasPrefix(err.Error(), tc.verb+" "+kind.noun+" ns/x: "), err.Error())
				assert.Equal(t, strings.Join([]string{"ns", kind.kind, "x", tc.operation, metrics.FAIL, metrics.K8S_MISC}, " "), rec.calls[len(rec.calls)-1])
				assert.Empty(t, lines)
			})
		}
	}
}
