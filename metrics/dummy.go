package metrics

// Dummy is a Recorder that discards all the metrics. The tests use it.
var Dummy = &dummy{
	ControllerRecorder: dummyControllerRecorder{},
}

// dummy implements Recorder and does nothing.
type dummy struct {
	ControllerRecorder
}

func (d *dummy) SetClusterOK(namespace, name string)    {}
func (d *dummy) SetClusterError(namespace, name string) {}
func (d *dummy) DeleteCluster(namespace, name string)   {}
func (d *dummy) SetAutoscaler(namespace, name string, recommendedBytes int64, usedRatio float64, highWindows int32, effectiveBytes int64) {
}
func (d *dummy) DeleteAutoscaler(namespace, name string) {}
func (d *dummy) RecordEnsureOperation(objectNamespace, objectName, objectKind, resourceName, status string) {
}
func (d *dummy) RecordRedisCheck(namespace, resource, indicator, instance, status string) {
}
func (d *dummy) RecordSentinelCheck(namespace, resource, indicator, instance, status string) {
}
func (d dummy) RecordK8sOperation(namespace, kind, object, operation, status, err string) {
}
func (d dummy) RecordRedisOperation(kind, IP, operation, status, err string) {
}
