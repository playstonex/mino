package base

import (
	"fmt"
	"log"
	"os"
	"path/filepath"
	"strings"
	"sync"
)

const (
	_Debug = iota
	_Info
	_Warn
	_Error
	_Fatal
)

var (
	defaultLogger   *Logger
	defaultLoggerMu sync.RWMutex
	logName         = "vpnagent.log"
)

type logWriter struct {
	UseStdout bool
	FileName  string
	File      *os.File
}

type Logger struct {
	normalWriter *logWriter
	debugWriter  *logWriter
	normalLogger *log.Logger
	debugLogger  *log.Logger
	baseLevel    int
	levels       map[int]string
}

func (lw *logWriter) Write(p []byte) (n int, err error) {
	return lw.File.Write(p)
}

func (lw *logWriter) newFile() {
	if lw.UseStdout || strings.TrimSpace(lw.FileName) == "" {
		lw.File = os.Stdout
		return
	}
	if err := os.MkdirAll(filepath.Dir(lw.FileName), 0o755); err != nil {
		lw.File = os.Stdout
		_, _ = os.Stderr.WriteString(err.Error() + "\n")
		return
	}
	_ = os.Remove(lw.FileName)
	f, err := os.OpenFile(lw.FileName, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o644)
	if err != nil {
		lw.File = os.Stdout
		_, _ = os.Stderr.WriteString(err.Error() + "\n")
		return
	}
	lw.File = f
}

func NewLogger(cfg *ClientConfig) *Logger {
	if cfg == nil {
		cfg = NewClientConfig()
	}

	normalWriter := &logWriter{
		UseStdout: strings.TrimSpace(cfg.LogPath) == "",
		FileName:  filepath.Join(cfg.LogPath, logName),
	}
	normalWriter.newFile()

	debugWriter := normalWriter
	if strings.TrimSpace(cfg.DebugLogPath) != "" {
		debugWriter = &logWriter{UseStdout: false, FileName: cfg.DebugLogPath}
		debugWriter.newFile()
	}

	logger := &Logger{
		normalWriter: normalWriter,
		debugWriter:  debugWriter,
		baseLevel:    logLevel2Int(cfg.LogLevel),
		levels: map[int]string{
			_Debug: "Debug",
			_Info:  "Info",
			_Warn:  "Warn",
			_Error: "Error",
			_Fatal: "Fatal",
		},
	}
	logger.normalLogger = log.New(normalWriter, "", log.LstdFlags)
	logger.debugLogger = log.New(debugWriter, "", log.LstdFlags)
	return logger
}

func InitLog(cfgs ...*ClientConfig) *Logger {
	cfg := Cfg
	if len(cfgs) > 0 && cfgs[0] != nil {
		cfg = cfgs[0]
	}
	logger := NewLogger(cfg)
	SetDefaultLogger(logger)
	return logger
}

func SetDefaultLogger(logger *Logger) {
	defaultLoggerMu.Lock()
	defer defaultLoggerMu.Unlock()
	defaultLogger = logger
}

func getDefaultLogger() *Logger {
	defaultLoggerMu.RLock()
	if defaultLogger != nil {
		logger := defaultLogger
		defaultLoggerMu.RUnlock()
		return logger
	}
	defaultLoggerMu.RUnlock()

	defaultLoggerMu.Lock()
	defer defaultLoggerMu.Unlock()
	if defaultLogger == nil {
		defaultLogger = NewLogger(NewClientConfig())
	}
	return defaultLogger
}

func (l *Logger) StdLogger() *log.Logger {
	if l == nil {
		return getDefaultLogger().StdLogger()
	}
	return l.normalLogger
}

func GetBaseLogger() *log.Logger {
	return getDefaultLogger().StdLogger()
}

func logLevel2Int(level string) int {
	lvl := _Info
	levels := map[int]string{
		_Debug: "Debug",
		_Info:  "Info",
		_Warn:  "Warn",
		_Error: "Error",
		_Fatal: "Fatal",
	}
	for k, v := range levels {
		if strings.EqualFold(strings.ToLower(level), strings.ToLower(v)) {
			lvl = k
		}
	}
	return lvl
}

func (l *Logger) output(level int, s ...interface{}) {
	if l == nil {
		getDefaultLogger().output(level, s...)
		return
	}
	prefix := fmt.Sprintf("[%s] ", l.levels[level])
	line := prefix + fmt.Sprintln(s...)
	target := l.normalLogger
	if level == _Debug {
		target = l.debugLogger
	}
	_ = target.Output(3, line)
}

func (l *Logger) Debug(v ...interface{}) {
	level := _Debug
	if l == nil {
		getDefaultLogger().Debug(v...)
		return
	}
	if l.baseLevel > level {
		return
	}
	l.output(level, v...)
}

func (l *Logger) Info(v ...interface{}) {
	level := _Info
	if l == nil {
		getDefaultLogger().Info(v...)
		return
	}
	if l.baseLevel > level {
		return
	}
	l.output(level, v...)
}

func (l *Logger) Warn(v ...interface{}) {
	level := _Warn
	if l == nil {
		getDefaultLogger().Warn(v...)
		return
	}
	if l.baseLevel > level {
		return
	}
	l.output(level, v...)
}

func (l *Logger) Error(v ...interface{}) {
	level := _Error
	if l == nil {
		getDefaultLogger().Error(v...)
		return
	}
	if l.baseLevel > level {
		return
	}
	l.output(level, v...)
}

func (l *Logger) Fatal(v ...interface{}) {
	level := _Fatal
	if l == nil {
		getDefaultLogger().Fatal(v...)
		return
	}
	if l.baseLevel > level {
		return
	}
	l.output(level, v...)
	os.Exit(1)
}

func Debug(v ...interface{}) {
	getDefaultLogger().Debug(v...)
}

func Info(v ...interface{}) {
	getDefaultLogger().Info(v...)
}

func Warn(v ...interface{}) {
	getDefaultLogger().Warn(v...)
}

func Error(v ...interface{}) {
	getDefaultLogger().Error(v...)
}

func Fatal(v ...interface{}) {
	getDefaultLogger().Fatal(v...)
}
