package log

import (
	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"
)

var (
	log *zap.SugaredLogger
)

// Errorf uses fmt.Sprintf to log a formatted string.
func Errorf(format string, args ...interface{}) {
	log.Errorf(format, args...)
}

// Fatalf uses fmt.Sprintf to log a formatted string.
func Fatalf(format string, args ...interface{}) {
	log.Fatalf(format, args...)
}

// Infof uses fmt.Sprintf to log a formatted string.
func Infof(format string, args ...interface{}) {
	log.Infof(format, args...)
}

func init() {
	enc := zap.NewDevelopmentEncoderConfig()
	enc.EncodeLevel = zapcore.CapitalColorLevelEncoder
	enc.EncodeTime = zapcore.RFC3339TimeEncoder
	cfg := zap.Config{
		DisableCaller:     true,
		DisableStacktrace: true,
		EncoderConfig:     enc,
		Encoding:          "console",
		ErrorOutputPaths:  []string{"stderr"},
		Level:             zap.NewAtomicLevelAt(zap.InfoLevel),
		OutputPaths:       []string{"stderr"},
		Sampling: &zap.SamplingConfig{
			Initial:    100,
			Thereafter: 100,
		},
	}
	logger, _ := cfg.Build()
	log = logger.Sugar()
	zap.RedirectStdLog(logger)
}
