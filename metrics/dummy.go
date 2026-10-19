package metrics

// Dummy is a Recorder that discards all the metrics. The tests use it.
var Dummy = &dummy{
	ControllerRecorder: dummyControllerRecorder{},
}

// dummy implements Recorder and does nothing.
type dummy struct {
	ControllerRecorder
}

func (d *dummy) SetClusterOK(_, _ string)    {}
func (d *dummy) SetClusterError(_, _ string) {}
func (d *dummy) DeleteCluster(_, _ string)   {}
func (d *dummy) SetAutoscaler(_, _ string, _ int64, _ float64, _ int32, _ int64) {
}
func (d *dummy) SetAutoscalerPhase(_, _ string, _ int) {}
func (d *dummy) CountAutoscalerStep(_, _, _ string)    {}
func (d *dummy) DeleteAutoscaler(_, _ string)          {}
func (d *dummy) SetSentinelUnknownAddrs(_, _ string, _ int, _ bool) {
}
func (d *dummy) RecordEnsureOperation(_, _, _, _, _ string) {
}
func (d *dummy) RecordRedisCheck(_, _, _, _, _ string) {
}
func (d *dummy) RecordSentinelCheck(_, _, _, _, _ string) {
}
func (d *dummy) RecordK8sOperation(_, _, _, _, _, _ string) {
}
func (d *dummy) RecordRedisOperation(_, _, _, _, _ string) {
}
func (d *dummy) SetPodIP(_, _, _, _ string) {
}
