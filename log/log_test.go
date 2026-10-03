package log

import (
	"bytes"
	"encoding/json"
	"fmt"
	"runtime"
	"testing"

	"github.com/sirupsen/logrus"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// captureBase sends the output of the base logger to a buffer until the test
// ends.
func captureBase(t *testing.T) *bytes.Buffer {
	t.Helper()
	var buf bytes.Buffer
	l := baseLogger.entry.Logger
	out, formatter := l.Out, l.Formatter
	l.SetOutput(&buf)
	l.SetFormatter(&logrus.JSONFormatter{})
	t.Cleanup(func() {
		l.SetOutput(out)
		l.SetFormatter(formatter)
	})
	return &buf
}

func srcField(t *testing.T, buf *bytes.Buffer) string {
	t.Helper()
	var fields map[string]interface{}
	require.NoError(t, json.Unmarshal(buf.Bytes(), &fields))
	src, _ := fields["src"].(string)
	return src
}

// The src field must name the line that calls the logger, so that a log line
// leads to the code that wrote it.
func TestSourceIsTheCallSite(t *testing.T) {
	tests := []struct {
		name string
		// call logs one message on the line after the line it returns.
		call func() int
	}{
		{"method", func() int {
			_, _, line, _ := runtime.Caller(0)
			Base().Infof("message")
			return line
		}},
		{"method of a child logger", func() int {
			_, _, line, _ := runtime.Caller(0)
			Base().WithField("key", "value").Info("message")
			return line
		}},
		{"package function", func() int {
			_, _, line, _ := runtime.Caller(0)
			Infof("message")
			return line
		}},
		{"package Panic", func() (line int) {
			defer func() { _ = recover() }()
			_, _, line, _ = runtime.Caller(0)
			Panic("message")
			return line
		}},
		{"package Panicln", func() (line int) {
			defer func() { _ = recover() }()
			_, _, line, _ = runtime.Caller(0)
			Panicln("message")
			return line
		}},
		{"package Panicf", func() (line int) {
			defer func() { _ = recover() }()
			_, _, line, _ = runtime.Caller(0)
			Panicf("message")
			return line
		}},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			buf := captureBase(t)
			var line int
			assert.NotPanics(t, func() { line = test.call() })
			assert.Equal(t, fmt.Sprintf("log_test.go:%d", line+1), srcField(t, buf))
		})
	}
}
