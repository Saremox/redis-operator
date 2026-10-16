package redisfailover

import (
	"cmp"
	"errors"
	"slices"

	"github.com/saremox/redis-operator/service/k8s"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	redisfailoverv1 "github.com/saremox/redis-operator/api/redisfailover/v1"
	rfservice "github.com/saremox/redis-operator/operator/redisfailover/service"
)

// passwordState is the password the Redis pods and the Sentinels were last
// brought onto.
type passwordState struct {
	redis string
	// applied is the password that the running Redis pods were last changed to.
	// It is not redis while a pod yet to start keeps redis in use.
	applied  string
	sentinel string
	// stale makes the next reconcile check the Redis pods again, also when
	// redis is the password: a container restart of a pod on a stale template
	// gives Redis another password.
	stale bool
	// retrySecret makes the next reconcile read and write the Secret again
	// after a temporary error.
	retrySecret bool
}

func passwordKey(rf *redisfailoverv1.RedisFailover) string {
	return rf.Namespace + "/" + rf.Name
}

// Keys of the Secret that keeps redis and applied of passwordState. The
// Sentinel password needs no key: it is set without the one it replaces.
const (
	appliedSecretRedisKey   = "redis"
	appliedSecretAppliedKey = "applied"
)

// loadAppliedPasswords reads the passwords that saveAppliedPasswords kept
// before an operator restart. ok is false when the Secret must not be written
// in this reconcile: an unknown content must not be replaced, and another
// owner keeps its Secret. retry is true for a temporary error.
func (r *RedisFailoverHandler) loadAppliedPasswords(rf *redisfailoverv1.RedisFailover) (saved passwordState, ok, retry bool) {
	name := rfservice.GetRedisAuthAppliedName(rf)
	secret, err := r.k8sservice.GetSecret(rf.Namespace, name)
	if apierrors.IsNotFound(err) {
		r.forgetAppliedSecretLog(rf, true)
		return passwordState{}, true, false
	}
	if err == nil {
		err = k8s.CheckSameController("Secret", secret, &metav1.ObjectMeta{OwnerReferences: r.createOwnerReferences(rf)})
	}
	if err != nil {
		r.logAppliedSecretError(rf, true, "unable to read the applied passwords from Secret "+name, err)
		return passwordState{}, false, retryableSecretError(err)
	}
	r.forgetAppliedSecretLog(rf, true)
	return passwordState{
		redis:   string(secret.Data[appliedSecretRedisKey]),
		applied: string(secret.Data[appliedSecretAppliedKey]),
	}, true, false
}

// retryableSecretError is false for a missing RBAC verb and for another
// owner: a retry at each reconcile cannot change them.
func retryableSecretError(err error) bool {
	var other *k8s.ControlledByOtherError
	return !apierrors.IsForbidden(err) && !errors.As(err, &other)
}

// saveAppliedPasswords keeps the passwords of state in a Secret, so that an
// operator restart does not forget them. A failure costs only that memory, so
// it does not stop the reconcile.
func (r *RedisFailoverHandler) saveAppliedPasswords(rf *redisfailoverv1.RedisFailover, state *passwordState) {
	secret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name:            rfservice.GetRedisAuthAppliedName(rf),
			Namespace:       rf.Namespace,
			Labels:          r.getLabels(rf),
			OwnerReferences: r.createOwnerReferences(rf),
		},
		Data: map[string][]byte{
			appliedSecretRedisKey:   []byte(state.redis),
			appliedSecretAppliedKey: []byte(state.applied),
		},
	}
	err := r.k8sservice.CreateOrUpdateSecret(rf.Namespace, secret)
	if err == nil {
		r.forgetAppliedSecretLog(rf, false)
		return
	}
	state.retrySecret = retryableSecretError(err)
	r.logAppliedSecretError(rf, false, "unable to save the applied passwords in Secret "+secret.Name+", an operator restart forgets them", err)
}

// appliedSecretLog is the last error with the applied passwords Secret. read
// is true for an error of a read.
type appliedSecretLog struct {
	read bool
	err  string
}

func (r *RedisFailoverHandler) logAppliedSecretError(rf *redisfailoverv1.RedisFailover, read bool, msg string, err error) {
	logger := r.logger.WithField("namespace", rf.Namespace).WithField("name", rf.Name)
	logged := appliedSecretLog{read: read, err: err.Error()}
	prev, _ := r.appliedSecretLogged.Swap(passwordKey(rf), logged)
	if last, ok := prev.(appliedSecretLog); ok && last.err == logged.err {
		logger.Debugf("%s: %v", msg, err)
		return
	}
	logger.Warningf("%s: %v", msg, err)
}

// forgetAppliedSecretLog drops the logged error after a success, so that the
// same error logs a Warning again. A read does not show that a write works.
func (r *RedisFailoverHandler) forgetAppliedSecretLog(rf *redisfailoverv1.RedisFailover, read bool) {
	key := passwordKey(rf)
	v, _ := r.appliedSecretLogged.Load(key)
	if last, ok := v.(appliedSecretLog); ok && (!read || last.read) {
		r.appliedSecretLogged.CompareAndDelete(key, v)
	}
}

// applyPassword applies a changed auth secret to the running Redis and the
// Sentinels. Nothing is checked while both are on the secret.
func (r *RedisFailoverHandler) applyPassword(rf *redisfailoverv1.RedisFailover) error {
	password, err := k8s.GetRedisPassword(r.k8sservice, rf)
	if err != nil {
		return err
	}
	key := passwordKey(rf)
	v, known := r.passwords.Load(key)
	state, _ := v.(passwordState)
	if known && state.current(password) {
		return nil
	}

	saved, save := state, true
	if !known || state.retrySecret {
		saved, save, state.retrySecret = r.loadAppliedPasswords(rf)
	}
	if err := r.applyRedisPassword(rf, password, known, saved, &state); err != nil {
		return err
	}
	if save && (state.redis != saved.redis || state.applied != saved.applied) {
		r.saveAppliedPasswords(rf, &state)
	}

	// Every running Redis now accepts the password, so the Sentinels need it
	// to reach them.
	if err := r.applySentinelPassword(rf, password, &state); err != nil {
		return err
	}
	// Without the Secret, state.redis can miss the password of a pod yet to
	// start, so the next reconcile starts again without a state.
	if !known && !save && state.retrySecret {
		return nil
	}
	r.passwords.Store(key, state)
	return nil
}

// current reports whether the Redis pods and the Sentinels are on password,
// and no check or Secret write is due.
func (s passwordState) current(password string) bool {
	return s.redis == password && s.applied == password && s.sentinel == password && !s.stale && !s.retrySecret
}

// applyRedisPassword brings the running Redis pods onto password, unless the
// known state shows them on it already. saved holds the passwords of the
// Secret.
func (r *RedisFailoverHandler) applyRedisPassword(rf *redisfailoverv1.RedisFailover, password string, known bool, saved passwordState, state *passwordState) error {
	if known && !state.stale && state.redis == password && state.applied == password {
		return nil
	}
	previous := []string{password}
	if known {
		previous = []string{state.redis}
		if state.applied != state.redis {
			previous = append(previous, state.applied)
		}
	} else {
		for _, p := range []string{saved.redis, saved.applied} {
			if p != "" && !slices.Contains(previous, p) {
				previous = append(previous, p)
			}
		}
	}
	complete, stale, err := r.rfHealer.ApplyPassword(rf, password, previous)
	if err != nil {
		return err
	}
	state.applied = password
	state.stale = stale
	// A pod yet to start keeps the old password in play. With none known,
	// not even in the Secret, the one every running pod accepts is the
	// best there is.
	if complete {
		state.redis = password
	} else if !known {
		state.redis = cmp.Or(saved.redis, password)
	}
	return nil
}

// applySentinelPassword gives the Sentinels the password.
func (r *RedisFailoverHandler) applySentinelPassword(rf *redisfailoverv1.RedisFailover, password string, state *passwordState) error {
	if state.sentinel == password {
		return nil
	}
	complete, err := r.rfHealer.ApplySentinelPassword(rf, password)
	if err != nil {
		return err
	}
	if complete {
		state.sentinel = password
	}
	return nil
}
