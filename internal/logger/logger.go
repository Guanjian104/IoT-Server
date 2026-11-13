// Package logger 提供业务代码使用的日志接口。
// 实现使用标准库 log/slog，不引入第三方日志库。
package logger

import (
	"io"
	"log/slog"
	"strings"
)

// Logger 是骨架和后续业务模块共用的日志接口。
type Logger interface {
	Debug(msg string, args ...any)
	Info(msg string, args ...any)
	Warn(msg string, args ...any)
	Error(msg string, args ...any)
	With(args ...any) Logger
}

type slogLogger struct {
	inner *slog.Logger
}

// New 按级别创建日志器。level 只能是 debug、info、warn、error。
func New(w io.Writer, level string) (Logger, error) {
	if w == nil {
		w = io.Discard
	}
	var slogLevel slog.Level
	switch strings.ToLower(strings.TrimSpace(level)) {
	case "debug":
		slogLevel = slog.LevelDebug
	case "info":
		slogLevel = slog.LevelInfo
	case "warn":
		slogLevel = slog.LevelWarn
	case "error":
		slogLevel = slog.LevelError
	default:
		return nil, errInvalidLevel
	}
	handler := slog.NewTextHandler(w, &slog.HandlerOptions{Level: slogLevel})
	return slogLogger{inner: slog.New(handler)}, nil
}

var errInvalidLevel = errLevel{}

type errLevel struct{}

func (errLevel) Error() string { return "invalid log level" }

func (l slogLogger) Debug(msg string, args ...any) {
	l.inner.Debug(msg, redact(args)...)
}

func (l slogLogger) Info(msg string, args ...any) {
	l.inner.Info(msg, redact(args)...)
}

func (l slogLogger) Warn(msg string, args ...any) {
	l.inner.Warn(msg, redact(args)...)
}

func (l slogLogger) Error(msg string, args ...any) {
	l.inner.Error(msg, redact(args)...)
}

func (l slogLogger) With(args ...any) Logger {
	return slogLogger{inner: l.inner.With(redact(args)...)}
}

// Scrub 在写日志前去掉已知的长密钥。短字符串不参与替换，避免误伤普通字段。
func Scrub(next Logger, secrets []string) Logger {
	kept := make([]string, 0, len(secrets))
	for _, secret := range secrets {
		if len(secret) >= 12 {
			kept = append(kept, secret)
		}
	}
	if len(kept) == 0 || next == nil {
		return next
	}
	return scrubLogger{next: next, secrets: kept}
}

type scrubLogger struct {
	next    Logger
	secrets []string
}

func (l scrubLogger) Debug(msg string, args ...any) { l.next.Debug(l.scrub(msg), l.scrubArgs(args)...) }
func (l scrubLogger) Info(msg string, args ...any)  { l.next.Info(l.scrub(msg), l.scrubArgs(args)...) }
func (l scrubLogger) Warn(msg string, args ...any)  { l.next.Warn(l.scrub(msg), l.scrubArgs(args)...) }
func (l scrubLogger) Error(msg string, args ...any) {
	l.next.Error(l.scrub(msg), l.scrubArgs(args)...)
}
func (l scrubLogger) With(args ...any) Logger {
	return scrubLogger{next: l.next.With(l.scrubArgs(args)...), secrets: l.secrets}
}

func (l scrubLogger) scrub(value string) string {
	for _, secret := range l.secrets {
		value = strings.ReplaceAll(value, secret, "[redacted]")
	}
	return value
}

func (l scrubLogger) scrubArgs(args []any) []any {
	if len(args) == 0 {
		return nil
	}
	out := make([]any, len(args))
	for i, arg := range args {
		if text, ok := arg.(string); ok {
			out[i] = l.scrub(text)
			continue
		}
		out[i] = arg
	}
	return out
}

func redact(args []any) []any {
	if len(args) == 0 {
		return nil
	}
	out := make([]any, len(args))
	copy(out, args)
	for i := 0; i+1 < len(out); i += 2 {
		key, ok := out[i].(string)
		if ok && sensitiveKey(key) {
			out[i+1] = "[redacted]"
		}
	}
	return out
}

func sensitiveKey(key string) bool {
	k := strings.ToLower(key)
	return strings.Contains(k, "password") ||
		strings.Contains(k, "secret") ||
		strings.Contains(k, "token") ||
		strings.Contains(k, "credential")
}
