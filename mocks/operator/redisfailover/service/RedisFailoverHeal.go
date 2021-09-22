// Code generated by mockery v2.9.4. DO NOT EDIT.

package mocks

import (
	mock "github.com/stretchr/testify/mock"

	v1 "github.com/spotahome/redis-operator/api/redisfailover/v1"
)

// RedisFailoverHeal is an autogenerated mock type for the RedisFailoverHeal type
type RedisFailoverHeal struct {
	mock.Mock
}

// DeletePod provides a mock function with given fields: podName, rFailover
func (_m *RedisFailoverHeal) DeletePod(podName string, rFailover *v1.RedisFailover) error {
	ret := _m.Called(podName, rFailover)

	var r0 error
	if rf, ok := ret.Get(0).(func(string, *v1.RedisFailover) error); ok {
		r0 = rf(podName, rFailover)
	} else {
		r0 = ret.Error(0)
	}

	return r0
}

// MakeMaster provides a mock function with given fields: ip, rFailover
func (_m *RedisFailoverHeal) MakeMaster(ip string, rFailover *v1.RedisFailover) error {
	ret := _m.Called(ip, rFailover)

	var r0 error
	if rf, ok := ret.Get(0).(func(string, *v1.RedisFailover) error); ok {
		r0 = rf(ip, rFailover)
	} else {
		r0 = ret.Error(0)
	}

	return r0
}

// NewSentinelMonitor provides a mock function with given fields: ip, monitor, rFailover
func (_m *RedisFailoverHeal) NewSentinelMonitor(ip string, monitor string, rFailover *v1.RedisFailover) error {
	ret := _m.Called(ip, monitor, rFailover)

	var r0 error
	if rf, ok := ret.Get(0).(func(string, string, *v1.RedisFailover) error); ok {
		r0 = rf(ip, monitor, rFailover)
	} else {
		r0 = ret.Error(0)
	}

	return r0
}

// NewSentinelMonitorWithPort provides a mock function with given fields: ip, monitor, port, rFailover
func (_m *RedisFailoverHeal) NewSentinelMonitorWithPort(ip string, monitor string, port string, rFailover *v1.RedisFailover) error {
	ret := _m.Called(ip, monitor, port, rFailover)

	var r0 error
	if rf, ok := ret.Get(0).(func(string, string, string, *v1.RedisFailover) error); ok {
		r0 = rf(ip, monitor, port, rFailover)
	} else {
		r0 = ret.Error(0)
	}

	return r0
}

// RestoreSentinel provides a mock function with given fields: ip
func (_m *RedisFailoverHeal) RestoreSentinel(ip string) error {
	ret := _m.Called(ip)

	var r0 error
	if rf, ok := ret.Get(0).(func(string) error); ok {
		r0 = rf(ip)
	} else {
		r0 = ret.Error(0)
	}

	return r0
}

// SetExternalMasterOnAll provides a mock function with given fields: masterIP, masterPort, rFailover
func (_m *RedisFailoverHeal) SetExternalMasterOnAll(masterIP string, masterPort string, rFailover *v1.RedisFailover) error {
	ret := _m.Called(masterIP, masterPort, rFailover)

	var r0 error
	if rf, ok := ret.Get(0).(func(string, string, *v1.RedisFailover) error); ok {
		r0 = rf(masterIP, masterPort, rFailover)
	} else {
		r0 = ret.Error(0)
	}

	return r0
}

// SetMasterOnAll provides a mock function with given fields: masterIP, rFailover
func (_m *RedisFailoverHeal) SetMasterOnAll(masterIP string, rFailover *v1.RedisFailover) error {
	ret := _m.Called(masterIP, rFailover)

	var r0 error
	if rf, ok := ret.Get(0).(func(string, *v1.RedisFailover) error); ok {
		r0 = rf(masterIP, rFailover)
	} else {
		r0 = ret.Error(0)
	}

	return r0
}

// SetOldestAsMaster provides a mock function with given fields: rFailover
func (_m *RedisFailoverHeal) SetOldestAsMaster(rFailover *v1.RedisFailover) error {
	ret := _m.Called(rFailover)

	var r0 error
	if rf, ok := ret.Get(0).(func(*v1.RedisFailover) error); ok {
		r0 = rf(rFailover)
	} else {
		r0 = ret.Error(0)
	}

	return r0
}

// SetRedisCustomConfig provides a mock function with given fields: ip, rFailover
func (_m *RedisFailoverHeal) SetRedisCustomConfig(ip string, rFailover *v1.RedisFailover) error {
	ret := _m.Called(ip, rFailover)

	var r0 error
	if rf, ok := ret.Get(0).(func(string, *v1.RedisFailover) error); ok {
		r0 = rf(ip, rFailover)
	} else {
		r0 = ret.Error(0)
	}

	return r0
}

// SetSentinelCustomConfig provides a mock function with given fields: ip, rFailover
func (_m *RedisFailoverHeal) SetSentinelCustomConfig(ip string, rFailover *v1.RedisFailover) error {
	ret := _m.Called(ip, rFailover)

	var r0 error
	if rf, ok := ret.Get(0).(func(string, *v1.RedisFailover) error); ok {
		r0 = rf(ip, rFailover)
	} else {
		r0 = ret.Error(0)
	}

	return r0
}
