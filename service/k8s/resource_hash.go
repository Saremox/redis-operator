package k8s

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
)

// resourceHashAnnotationKey holds the hash of the object the operator last
// applied. Comparing it against the hash of the freshly built object tells us
// whether an update would actually change anything.
const resourceHashAnnotationKey = "databases.spotahome.com/resource-hash"

// annotated is the subset of metav1.Object needed to read and stamp the hash.
type annotated interface {
	GetAnnotations() map[string]string
	SetAnnotations(map[string]string)
}

// ServiceOption configures behaviour shared by the k8s services.
type ServiceOption func(*serviceOptions)

type serviceOptions struct {
	hashingEnabled bool
}

// WithObjectHashing makes the CreateOrUpdate calls skip the update when the
// object they would write is identical to the one they last applied.
func WithObjectHashing(enabled bool) ServiceOption {
	return func(o *serviceOptions) {
		o.hashingEnabled = enabled
	}
}

func newServiceOptions(opts []ServiceOption) serviceOptions {
	var o serviceOptions
	for _, opt := range opts {
		opt(&o)
	}
	return o
}

// resourceHash digests the object as JSON. Marshalling these typed structs is
// deterministic - fields follow declaration order and map keys are sorted - so
// the same object always yields the same hash.
func resourceHash(obj any) (string, error) {
	data, err := json.Marshal(obj)
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:]), nil
}

// hashObject digests obj ignoring any hash annotation it already carries, so
// the result never depends on a previous run's value.
func hashObject(obj annotated) (string, error) {
	current := obj.GetAnnotations()
	if _, found := current[resourceHashAnnotationKey]; found {
		without := make(map[string]string, len(current))
		for k, v := range current {
			if k != resourceHashAnnotationKey {
				without[k] = v
			}
		}
		obj.SetAnnotations(without)
		defer obj.SetAnnotations(current)
	}
	return resourceHash(obj)
}

// addHashAnnotation stamps the object with the hash of itself. It must be
// called before the stored ResourceVersion is copied onto the object,
// otherwise that value feeds the hash and it changes on every reconcile.
func addHashAnnotation(obj annotated) error {
	hash, err := hashObject(obj)
	if err != nil {
		return err
	}

	annotations := obj.GetAnnotations()
	if annotations == nil {
		annotations = map[string]string{}
	}
	annotations[resourceHashAnnotationKey] = hash
	obj.SetAnnotations(annotations)

	return nil
}

// shouldUpdate reports whether desired differs from what was last applied.
// It errs towards updating: an object that has never been stamped, or one we
// fail to hash, is always written. A redundant write is harmless, a skipped
// one is not.
func shouldUpdate(desired, stored annotated) bool {
	storedHash, found := stored.GetAnnotations()[resourceHashAnnotationKey]
	if !found {
		return true
	}

	desiredHash, err := hashObject(desired)
	if err != nil {
		return true
	}

	return desiredHash != storedHash
}
