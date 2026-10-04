/*
Package redisfailover reconciles the RedisFailover resources. A RedisFailover
has a group of Redis pods with one master. By default, the operator manages
the failover. When sentinel.enabled is true, a group of Sentinels manages it.
*/

package redisfailover
