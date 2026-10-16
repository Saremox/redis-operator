package log

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"

	"github.com/sirupsen/logrus"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// logLine is one JSON log line of the test output.
type logLine struct {
	Level string `json:"level"`
	Msg   string `json:"msg"`
	Src   string `json:"src"`
	Key   string `json:"key"`
	Other string `json:"other"`
}

func parseLines(t *testing.T, buf *bytes.Buffer) []logLine {
	t.Helper()
	var lines []logLine
	for raw := range strings.SplitSeq(strings.TrimSpace(buf.String()), "\n") {
		if raw == "" {
			continue
		}
		var l logLine
		require.NoError(t, json.Unmarshal([]byte(raw), &l))
		lines = append(lines, l)
	}
	return lines
}

// testLogger returns a logger at the debug level that writes JSON to a
// buffer. A fatal call writes its exit code to the returned slice and does
// not stop the test binary.
func testLogger() (*logger, *bytes.Buffer, *[]int) {
	var buf bytes.Buffer
	var exits []int
	l := logrus.New()
	l.SetOutput(&buf)
	l.SetFormatter(&logrus.JSONFormatter{})
	l.SetLevel(logrus.DebugLevel)
	l.ExitFunc = func(code int) { exits = append(exits, code) }
	return &logger{entry: logrus.NewEntry(l)}, &buf, &exits
}

// captureBaseAtDebug sends the output of the base logger to a buffer at the
// debug level and records the exit code of a fatal call until the test ends.
func captureBaseAtDebug(t *testing.T) (*bytes.Buffer, *[]int) {
	t.Helper()
	buf := captureBase(t)
	l := baseLogger.entry.Logger
	level, exitFunc := l.GetLevel(), l.ExitFunc
	var exits []int
	l.SetLevel(logrus.DebugLevel)
	l.ExitFunc = func(code int) { exits = append(exits, code) }
	t.Cleanup(func() {
		l.SetLevel(level)
		l.ExitFunc = exitFunc
	})
	return buf, &exits
}

type logCall struct {
	name  string
	call  func()
	level string
	msg   string
	exit  bool
	panic bool
}

// assertLogCalls checks that each call writes one line with the expected
// level, message and src field, and that only the fatal calls exit.
func assertLogCalls(t *testing.T, calls []logCall, buf *bytes.Buffer, exits *[]int) {
	t.Helper()
	for _, c := range calls {
		t.Run(c.name, func(t *testing.T) {
			buf.Reset()
			*exits = nil
			if c.panic {
				assert.Panics(t, c.call)
			} else {
				assert.NotPanics(t, c.call)
			}
			lines := parseLines(t, buf)
			require.Len(t, lines, 1)
			assert.Equal(t, c.level, lines[0].Level)
			assert.Equal(t, c.msg, lines[0].Msg)
			assert.True(t, strings.HasPrefix(lines[0].Src, "logger_test.go:"), "src %q", lines[0].Src)
			if c.exit {
				assert.Equal(t, []int{1}, *exits)
			} else {
				assert.Empty(t, *exits)
			}
		})
	}
}

func TestLoggerMethodsWriteLevelAndMessage(t *testing.T) {
	l, buf, exits := testLogger()
	assertLogCalls(t, []logCall{
		{name: "Debug", call: func() { l.Debug("a", "b") }, level: "debug", msg: "ab"},
		{name: "Debugln", call: func() { l.Debugln("a", "b") }, level: "debug", msg: "a b"},
		{name: "Debugf", call: func() { l.Debugf("n=%d", 1) }, level: "debug", msg: "n=1"},
		{name: "Info", call: func() { l.Info("a", "b") }, level: "info", msg: "ab"},
		{name: "Infoln", call: func() { l.Infoln("a", "b") }, level: "info", msg: "a b"},
		{name: "Infof", call: func() { l.Infof("n=%d", 2) }, level: "info", msg: "n=2"},
		{name: "Warn", call: func() { l.Warn("a", "b") }, level: "warning", msg: "ab"},
		{name: "Warnln", call: func() { l.Warnln("a", "b") }, level: "warning", msg: "a b"},
		{name: "Warnf", call: func() { l.Warnf("n=%d", 3) }, level: "warning", msg: "n=3"},
		{name: "Warningf", call: func() { l.Warningf("n=%d", 4) }, level: "warning", msg: "n=4"},
		{name: "Error", call: func() { l.Error("a", "b") }, level: "error", msg: "ab"},
		{name: "Errorln", call: func() { l.Errorln("a", "b") }, level: "error", msg: "a b"},
		{name: "Errorf", call: func() { l.Errorf("n=%d", 5) }, level: "error", msg: "n=5"},
		{name: "Fatal", call: func() { l.Fatal("a", "b") }, level: "fatal", msg: "ab", exit: true},
		{name: "Fatalln", call: func() { l.Fatalln("a", "b") }, level: "fatal", msg: "a b", exit: true},
		{name: "Fatalf", call: func() { l.Fatalf("n=%d", 6) }, level: "fatal", msg: "n=6", exit: true},
		{name: "Panic", call: func() { l.Panic("a", "b") }, level: "panic", msg: "ab", panic: true},
		{name: "Panicln", call: func() { l.Panicln("a", "b") }, level: "panic", msg: "a b", panic: true},
		{name: "Panicf", call: func() { l.Panicf("n=%d", 7) }, level: "panic", msg: "n=7", panic: true},
	}, buf, exits)
}

func TestPackageFunctionsWriteToTheBaseLogger(t *testing.T) {
	buf, exits := captureBaseAtDebug(t)
	assertLogCalls(t, []logCall{
		{name: "Debug", call: func() { Debug("a", "b") }, level: "debug", msg: "ab"},
		{name: "Debugln", call: func() { Debugln("a", "b") }, level: "debug", msg: "a b"},
		{name: "Debugf", call: func() { Debugf("n=%d", 1) }, level: "debug", msg: "n=1"},
		{name: "Info", call: func() { Info("a", "b") }, level: "info", msg: "ab"},
		{name: "Infoln", call: func() { Infoln("a", "b") }, level: "info", msg: "a b"},
		{name: "Infof", call: func() { Infof("n=%d", 2) }, level: "info", msg: "n=2"},
		{name: "Warn", call: func() { Warn("a", "b") }, level: "warning", msg: "ab"},
		{name: "Warnln", call: func() { Warnln("a", "b") }, level: "warning", msg: "a b"},
		{name: "Warnf", call: func() { Warnf("n=%d", 3) }, level: "warning", msg: "n=3"},
		{name: "Error", call: func() { Error("a", "b") }, level: "error", msg: "ab"},
		{name: "Errorln", call: func() { Errorln("a", "b") }, level: "error", msg: "a b"},
		{name: "Errorf", call: func() { Errorf("n=%d", 5) }, level: "error", msg: "n=5"},
		{name: "Fatal", call: func() { Fatal("a", "b") }, level: "fatal", msg: "ab", exit: true},
		{name: "Fatalln", call: func() { Fatalln("a", "b") }, level: "fatal", msg: "a b", exit: true},
		{name: "Fatalf", call: func() { Fatalf("n=%d", 6) }, level: "fatal", msg: "n=6", exit: true},
		{name: "Panic", call: func() { Panic("a", "b") }, level: "panic", msg: "ab", panic: true},
		{name: "Panicln", call: func() { Panicln("a", "b") }, level: "panic", msg: "a b", panic: true},
		{name: "Panicf", call: func() { Panicf("n=%d", 7) }, level: "panic", msg: "n=7", panic: true},
	}, buf, exits)
}

func TestLoggerFieldsGoToTheChildOnly(t *testing.T) {
	l, buf, _ := testLogger()

	l.With("key", "with").Info("m")
	l.WithField("key", "withfield").Info("m")
	l.WithFields(map[string]any{"key": "withfields", "other": "o"}).Info("m")
	l.Info("m")

	lines := parseLines(t, buf)
	require.Len(t, lines, 4)
	assert.Equal(t, "with", lines[0].Key)
	assert.Equal(t, "withfield", lines[1].Key)
	assert.Equal(t, "withfields", lines[2].Key)
	assert.Equal(t, "o", lines[2].Other)
	assert.Empty(t, lines[3].Key, "the parent logger keeps no field of a child")
}

func TestPackageWithAndWithFieldUseTheBaseLogger(t *testing.T) {
	buf, _ := captureBaseAtDebug(t)

	With("key", "with").Info("m")
	WithField("key", "withfield").Info("m")

	lines := parseLines(t, buf)
	require.Len(t, lines, 2)
	assert.Equal(t, "with", lines[0].Key)
	assert.Equal(t, "withfield", lines[1].Key)
}

func TestLoggerSet(t *testing.T) {
	l, buf, _ := testLogger()

	require.NoError(t, l.Set("warning"))
	assert.Equal(t, logrus.WarnLevel, l.entry.Logger.GetLevel())
	l.Info("dropped")
	l.Warn("kept")
	lines := parseLines(t, buf)
	require.Len(t, lines, 1)
	assert.Equal(t, "kept", lines[0].Msg)

	require.EqualError(t, l.Set("verbose"), `set the log level: not a valid logrus Level: "verbose"`)
	assert.Equal(t, logrus.WarnLevel, l.entry.Logger.GetLevel(), "an unknown level keeps the level")
}

func TestPackageSetChangesTheBaseLevel(t *testing.T) {
	captureBaseAtDebug(t)

	require.NoError(t, Set("error"))
	assert.Equal(t, logrus.ErrorLevel, baseLogger.entry.Logger.GetLevel())
	require.Error(t, Set("verbose"))
	assert.Equal(t, logrus.ErrorLevel, baseLogger.entry.Logger.GetLevel())
}

// The tests of other packages use Dummy, so it must not write, exit or panic.
func TestDummyLoggerDiscardsAll(t *testing.T) {
	var l Logger = Dummy
	assert.NotPanics(t, func() {
		l.Debug("m")
		l.Debugln("m")
		l.Debugf("m")
		l.Info("m")
		l.Infoln("m")
		l.Infof("m")
		l.Warn("m")
		l.Warnln("m")
		l.Warnf("m")
		l.Warningf("m")
		l.Error("m")
		l.Errorln("m")
		l.Errorf("m")
		l.Fatal("m")
		l.Fatalln("m")
		l.Fatalf("m")
		l.Panic("m")
		l.Panicln("m")
		l.Panicf("m")
	})
	assert.Equal(t, Dummy, l.With("key", "value"))
	assert.Equal(t, Dummy, l.WithField("key", "value"))
	assert.Equal(t, Dummy, l.WithFields(map[string]any{"key": "value"}))
	assert.NoError(t, l.Set("verbose"))
}
