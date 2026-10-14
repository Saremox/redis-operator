package log

// Dummy is a dummy logger.
var Dummy = DummyLogger{}

// DummyLogger is an empty logger mainly used for tests.
type DummyLogger struct{}

// Debug discards the message.
func (l DummyLogger) Debug(...any) {}

// Debugln discards the message.
func (l DummyLogger) Debugln(...any) {}

// Debugf discards the message.
func (l DummyLogger) Debugf(string, ...any) {}

// Info discards the message.
func (l DummyLogger) Info(...any) {}

// Infoln discards the message.
func (l DummyLogger) Infoln(...any) {}

// Infof discards the message.
func (l DummyLogger) Infof(string, ...any) {}

// Warn discards the message.
func (l DummyLogger) Warn(...any) {}

// Warnln discards the message.
func (l DummyLogger) Warnln(...any) {}

// Warnf discards the message.
func (l DummyLogger) Warnf(string, ...any) {}

// Warningf discards the message.
func (l DummyLogger) Warningf(string, ...any) {}

// Error discards the message.
func (l DummyLogger) Error(...any) {}

// Errorln discards the message.
func (l DummyLogger) Errorln(...any) {}

// Errorf discards the message.
func (l DummyLogger) Errorf(string, ...any) {}

// Fatal discards the message and does not stop the process.
func (l DummyLogger) Fatal(...any) {}

// Fatalln discards the message and does not stop the process.
func (l DummyLogger) Fatalln(...any) {}

// Fatalf discards the message and does not stop the process.
func (l DummyLogger) Fatalf(string, ...any) {}

// Panic discards the message and does not panic.
func (l DummyLogger) Panic(...any) {}

// Panicln discards the message and does not panic.
func (l DummyLogger) Panicln(...any) {}

// Panicf discards the message and does not panic.
func (l DummyLogger) Panicf(string, ...any) {}

// With returns the same DummyLogger, because it keeps no fields.
func (l DummyLogger) With(string, any) Logger { return l }

// WithField returns the same DummyLogger, because it keeps no fields.
func (l DummyLogger) WithField(string, any) Logger { return l }

// WithFields returns the same DummyLogger, because it keeps no fields.
func (l DummyLogger) WithFields(map[string]any) Logger { return l }

// Set ignores the level and always returns nil.
func (l DummyLogger) Set(Level) error { return nil }
