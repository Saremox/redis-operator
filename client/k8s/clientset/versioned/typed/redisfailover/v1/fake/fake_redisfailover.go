// Code generated by client-gen. DO NOT EDIT.

package fake

import (
	"context"

	v1 "github.com/spotahome/redis-operator/api/redisfailover/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	labels "k8s.io/apimachinery/pkg/labels"
	types "k8s.io/apimachinery/pkg/types"
	watch "k8s.io/apimachinery/pkg/watch"
	testing "k8s.io/client-go/testing"
)

// FakeRedisFailovers implements RedisFailoverInterface
type FakeRedisFailovers struct {
	Fake *FakeDatabasesV1
	ns   string
}

var redisfailoversResource = v1.SchemeGroupVersion.WithResource("redisfailovers")

var redisfailoversKind = v1.SchemeGroupVersion.WithKind("RedisFailover")

// Get takes name of the redisFailover, and returns the corresponding redisFailover object, and an error if there is any.
func (c *FakeRedisFailovers) Get(ctx context.Context, name string, options metav1.GetOptions) (result *v1.RedisFailover, err error) {
	obj, err := c.Fake.
		Invokes(testing.NewGetAction(redisfailoversResource, c.ns, name), &v1.RedisFailover{})

	if obj == nil {
		return nil, err
	}
	return obj.(*v1.RedisFailover), err
}

// List takes label and field selectors, and returns the list of RedisFailovers that match those selectors.
func (c *FakeRedisFailovers) List(ctx context.Context, opts metav1.ListOptions) (result *v1.RedisFailoverList, err error) {
	obj, err := c.Fake.
		Invokes(testing.NewListAction(redisfailoversResource, redisfailoversKind, c.ns, opts), &v1.RedisFailoverList{})

	if obj == nil {
		return nil, err
	}

	label, _, _ := testing.ExtractFromListOptions(opts)
	if label == nil {
		label = labels.Everything()
	}
	list := &v1.RedisFailoverList{ListMeta: obj.(*v1.RedisFailoverList).ListMeta}
	for _, item := range obj.(*v1.RedisFailoverList).Items {
		if label.Matches(labels.Set(item.Labels)) {
			list.Items = append(list.Items, item)
		}
	}
	return list, err
}

// Watch returns a watch.Interface that watches the requested redisFailovers.
func (c *FakeRedisFailovers) Watch(ctx context.Context, opts metav1.ListOptions) (watch.Interface, error) {
	return c.Fake.
		InvokesWatch(testing.NewWatchAction(redisfailoversResource, c.ns, opts))

}

// Create takes the representation of a redisFailover and creates it.  Returns the server's representation of the redisFailover, and an error, if there is any.
func (c *FakeRedisFailovers) Create(ctx context.Context, redisFailover *v1.RedisFailover, opts metav1.CreateOptions) (result *v1.RedisFailover, err error) {
	obj, err := c.Fake.
		Invokes(testing.NewCreateAction(redisfailoversResource, c.ns, redisFailover), &v1.RedisFailover{})

	if obj == nil {
		return nil, err
	}
	return obj.(*v1.RedisFailover), err
}

// Update takes the representation of a redisFailover and updates it. Returns the server's representation of the redisFailover, and an error, if there is any.
func (c *FakeRedisFailovers) Update(ctx context.Context, redisFailover *v1.RedisFailover, opts metav1.UpdateOptions) (result *v1.RedisFailover, err error) {
	obj, err := c.Fake.
		Invokes(testing.NewUpdateAction(redisfailoversResource, c.ns, redisFailover), &v1.RedisFailover{})

	if obj == nil {
		return nil, err
	}
	return obj.(*v1.RedisFailover), err
}

// UpdateStatus was generated because the type contains a Status member.
// Add a +genclient:noStatus comment above the type to avoid generating UpdateStatus().
func (c *FakeRedisFailovers) UpdateStatus(ctx context.Context, redisFailover *v1.RedisFailover, opts metav1.UpdateOptions) (*v1.RedisFailover, error) {
	obj, err := c.Fake.
		Invokes(testing.NewUpdateSubresourceAction(redisfailoversResource, "status", c.ns, redisFailover), &v1.RedisFailover{})

	if obj == nil {
		return nil, err
	}
	return obj.(*v1.RedisFailover), err
}

// Delete takes name of the redisFailover and deletes it. Returns an error if one occurs.
func (c *FakeRedisFailovers) Delete(ctx context.Context, name string, opts metav1.DeleteOptions) error {
	_, err := c.Fake.
		Invokes(testing.NewDeleteActionWithOptions(redisfailoversResource, c.ns, name, opts), &v1.RedisFailover{})

	return err
}

// DeleteCollection deletes a collection of objects.
func (c *FakeRedisFailovers) DeleteCollection(ctx context.Context, opts metav1.DeleteOptions, listOpts metav1.ListOptions) error {
	action := testing.NewDeleteCollectionAction(redisfailoversResource, c.ns, listOpts)

	_, err := c.Fake.Invokes(action, &v1.RedisFailoverList{})
	return err
}

// Patch applies the patch and returns the patched redisFailover.
func (c *FakeRedisFailovers) Patch(ctx context.Context, name string, pt types.PatchType, data []byte, opts metav1.PatchOptions, subresources ...string) (result *v1.RedisFailover, err error) {
	obj, err := c.Fake.
		Invokes(testing.NewPatchSubresourceAction(redisfailoversResource, c.ns, name, pt, data, subresources...), &v1.RedisFailover{})

	if obj == nil {
		return nil, err
	}
	return obj.(*v1.RedisFailover), err
}
