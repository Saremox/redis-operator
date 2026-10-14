package log

// Dummy is a dummy logger.
var Dummy = DummyLogger{}

// DummyLogger is an empty logger mainly used for tests.
type DummyLogger struct{}

// Debug discards the message.
func (l DummyLogger) Debug(...interface{}) {}

// Debugln discards the message.
func (l DummyLogger) Debugln(...interface{}) {}

// Debugf discards the message.
func (l DummyLogger) Debugf(string, ...interface{}) {}

// Info discards the message.
func (l DummyLogger) Info(...interface{}) {}

// Infoln discards the message.
func (l DummyLogger) Infoln(...interface{}) {}

// Infof discards the message.
func (l DummyLogger) Infof(string, ...interface{}) {}

// Warn discards the message.
func (l DummyLogger) Warn(...interface{}) {}

// Warnln discards the message.
func (l DummyLogger) Warnln(...interface{}) {}

// Warnf discards the message.
func (l DummyLogger) Warnf(string, ...interface{}) {}

// Warningf discards the message.
func (l DummyLogger) Warningf(format string, args ...interface{}) {}

// Error discards the message.
func (l DummyLogger) Error(...interface{}) {}

// Errorln discards the message.
func (l DummyLogger) Errorln(...interface{}) {}

// Errorf discards the message.
func (l DummyLogger) Errorf(string, ...interface{}) {}

// Fatal discards the message and does not stop the process.
func (l DummyLogger) Fatal(...interface{}) {}

// Fatalln discards the message and does not stop the process.
func (l DummyLogger) Fatalln(...interface{}) {}

// Fatalf discards the message and does not stop the process.
func (l DummyLogger) Fatalf(string, ...interface{}) {}

// Panic discards the message and does not panic.
func (l DummyLogger) Panic(...interface{}) {}

// Panicln discards the message and does not panic.
func (l DummyLogger) Panicln(...interface{}) {}

// Panicf discards the message and does not panic.
func (l DummyLogger) Panicf(string, ...interface{}) {}

// With returns the same DummyLogger, because it keeps no fields.
func (l DummyLogger) With(key string, value interface{}) Logger { return l }

// WithField returns the same DummyLogger, because it keeps no fields.
func (l DummyLogger) WithField(key string, value interface{}) Logger { return l }

// WithFields returns the same DummyLogger, because it keeps no fields.
func (l DummyLogger) WithFields(values map[string]interface{}) Logger { return l }

// Set ignores the level and always returns nil.
func (l DummyLogger) Set(level Level) error { return nil }
