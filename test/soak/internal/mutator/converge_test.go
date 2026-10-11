package mutator

import (
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

type predicateCase struct {
	name   string
	change func(*state)
	ok     bool
}

func runPredicate(t *testing.T, converged func(state) error, cases []predicateCase) {
	t.Helper()
	runPredicateOn(t, testState, converged, cases)
}

func runPredicateOn(t *testing.T, base func() state, converged func(state) error, cases []predicateCase) {
	t.Helper()
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			s := base()
			c.change(&s)
			err := converged(s)
			if (err == nil) != c.ok {
				t.Errorf("converged = %v, want ok %v", err, c.ok)
			}
		})
	}
}

func TestRedisReplicasConverged(t *testing.T) {
	scaled := func(s *state) {
		four := int32(4)
		s.sts.Spec.Replicas = &four
		s.sts.Generation, s.sts.Status.ObservedGeneration = 3, 3
		s.sts.Status.Replicas, s.sts.Status.ReadyReplicas, s.sts.Status.UpdatedReplicas = 4, 4, 4
		s.redis = append(s.redis, readyPod("rfr-x-3", "u3", roleReplica))
	}
	runPredicate(t, redisReplicasConverged(4), []predicateCase{
		{"converged", scaled, true},
		{"not picked up by the operator", func(*state) {}, false},
		{"not observed", func(s *state) {
			scaled(s)
			s.sts.Generation = 4
		}, false},
		{"not ready", func(s *state) {
			scaled(s)
			s.sts.Status.ReadyReplicas = 3
		}, false},
		{"pod not ready", func(s *state) {
			scaled(s)
			s.redis[3].Status.Conditions[0].Status = corev1.ConditionFalse
		}, false},
		{"pod missing", func(s *state) {
			scaled(s)
			s.redis = s.redis[:3]
		}, false},
		{"pod terminating", func(s *state) {
			scaled(s)
			s.redis[0].DeletionTimestamp = &metav1.Time{}
		}, false},
		{"not on the update revision", func(s *state) {
			scaled(s)
			s.sts.Status.UpdatedReplicas = 3
		}, false},
	})
}

func TestSentinelReplicasConverged(t *testing.T) {
	scaled := func(s *state) {
		five := int32(5)
		d := s.sentinel
		d.Spec.Replicas = &five
		d.Generation, d.Status.ObservedGeneration = 2, 2
		d.Status.Replicas, d.Status.ReadyReplicas, d.Status.UpdatedReplicas, d.Status.AvailableReplicas = 5, 5, 5, 5
		s.sentinels = append(s.sentinels, readyPod("rfs-x-d", "sd", ""), readyPod("rfs-x-e", "se", ""))
	}
	runPredicate(t, sentinelReplicasConverged(5), []predicateCase{
		{"converged", scaled, true},
		{"not picked up by the operator", func(*state) {}, false},
		{"not available", func(s *state) {
			scaled(s)
			s.sentinel.Status.AvailableReplicas = 4
		}, false},
		{"old pod still terminating", func(s *state) {
			scaled(s)
			s.sentinel.Status.Replicas = 6
		}, false},
		{"pod not ready", func(s *state) {
			scaled(s)
			s.sentinels[4].Status.Conditions = nil
		}, false},
		{"no deployment", func(s *state) {
			s.sentinel = nil
		}, false},
	})
}

func TestResourcesConverged(t *testing.T) {
	want := corev1.ResourceRequirements{
		Requests: quantities("cpu", "75m", "memory", "64Mi"),
		Limits:   quantities("memory", "200Mi"),
	}
	resized := func(s *state) {
		for i := range s.redis {
			s.redis[i].Spec.Containers[0].Resources = *want.DeepCopy()
			s.redis[i].Status.ContainerStatuses[0].Resources = want.DeepCopy()
		}
	}
	runPredicate(t, resourcesConverged(want, 3), []predicateCase{
		{"resized", resized, true},
		{"not resized", func(*state) {}, false},
		{"one pod left", func(s *state) {
			resized(s)
			s.redis[2].Status.ContainerStatuses[0].Resources.Limits = quantities("memory", "256Mi")
		}, false},
		{"spec resized, kubelet not yet", func(s *state) {
			resized(s)
			s.redis[1].Status.ContainerStatuses[0].Resources = s.redis[0].Status.ContainerStatuses[0].Resources.DeepCopy()
			s.redis[1].Status.ContainerStatuses[0].Resources.Requests = quantities("cpu", "50m", "memory", "64Mi")
		}, false},
		{"resize annotation", func(s *state) {
			resized(s)
			s.redis[0].Annotations = map[string]string{resizeRequest: "2026-10-01T09:00:00Z"}
		}, false},
		{"cleared resize annotation", func(s *state) {
			resized(s)
			s.redis[0].Annotations = map[string]string{resizeRequest: ""}
		}, true},
		{"recreated, no reported resources", func(s *state) {
			resized(s)
			s.redis[0].Status.ContainerStatuses = nil
		}, true},
		{"no reported resources, resize in progress", func(s *state) {
			resized(s)
			s.redis[0].Status.ContainerStatuses = nil
			s.redis[0].Status.Conditions = append(s.redis[0].Status.Conditions,
				corev1.PodCondition{Type: corev1.PodResizeInProgress, Status: corev1.ConditionTrue})
		}, false},
		{"pod not on the update revision", func(s *state) {
			resized(s)
			s.sts.Status.UpdatedReplicas = 2
		}, false},
		{"pod not ready", func(s *state) {
			resized(s)
			s.redis[0].Status.Conditions[0].Status = corev1.ConditionFalse
		}, false},
	})
}

func TestRedisPodReplaced(t *testing.T) {
	runPredicate(t, redisPodReplaced("rfr-x-0", "u0"), []predicateCase{
		{"not deleted", func(*state) {}, false},
		{"terminating", func(s *state) {
			s.redis[0].DeletionTimestamp = &metav1.Time{}
		}, false},
		{"gone", func(s *state) {
			s.redis = s.redis[1:]
		}, false},
		{"recreated, not ready", func(s *state) {
			s.redis[0].UID = "u0-new"
			s.redis[0].Status.Conditions = nil
		}, false},
		{"recreated and ready", func(s *state) {
			s.redis[0].UID = "u0-new"
		}, true},
	})
}

func TestSentinelPodReplaced(t *testing.T) {
	replaced := func(s *state) {
		s.sentinels[0] = readyPod("rfs-x-z", "sz", "")
	}
	runPredicate(t, sentinelPodReplaced("sa", 3), []predicateCase{
		{"not deleted", func(*state) {}, false},
		{"replaced", replaced, true},
		{"replacement not ready", func(s *state) {
			replaced(s)
			s.sentinels[0].Status.Conditions = nil
		}, false},
		{"not available", func(s *state) {
			replaced(s)
			s.sentinel.Status.AvailableReplicas = 2
		}, false},
		{"not replaced yet", func(s *state) {
			s.sentinels = s.sentinels[1:]
			s.sentinel.Status.ReadyReplicas = 2
		}, false},
	})
}
