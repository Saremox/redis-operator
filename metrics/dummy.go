package metrics

// Dummy is a Recorder that discards all the metrics. The tests use it.
var Dummy = &dummy{
	ControllerRecorder: dummyControllerRecorder{},
}

// dummy implements Recorder and does nothing.
type dummy struct {
	ControllerRecorder
}

func (d *dummy) SetClusterOK(namespace string, name string)    {}
func (d *dummy) SetClusterError(namespace string, name string) {}
func (d *dummy) DeleteCluster(namespace string, name string)   {}
func (d *dummy) RecordEnsureOperation(objectNamespace string, objectName string, objectKind string, resourceName string, status string) {
}
func (d *dummy) RecordRedisCheck(namespace string, resource string, indicator string, instance string, status string) {
}
func (d *dummy) RecordSentinelCheck(namespace string, resource string, indicator string, instance string, status string) {
}
func (d dummy) RecordK8sOperation(namespace string, kind string, object string, operation string, status string, err string) {
}
func (d dummy) RecordRedisOperation(kind string, IP string, operation string, status string, err string) {
}
