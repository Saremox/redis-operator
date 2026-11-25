package log

import (
	"fmt"
	"runtime"
	"strings"

	"github.com/sirupsen/logrus"
)

// Level refers to the level of logging.
type Level string

// Logger is an interface that needs to be implemented in order to log.
type Logger interface {
	Debug(...any)
	Debugf(string, ...any)

	Info(...any)
	Infof(string, ...any)

	Warnf(string, ...any)
	Warningf(string, ...any)

	Error(...any)
	Errorf(string, ...any)

	Fatal(...any)
	Fatalf(string, ...any)

	With(key string, value any) Logger
	WithField(key string, value any) Logger
	WithFields(values map[string]any) Logger
	Set(level Level) error
}

type logger struct {
	entry *logrus.Entry
}

func (l *logger) Debug(args ...any) {
	l.sourced().Debug(args...)
}

func (l *logger) Debugf(format string, args ...any) {
	l.sourced().Debugf(format, args...)
}

func (l *logger) Info(args ...any) {
	l.sourced().Info(args...)
}

func (l *logger) Infof(format string, args ...any) {
	l.sourced().Infof(format, args...)
}

func (l *logger) Warnf(format string, args ...any) {
	l.sourced().Warnf(format, args...)
}

func (l *logger) Warningf(format string, args ...any) {
	l.sourced().Warnf(format, args...)
}

func (l *logger) Error(args ...any) {
	l.sourced().Error(args...)
}

func (l *logger) Errorf(format string, args ...any) {
	l.sourced().Errorf(format, args...)
}

func (l *logger) Fatal(args ...any) {
	l.sourced().Fatal(args...)
}

func (l *logger) Fatalf(format string, args ...any) {
	l.sourced().Fatalf(format, args...)
}
func (l *logger) With(key string, value any) Logger {
	return &logger{l.entry.WithField(key, value)}
}

func (l *logger) WithField(key string, value any) Logger {
	return &logger{l.entry.WithField(key, value)}
}

func (l *logger) WithFields(values map[string]any) Logger {
	return &logger{l.entry.WithFields(logrus.Fields(values))}
}

func (l *logger) Set(level Level) error {
	leLev, err := logrus.ParseLevel(string(level))
	if err != nil {
		return fmt.Errorf("set the log level: %w", err)
	}
	l.entry.Logger.Level = leLev
	return nil
}

// sourced adds the call site as the src field. Caller(2) skips sourced and
// the logger function.
func (l *logger) sourced() *logrus.Entry {
	_, file, line, ok := runtime.Caller(2)
	if !ok {
		file = "<???>"
		line = 1
	} else {
		slash := strings.LastIndex(file, "/")
		file = file[slash+1:]
	}
	return l.entry.WithField("src", fmt.Sprintf("%s:%d", file, line))
}

var baseLogger = &logger{
	entry: &logrus.Entry{
		Logger: logrus.New(),
	},
}

// Base returns the logger of the package functions.
func Base() Logger {
	return baseLogger
}

// Debug and the other package log functions use the base logger and add the
// call site as the src field.
func Debug(args ...any) {
	baseLogger.sourced().Debug(args...)
}

// Debugf is Debug with a fmt.Sprintf format.
func Debugf(format string, args ...any) {
	baseLogger.sourced().Debugf(format, args...)
}

// Info logs the arguments at the info level.
func Info(args ...any) {
	baseLogger.sourced().Info(args...)
}

// Infof is Info with a fmt.Sprintf format.
func Infof(format string, args ...any) {
	baseLogger.sourced().Infof(format, args...)
}

// Warnf is Warn with a fmt.Sprintf format.
func Warnf(format string, args ...any) {
	baseLogger.sourced().Warnf(format, args...)
}

// Error logs the arguments at the error level.
func Error(args ...any) {
	baseLogger.sourced().Error(args...)
}

// Errorf is Error with a fmt.Sprintf format.
func Errorf(format string, args ...any) {
	baseLogger.sourced().Errorf(format, args...)
}

// Fatal logs the arguments at the fatal level, then stops the process with
// exit code 1.
func Fatal(args ...any) {
	baseLogger.sourced().Fatal(args...)
}

// Fatalf is Fatal with a fmt.Sprintf format.
func Fatalf(format string, args ...any) {
	baseLogger.sourced().Fatalf(format, args...)
}

// With returns a child of the base logger that adds the key and the value as a
// field to each message.
func With(key string, value any) Logger {
	return baseLogger.With(key, value)
}

// WithField is the same as With.
func WithField(key string, value any) Logger {
	return baseLogger.WithField(key, value)
}

// Set sets the level of the base logger.
func Set(level Level) error {
	return baseLogger.Set(level)
}
