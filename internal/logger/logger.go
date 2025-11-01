package logger

import (
	"log"
)

type LogLevel int

const (
	LogLevelNone LogLevel = iota
	LogLevelError
	LogLevelWarning
	LogLevelInfo
	LogLevelDebug
)

type Logger struct {
	logger *log.Logger
	level  LogLevel
	tag    string
}

func NewLogger(logger *log.Logger, level LogLevel) *Logger {
	return &Logger{
		logger: logger,
		level:  level,
		tag:    "",
	}
}

// WithTag creates a new logger with a tag prefix
func (l *Logger) WithTag(tag string) *Logger {
	return &Logger{
		logger: l.logger,
		level:  l.level,
		tag:    tag,
	}
}

func (l *Logger) formatMessage(level string, format string) string {
	if l.tag != "" {
		if level != "" {
			return "[" + l.tag + "] " + level + " " + format
		}
		return "[" + l.tag + "] " + format
	}
	if level != "" {
		return level + " " + format
	}
	return format
}

func (l *Logger) Debugf(format string, v ...interface{}) {
	if l.level >= LogLevelDebug {
		l.logger.Printf(l.formatMessage("DEBUG:", format), v...)
	}
}

func (l *Logger) Infof(format string, v ...interface{}) {
	if l.level >= LogLevelInfo {
		l.logger.Printf(l.formatMessage("", format), v...)
	}
}

// Printf is an alias for Infof for compatibility
func (l *Logger) Printf(format string, v ...interface{}) {
	l.Infof(format, v...)
}

func (l *Logger) Warnf(format string, v ...interface{}) {
	if l.level >= LogLevelWarning {
		l.logger.Printf(l.formatMessage("WARN:", format), v...)
	}
}

func (l *Logger) Errorf(format string, v ...interface{}) {
	if l.level >= LogLevelError {
		l.logger.Printf(l.formatMessage("ERROR:", format), v...)
	}
}

func (l *Logger) Fatalf(format string, v ...interface{}) {
	l.logger.Fatalf(l.formatMessage("FATAL:", format), v...)
}
