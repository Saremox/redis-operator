package main

import (
	"testing"

	"github.com/stretchr/testify/assert"

	"github.com/saremox/redis-operator/cmd/utils"
	"github.com/saremox/redis-operator/log"
	mLog "github.com/saremox/redis-operator/mocks/log"
)

func TestRunLogsTheVersion(t *testing.T) {
	prev := Version
	t.Cleanup(func() { Version = prev })
	Version = "v1.2.3"

	logger := &mLog.Logger{}
	logger.On("Infof", "Starting redis-operator %s", "v1.2.3").Once().Return()
	logger.On("Set", log.Level("info")).Once().Return(assert.AnError)
	m := Main{flags: &utils.CMDFlags{LogLevel: "info"}, logger: logger}

	err := m.Run()

	assert.ErrorIs(t, err, assert.AnError)
	logger.AssertExpectations(t)
}
