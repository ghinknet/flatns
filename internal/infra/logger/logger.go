// Package logger provides a process-wide zap logger configured from the config
// singleton. It writes a human-readable console stream to stdout and, when
// configured, JSON log files with lumberjack-backed rotation: a combined file
// at the active level and an error-only file at Warn and above.
package logger

import (
	"io"
	"os"

	"flatns/internal/infra/config"

	"github.com/goccy/go-json"
	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"
	"gopkg.in/natefinch/lumberjack.v2"
)

// L is the global logger. It is valid only after Init has returned.
var L *zap.Logger

// Init builds the global logger from the current configuration. It must be
// called after config.Init.
func Init() {
	L = build(config.Get().Log)
	L.Debug("logger initialized")
}

// Reload rebuilds the logger from the current configuration. It is registered
// as a config reload hook so log level and file settings take effect on SIGHUP
// or file edits. The previous logger is flushed before replacement.
func Reload() {
	old := L
	L = build(config.Get().Log)
	if old != nil {
		_ = old.Sync()
	}
	L.Debug("logger reloaded")
}

// build assembles a zap logger from a LogConfig.
func build(cfg config.LogConfig) *zap.Logger {
	// Select log level.
	level := zap.InfoLevel
	if cfg.Debug {
		level = zap.DebugLevel
	}

	encoderConfig := zapcore.EncoderConfig{
		TimeKey:        "time",
		LevelKey:       "level",
		NameKey:        "logger",
		CallerKey:      "caller",
		FunctionKey:    zapcore.OmitKey,
		MessageKey:     "msg",
		StacktraceKey:  "stacktrace",
		LineEnding:     zapcore.DefaultLineEnding,
		EncodeLevel:    zapcore.CapitalLevelEncoder,
		EncodeTime:     zapcore.ISO8601TimeEncoder,
		EncodeDuration: zapcore.SecondsDurationEncoder,
		EncodeCaller:   zapcore.ShortCallerEncoder,
		NewReflectedEncoder: func(w io.Writer) zapcore.ReflectedEncoder {
			enc := json.NewEncoder(w)
			enc.SetEscapeHTML(false)
			return enc
		},
	}

	cores := make([]zapcore.Core, 0, 3)

	// Always log to stdout using the console encoder for readability.
	cores = append(cores, zapcore.NewCore(
		zapcore.NewConsoleEncoder(encoderConfig),
		zapcore.Lock(os.Stdout),
		level,
	))

	// Combined log file at the active level.
	if cfg.File.All != "" {
		cores = append(cores, zapcore.NewCore(
			zapcore.NewJSONEncoder(encoderConfig),
			zapcore.AddSync(newRotator(cfg, cfg.File.All)),
			level,
		))
	}

	// Error-only log file at Warn and above.
	if cfg.File.Err != "" {
		cores = append(cores, zapcore.NewCore(
			zapcore.NewJSONEncoder(encoderConfig),
			zapcore.AddSync(newRotator(cfg, cfg.File.Err)),
			zapcore.WarnLevel,
		))
	}

	core := zapcore.NewTee(cores...)
	return zap.New(core, zap.AddCaller(), zap.AddStacktrace(zap.ErrorLevel))
}

// newRotator builds a lumberjack writer for a log file using the rotation
// settings shared across all sinks.
func newRotator(cfg config.LogConfig, filename string) *lumberjack.Logger {
	return &lumberjack.Logger{
		Filename:   filename,
		MaxSize:    cfg.MaxSize,
		MaxBackups: cfg.MaxBackups,
		MaxAge:     cfg.MaxAge,
		Compress:   cfg.Compress,
		LocalTime:  true,
	}
}

// Cleanup flushes any buffered log entries. It is safe to call when L is nil.
func Cleanup() {
	if L != nil {
		_ = L.Sync()
	}
}
