package instances

import (
	"context"
	"log/slog"
	"os"
	"path/filepath"
	"slices"
	"testing"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/kubernetes/fake"
	k8stesting "k8s.io/client-go/testing"

	redisfailoverv1 "github.com/saremox/redis-operator/api/redisfailover/v1"
	rffake "github.com/saremox/redis-operator/client/k8s/clientset/versioned/fake"
	"github.com/saremox/redis-operator/test/soak/internal/config"
)

func TestLoadTemplate(t *testing.T) {
	rf, err := LoadTemplate("testdata/chain.yaml", "chain")
	if err != nil {
		t.Fatal(err)
	}

	if rf.Spec.Redis.Replicas != 2 || rf.Spec.Auth.SecretPath != "chain-auth" || rf.Spec.Redis.Storage.PersistentVolumeClaim == nil {
		t.Errorf("template spec: %+v", rf.Spec)
	}

	dir := t.TempDir()
	for name, body := range map[string]string{
		"unknown field": "apiVersion: databases.spotahome.com/v1\nkind: RedisFailover\nmetadata: {name: chain}\nspec: {redis: {replica: 3}}\n",
		"other kind":    "apiVersion: v1\nkind: Secret\nmetadata: {name: chain}\n",
		"other names": "apiVersion: databases.spotahome.com/v1\nkind: RedisFailover\nmetadata: {name: x}\n---\n" +
			"apiVersion: databases.spotahome.com/v1\nkind: RedisFailover\nmetadata: {name: y}\n",
		"not yaml": "{",
	} {
		path := filepath.Join(dir, "t.yaml")
		if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
		if _, err := LoadTemplate(path, "chain"); err == nil {
			t.Errorf("%s: expected an error", name)
		}
	}
}

// The tester creates every instance of the shipped config from its
// template.
func TestShippedTemplates(t *testing.T) {
	cfg, err := config.Load("../../deploy/config.yaml")
	if err != nil {
		t.Fatal(err)
	}
	for _, in := range cfg.Instances {
		if rf, err := LoadTemplate(in.Template, in.Name); err != nil || rf.Name != in.Name {
			t.Errorf("%s: %v", in.Name, err)
		}
	}
}

func TestBuild(t *testing.T) {
	i, err := New(config.Instance{Name: "chain", Namespace: "ns", Template: "testdata/chain.yaml"}, nil, nil, slog.New(slog.DiscardHandler))
	if err != nil {
		t.Fatal(err)
	}
	rf := i.Build("valkey/valkey:8.1.10-alpine", "")
	if rf.Name != "chain" || rf.Namespace != "ns" || rf.Labels["soak"] != "true" {
		t.Errorf("metadata: %+v", rf.ObjectMeta)
	}
	if rf.Spec.Redis.Image != "valkey/valkey:8.1.10-alpine" || rf.Spec.Sentinel.Image != "" {
		t.Errorf("images: %q %q", rf.Spec.Redis.Image, rf.Spec.Sentinel.Image)
	}
	if i.Build("", "").Spec.Redis.Image != "redis:7.2.16-alpine" {
		t.Error("the template's image isn't kept")
	}
}

func object(kind, name string, labels map[string]string) metav1.ObjectMeta {
	return metav1.ObjectMeta{Name: name, Namespace: "ns", Labels: labels, UID: types.UID("old-" + kind)}
}

// A reset deletes the RedisFailover, waits until the operator's objects
// are gone, deletes the volumes and the Secret, and creates both again.
func TestReset(t *testing.T) {
	labels := map[string]string{"app.kubernetes.io/part-of": "redis-failover", "app.kubernetes.io/name": "chain", "app.kubernetes.io/component": "redis"}
	other := map[string]string{"app.kubernetes.io/part-of": "redis-failover", "app.kubernetes.io/name": "other", "app.kubernetes.io/component": "redis"}
	kube := fake.NewClientset(
		&corev1.Secret{ObjectMeta: object("secret", "chain-auth", nil), Data: map[string][]byte{"password": []byte("old")}},
		&appsv1.StatefulSet{ObjectMeta: object("sts", "rfr-chain", labels)},
		&corev1.Pod{ObjectMeta: object("pod", "rfr-chain-0", labels)},
		&corev1.PersistentVolumeClaim{ObjectMeta: object("pvc", "redis-data-rfr-chain-0", labels)},
		&corev1.PersistentVolumeClaim{ObjectMeta: object("pvc", "redis-data-rfr-chain-1", labels)},
		&corev1.PersistentVolumeClaim{ObjectMeta: object("pvc", "redis-data-rfr-other-0", other)},
	)
	old := &redisfailoverv1.RedisFailover{ObjectMeta: object("rf", "chain", nil)}
	old.Spec.Redis.Image = "valkey/valkey:9.1.2-alpine"
	rfs := rffake.NewSimpleClientset(old)
	// The garbage collector removes what the operator made once the
	// RedisFailover is gone, a little later.
	rfs.PrependReactor("delete", "redisfailovers", func(k8stesting.Action) (bool, runtime.Object, error) {
		go func() {
			time.Sleep(20 * time.Millisecond)
			_ = kube.AppsV1().StatefulSets("ns").Delete(context.Background(), "rfr-chain", metav1.DeleteOptions{})
			_ = kube.CoreV1().Pods("ns").Delete(context.Background(), "rfr-chain-0", metav1.DeleteOptions{})
		}()
		return false, nil, nil
	})
	i, err := New(config.Instance{Name: "chain", Namespace: "ns", Template: "testdata/chain.yaml"}, kube, rfs, slog.New(slog.DiscardHandler))
	if err != nil {
		t.Fatal(err)
	}
	i.poll = time.Millisecond
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	if err := i.Delete(ctx); err != nil {
		t.Fatal(err)
	}
	pvcs, _ := kube.CoreV1().PersistentVolumeClaims("ns").List(ctx, metav1.ListOptions{})
	if len(pvcs.Items) != 1 || pvcs.Items[0].Name != "redis-data-rfr-other-0" {
		t.Errorf("volumes left: %v", pvcs.Items)
	}
	if _, err := kube.CoreV1().Secrets("ns").Get(ctx, "chain-auth", metav1.GetOptions{}); err == nil {
		t.Error("the Secret is still there")
	}

	if err := i.Create(ctx, "redis:7.2.16-alpine", ""); err != nil {
		t.Fatal(err)
	}
	rf, err := rfs.DatabasesV1().RedisFailovers("ns").Get(ctx, "chain", metav1.GetOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if rf.Spec.Redis.Image != "redis:7.2.16-alpine" || rf.Spec.Redis.Replicas != 2 || rf.UID == "old-rf" {
		t.Errorf("recreated: %+v", rf)
	}
	secret, err := kube.CoreV1().Secrets("ns").Get(ctx, "chain-auth", metav1.GetOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if p := string(secret.Data["password"]); p == "" || p == "old" {
		t.Errorf("password %q", p)
	}

	var order []string
	for _, a := range slices.Concat(rfs.Actions(), kube.Actions()) {
		if a.GetVerb() == "delete" || a.GetVerb() == "create" {
			order = append(order, a.GetVerb()+" "+a.GetResource().Resource)
		}
	}
	// The fakes record their actions apart: within each the order holds.
	for _, want := range [][]string{
		{"delete redisfailovers", "create redisfailovers"},
		{"delete persistentvolumeclaims", "delete persistentvolumeclaims", "delete secrets", "create secrets"},
	} {
		if !isSubsequence(want, order) {
			t.Errorf("actions %v don't contain %v in order", order, want)
		}
	}
}

func isSubsequence(want, got []string) bool {
	i := 0
	for _, g := range got {
		if i < len(want) && g == want[i] {
			i++
		}
	}
	return i == len(want)
}
