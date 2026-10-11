// Package operator reads the state of the installed redis-operator.
package operator

import (
	"context"
	"strings"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
)

// Version returns the image tag of the operator Deployment's first
// container.
func Version(ctx context.Context, kube kubernetes.Interface, namespace, name string) (string, error) {
	d, err := kube.AppsV1().Deployments(namespace).Get(ctx, name, metav1.GetOptions{})
	if err != nil {
		return "", err
	}
	return imageTag(d.Spec.Template.Spec.Containers[0].Image), nil
}

func imageTag(image string) string {
	image, _, _ = strings.Cut(image, "@")
	i := strings.LastIndex(image, ":")
	if i < 0 || strings.Contains(image[i:], "/") {
		return "latest"
	}
	return image[i+1:]
}
