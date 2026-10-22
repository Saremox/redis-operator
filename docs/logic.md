# Controller logic

## Creation pipeline

The Redis-Operator creates Redis Failovers, with all the needed pieces. So, when a event arrives from Kubernetes (add or sync), the following steps are executed:

- Ensure: checks that all the pieces needed are created. It is important to notice that if a change is performed manually on the objects created, the operator will override them. This is done to ensure a healthy status. It will create the following:
  - Redis service (if exporter enabled)
  - Redis configmap
  - Redis shutdown configmap
  - Redis statefulset
  - Sentinel service
  - Sentinel configmap
  - Sentinel deployment
- Check & Heal: will connect to every Redis and Sentinel and will ensure that they are working as they are supposed to do. If this is not the case, it will reconfigure the nodes to move them to the desire state. It will check the following:
  - Number of Redis is equal as the set on the RF spec
  - Number of Sentinel is equal as the set on the RF spec
  - Only one Redis working as a master
  - All Redis slaves have the same master
  - All Redis slaves are connected to the master
  - All Sentinels points to the same Redis master
  - Sentinel has not death nodes
  - Sentinel knows the correct slave number
  - Ensure Redis has the custom configuration set
  - Ensure Sentinel has the custom configuration set

The controller tries to repair most problems. A [split-brain](<https://en.wikipedia.org/wiki/Split-brain_(computing)>) needs a manual fix, except in operator-managed mode when exactly one pod that runs has the master label. Then the controller makes the other masters replicas of that pod. **For a split-brain that it cannot repair, the controller logs an error and waits for a manual fix**.
