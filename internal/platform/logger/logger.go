// Package logger builds the structured zerolog logger.
package logger

import (
	"os"
	"time"

	"github.com/rs/zerolog"
)

func New(env, level string) zerolog.Logger {
	lvl, err := zerolog.ParseLevel(level)
	if err != nil {
		lvl = zerolog.InfoLevel
	}
	zerolog.TimeFieldFormat = time.RFC3339Nano
	if env == "dev" {
		return zerolog.New(zerolog.ConsoleWriter{Out: os.Stderr, TimeFormat: "15:04:05"}).Level(lvl).With().Timestamp().Logger()
	}
	return zerolog.New(os.Stdout).Level(lvl).With().Timestamp().Logger()
}
