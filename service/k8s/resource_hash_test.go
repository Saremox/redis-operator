package k8s

import (
	"testing"

	"github.com/stretchr/testify/assert"
	appsv1 "k8s.io/api/apps/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

func hashTestDeployment() *appsv1.Deployment {
	replicas := int32(3)
	return &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "testdeployment",
			Namespace: "testns",
		},
		Spec: appsv1.DeploymentSpec{
			Replicas: &replicas,
		},
	}
}

func TestResourceHashIsStable(t *testing.T) {
	assert := assert.New(t)

	first, err := resourceHash(hashTestDeployment())
	assert.NoError(err)
	second, err := resourceHash(hashTestDeployment())
	assert.NoError(err)

	assert.Equal(first, second, "the same object must always hash the same")
	assert.NotEmpty(first)
}

func TestResourceHashChangesWithContent(t *testing.T) {
	assert := assert.New(t)

	base, err := resourceHash(hashTestDeployment())
	assert.NoError(err)

	changed := hashTestDeployment()
	replicas := int32(5)
	changed.Spec.Replicas = &replicas

	other, err := resourceHash(changed)
	assert.NoError(err)

	assert.NotEqual(base, other, "a spec change must change the hash")
}

func TestResourceHashIgnoresAnnotationInsertionOrder(t *testing.T) {
	assert := assert.New(t)

	first := hashTestDeployment()
	first.Annotations = map[string]string{}
	first.Annotations["a"] = "1"
	first.Annotations["b"] = "2"

	second := hashTestDeployment()
	second.Annotations = map[string]string{}
	second.Annotations["b"] = "2"
	second.Annotations["a"] = "1"

	firstHash, err := hashObject(first)
	assert.NoError(err)
	secondHash, err := hashObject(second)
	assert.NoError(err)

	assert.Equal(firstHash, secondHash, "map ordering must not affect the hash")
}

func TestHashObjectIgnoresExistingHashAnnotation(t *testing.T) {
	assert := assert.New(t)

	clean := hashTestDeployment()
	cleanHash, err := hashObject(clean)
	assert.NoError(err)

	stamped := hashTestDeployment()
	stamped.Annotations = map[string]string{resourceHashAnnotationKey: "whatever-was-there-before"}
	stampedHash, err := hashObject(stamped)
	assert.NoError(err)

	assert.Equal(cleanHash, stampedHash, "the stored hash must not feed back into the new hash")
	assert.Equal("whatever-was-there-before", stamped.Annotations[resourceHashAnnotationKey],
		"hashing must leave the object as it found it")
}

func TestAddHashAnnotation(t *testing.T) {
	assert := assert.New(t)

	t.Run("stamps an object without annotations", func(t *testing.T) {
		d := hashTestDeployment()
		assert.NoError(addHashAnnotation(d))
		assert.NotEmpty(d.Annotations[resourceHashAnnotationKey])
	})

	t.Run("keeps existing annotations", func(t *testing.T) {
		d := hashTestDeployment()
		d.Annotations = map[string]string{"keep": "me"}
		assert.NoError(addHashAnnotation(d))
		assert.Equal("me", d.Annotations["keep"])
		assert.NotEmpty(d.Annotations[resourceHashAnnotationKey])
	})

	t.Run("re-stamping the same object is idempotent", func(t *testing.T) {
		d := hashTestDeployment()
		assert.NoError(addHashAnnotation(d))
		first := d.Annotations[resourceHashAnnotationKey]
		assert.NoError(addHashAnnotation(d))
		assert.Equal(first, d.Annotations[resourceHashAnnotationKey])
	})
}

func TestShouldUpdate(t *testing.T) {
	assert := assert.New(t)

	t.Run("updates when the stored object was never stamped", func(t *testing.T) {
		assert.True(shouldUpdate(hashTestDeployment(), hashTestDeployment()))
	})

	t.Run("skips when the stored hash matches", func(t *testing.T) {
		stored := hashTestDeployment()
		assert.NoError(addHashAnnotation(stored))

		assert.False(shouldUpdate(hashTestDeployment(), stored))
	})

	t.Run("updates when the desired object changed", func(t *testing.T) {
		stored := hashTestDeployment()
		assert.NoError(addHashAnnotation(stored))

		desired := hashTestDeployment()
		replicas := int32(5)
		desired.Spec.Replicas = &replicas

		assert.True(shouldUpdate(desired, stored))
	})
}

func TestNewServiceOptions(t *testing.T) {
	assert := assert.New(t)

	assert.False(newServiceOptions(nil).hashingEnabled, "hashing must be off unless asked for")
	assert.True(newServiceOptions([]ServiceOption{WithObjectHashing(true)}).hashingEnabled)
	assert.False(newServiceOptions([]ServiceOption{WithObjectHashing(false)}).hashingEnabled)
}
