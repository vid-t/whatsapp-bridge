package wa

import (
	"fmt"
	"log/slog"

	waLog "go.mau.fi/whatsmeow/util/log"
)

// slogAdapter satisfies whatsmeow's waLog.Logger using log/slog.
type slogAdapter struct {
	l *slog.Logger
}

func newWALog(l *slog.Logger, module string) waLog.Logger {
	return &slogAdapter{l: l.With("module", module)}
}

func (s *slogAdapter) Warnf(msg string, args ...any)  { s.l.Warn(fmt.Sprintf(msg, args...)) }
func (s *slogAdapter) Errorf(msg string, args ...any) { s.l.Error(fmt.Sprintf(msg, args...)) }
func (s *slogAdapter) Infof(msg string, args ...any)  { s.l.Info(fmt.Sprintf(msg, args...)) }
func (s *slogAdapter) Debugf(msg string, args ...any) { s.l.Debug(fmt.Sprintf(msg, args...)) }
func (s *slogAdapter) Sub(module string) waLog.Logger {
	return &slogAdapter{l: s.l.With("sub", module)}
}
