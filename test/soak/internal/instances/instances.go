// Package instances creates the RedisFailovers the tester owns from their
// templates, and recreates them from scratch.
package instances

import (
	"bufio"
	"bytes"
	"context"
	crand "crypto/rand"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	utilyaml "k8s.io/apimachinery/pkg/util/yaml"
	"k8s.io/client-go/kubernetes"
	"sigs.k8s.io/yaml"

	redisfailoverv1 "github.com/saremox/redis-operator/api/redisfailover/v1"
	"github.com/saremox/redis-operator/client/k8s/clientset/versioned"
	"github.com/saremox/redis-operator/test/soak/internal/auth"
	"github.com/saremox/redis-operator/test/soak/internal/config"
	"github.com/saremox/redis-operator/test/soak/internal/poll"
)

// LoadTemplate reads the RedisFailover of that name from a file of
// manifests, or the only one of the file. The instance sets its name and
// namespace.
func LoadTemplate(path, name string) (*redisfailoverv1.RedisFailover, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	r := utilyaml.NewYAMLReader(bufio.NewReader(bytes.NewReader(b)))
	var rfs []*redisfailoverv1.RedisFailover
	for {
		doc, err := r.Read()
		if errors.Is(err, io.EOF) {
			break
		}
		var rf redisfailoverv1.RedisFailover
		if err == nil {
			err = yaml.UnmarshalStrict(doc, &rf)
		}
		if err != nil {
			return nil, fmt.Errorf("%s: %w", path, err)
		}
		if rf.Kind != "RedisFailover" {
			return nil, fmt.Errorf("%s: kind %q, want RedisFailover", path, rf.Kind)
		}
		if rf.Name == name {
			return &rf, nil
		}
		rfs = append(rfs, &rf)
	}
	if len(rfs) != 1 {
		return nil, fmt.Errorf("%s: no RedisFailover %s", path, name)
	}
	return rfs[0], nil
}

// Instance makes an instance's RedisFailover from its template.
type Instance struct {
	in       config.Instance
	template *redisfailoverv1.RedisFailover
	kube     kubernetes.Interface
	rfs      versioned.Interface
	// poll is how often a wait checks again.
	poll time.Duration
	log  *slog.Logger
}

func New(in config.Instance, kube kubernetes.Interface, rfs versioned.Interface, log *slog.Logger) (*Instance, error) {
	t, err := LoadTemplate(in.Template, in.Name)
	if err != nil {
		return nil, err
	}
	return &Instance{
		in:       in,
		template: t,
		kube:     kube,
		rfs:      rfs,
		poll:     time.Second,
		log:      log.With("rf", in.Name, "namespace", in.Namespace, "mode", in.Mode),
	}, nil
}

// Build returns the RedisFailover the template makes, on the given redis
// and Sentinel images; an empty image keeps the template's.
func (i *Instance) Build(redisImage, sentinelImage string) *redisfailoverv1.RedisFailover {
	rf := &redisfailoverv1.RedisFailover{
		TypeMeta: i.template.TypeMeta,
		ObjectMeta: metav1.ObjectMeta{
			Name:        i.in.Name,
			Namespace:   i.in.Namespace,
			Labels:      i.template.Labels,
			Annotations: i.template.Annotations,
		},
		Spec: *i.template.Spec.DeepCopy(),
	}
	if redisImage != "" {
		rf.Spec.Redis.Image = redisImage
	}
	if sentinelImage != "" {
		rf.Spec.Sentinel.Image = sentinelImage
	}
	return rf
}

// Ensure creates the RedisFailover, and its auth Secret, unless it exists.
func (i *Instance) Ensure(ctx context.Context, redisImage, sentinelImage string) error {
	_, err := i.rfs.DatabasesV1().RedisFailovers(i.in.Namespace).Get(ctx, i.in.Name, metav1.GetOptions{})
	if !apierrors.IsNotFound(err) {
		return err
	}
	if err := i.Create(ctx, redisImage, sentinelImage); err != nil {
		return err
	}
	i.log.Info("instance created from its template", "redis_image", redisImage, "sentinel_image", sentinelImage)
	return nil
}

// Create creates the RedisFailover on the given images, and its auth
// Secret with a new password if it has none.
func (i *Instance) Create(ctx context.Context, redisImage, sentinelImage string) error {
	rf := i.Build(redisImage, sentinelImage)
	if name := rf.Spec.Auth.SecretPath; name != "" {
		secrets := i.kube.CoreV1().Secrets(i.in.Namespace)
		_, err := secrets.Get(ctx, name, metav1.GetOptions{})
		if apierrors.IsNotFound(err) {
			_, err = secrets.Create(ctx, auth.Secret(nil, i.in.Namespace, name, crand.Text()), metav1.CreateOptions{})
		}
		if err != nil {
			return fmt.Errorf("auth secret %s: %w", name, err)
		}
	}
	_, err := i.rfs.DatabasesV1().RedisFailovers(i.in.Namespace).Create(ctx, rf, metav1.CreateOptions{})
	return err
}

// Delete deletes the RedisFailover, its volumes and the auth Secret its
// template names, and waits until they, and every pod, StatefulSet and
// Deployment the operator made for it, are gone.
func (i *Instance) Delete(ctx context.Context) error {
	ns, name := i.in.Namespace, i.in.Name
	err := i.rfs.DatabasesV1().RedisFailovers(ns).Delete(ctx, name, metav1.DeleteOptions{})
	if err != nil && !apierrors.IsNotFound(err) {
		return err
	}
	selector := metav1.ListOptions{LabelSelector: "app.kubernetes.io/part-of=redis-failover,app.kubernetes.io/name=" + name}
	err = i.wait(ctx, func() error {
		if _, err := i.rfs.DatabasesV1().RedisFailovers(ns).Get(ctx, name, metav1.GetOptions{}); !apierrors.IsNotFound(err) {
			return errors.Join(errors.New("the RedisFailover is still there"), err)
		}
		if _, err := i.kube.AppsV1().StatefulSets(ns).Get(ctx, "rfr-"+name, metav1.GetOptions{}); !apierrors.IsNotFound(err) {
			return errors.Join(errors.New("the redis StatefulSet is still there"), err)
		}
		if _, err := i.kube.AppsV1().Deployments(ns).Get(ctx, "rfs-"+name, metav1.GetOptions{}); !apierrors.IsNotFound(err) {
			return errors.Join(errors.New("the Sentinel Deployment is still there"), err)
		}
		pods, err := i.kube.CoreV1().Pods(ns).List(ctx, selector)
		if err != nil {
			return err
		}
		if n := len(pods.Items); n > 0 {
			return fmt.Errorf("%d pods left", n)
		}
		return nil
	})
	if err != nil {
		return err
	}
	// The operator owns the volumes unless keepAfterDeletion is set, and
	// the StatefulSet keeps them either way: delete what is left.
	pvcs := i.kube.CoreV1().PersistentVolumeClaims(ns)
	err = i.wait(ctx, func() error {
		list, err := pvcs.List(ctx, selector)
		if err != nil {
			return err
		}
		for _, p := range list.Items {
			if p.DeletionTimestamp == nil {
				if err := pvcs.Delete(ctx, p.Name, metav1.DeleteOptions{}); err != nil && !apierrors.IsNotFound(err) {
					return err
				}
			}
		}
		if n := len(list.Items); n > 0 {
			return fmt.Errorf("%d volumes left", n)
		}
		return nil
	})
	if err != nil {
		return err
	}
	if secret := i.template.Spec.Auth.SecretPath; secret != "" {
		err := i.kube.CoreV1().Secrets(ns).Delete(ctx, secret, metav1.DeleteOptions{})
		if err != nil && !apierrors.IsNotFound(err) {
			return err
		}
	}
	return nil
}

func (i *Instance) wait(ctx context.Context, f func() error) error {
	return poll.Until(ctx, i.poll, 0, func(context.Context) error { return f() })
}
