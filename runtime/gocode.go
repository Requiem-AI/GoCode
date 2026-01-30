package main

import (
	"flag"
	"github.com/joho/godotenv"
	"github.com/requiem-ai/gocode/context"
	"github.com/requiem-ai/gocode/services"
	"github.com/rs/zerolog"
	"github.com/rs/zerolog/log"
	"os"
	"strings"
	"time"
)

func main() {
	err := godotenv.Load()
	if err != nil {
		log.Fatal().Err(err).Msg("Error loading .env file")
	}

	log.Logger = log.Output(zerolog.ConsoleWriter{
		Out:        os.Stdout,
		TimeFormat: time.RFC3339,
	})
	zerolog.TimeFieldFormat = time.RFC3339
	logLevel := strings.ToLower(os.Getenv("LOG_LEVEL"))
	switch logLevel {
	case "trace":
		log.Info().Str("level", logLevel).Msg("Setting Log Level")
		zerolog.SetGlobalLevel(zerolog.TraceLevel)
		break
	case "debug":
		log.Info().Str("level", logLevel).Msg("Setting Log Level")
		zerolog.SetGlobalLevel(zerolog.DebugLevel)
		break
	case "info":
		fallthrough
	default:
		log.Info().Str("level", logLevel).Msg("Setting Log Level")
		zerolog.SetGlobalLevel(zerolog.InfoLevel)
		break
	}

	log.Info().Msg("Starting GoBot")

	skipLoadPool := flag.Bool("pool-skip", false, "Skip preloading the pools on start")
	flag.Parse()
	if *skipLoadPool {
		log.Warn().Msg("Skipping pool preload")
		_ = os.Setenv("SKIP_PRELOAD_POOLS", "true")
	}

	ctx, err := context.NewCtx(
		//Core
		&services.SetupService{},
		&services.GitService{},
		&services.AgentService{},
		&services.TelegramService{},
	)

	if err != nil {
		log.Fatal().Err(err)
		return
	}

	err = ctx.Run()
	if err != nil {
		log.Fatal().Err(err)
		return
	}
}
