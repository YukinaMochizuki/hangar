package front

import (
	"context"
	"log/slog"

	"go.uber.org/zap/zapcore"
)

// slogCore is a zap core that writes to a slog logger, so certmagic, which
// logs with zap, logs as the rest of the edge does. Debug is dropped unless
// the logger takes it.
type slogCore struct {
	log    *slog.Logger
	fields []zapcore.Field
}

func (c slogCore) Enabled(l zapcore.Level) bool {
	return c.log.Enabled(context.Background(), slogLevel(l))
}

func (c slogCore) With(fields []zapcore.Field) zapcore.Core {
	return slogCore{log: c.log, fields: append(append([]zapcore.Field(nil), c.fields...), fields...)}
}

func (c slogCore) Check(e zapcore.Entry, ce *zapcore.CheckedEntry) *zapcore.CheckedEntry {
	if c.Enabled(e.Level) {
		return ce.AddCore(e, c)
	}
	return ce
}

func (c slogCore) Write(e zapcore.Entry, fields []zapcore.Field) error {
	enc := zapcore.NewMapObjectEncoder()
	for _, f := range c.fields {
		f.AddTo(enc)
	}
	for _, f := range fields {
		f.AddTo(enc)
	}
	attrs := make([]slog.Attr, 0, len(enc.Fields)+1)
	if e.LoggerName != "" {
		attrs = append(attrs, slog.String("logger", e.LoggerName))
	}
	for k, v := range enc.Fields {
		attrs = append(attrs, slog.Any(k, v))
	}
	c.log.LogAttrs(context.Background(), slogLevel(e.Level), e.Message, attrs...)
	return nil
}

func (c slogCore) Sync() error { return nil }

func slogLevel(l zapcore.Level) slog.Level {
	switch {
	case l < zapcore.InfoLevel:
		return slog.LevelDebug
	case l == zapcore.InfoLevel:
		return slog.LevelInfo
	case l == zapcore.WarnLevel:
		return slog.LevelWarn
	}
	return slog.LevelError
}
