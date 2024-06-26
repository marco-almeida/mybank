package main

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/golang-migrate/migrate/v4"
	_ "github.com/golang-migrate/migrate/v4/database/postgres"
	_ "github.com/golang-migrate/migrate/v4/source/file"
	"github.com/hibiken/asynq"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/rs/zerolog"
	"github.com/rs/zerolog/log"
	"golang.org/x/sync/errgroup"

	"github.com/marco-almeida/mybank/internal/config"
	"github.com/marco-almeida/mybank/internal/handler"
	"github.com/marco-almeida/mybank/internal/middleware"
	"github.com/marco-almeida/mybank/internal/postgresql"
	redisRepo "github.com/marco-almeida/mybank/internal/redis"
	"github.com/marco-almeida/mybank/internal/service"
	redisSvc "github.com/marco-almeida/mybank/internal/service/redis"
	"github.com/marco-almeida/mybank/internal/token"
)

var interruptSignals = []os.Signal{
	os.Interrupt,
	syscall.SIGTERM,
	syscall.SIGINT,
}

func main() {
	// get env vars
	config, err := config.LoadConfig(".")
	if err != nil {
		log.Fatal().Err(err).Msg("cannot load config")
	}

	setupLogging(config)

	// setup graceful shutdown signals
	ctx, stop := signal.NotifyContext(context.Background(), interruptSignals...)
	defer stop()

	// init db
	connPool, err := postgresql.NewPostgreSQL(ctx, &config)
	if err != nil {
		log.Fatal().Err(err).Msg("cannot connect to db")
	}

	// run migrations
	dbSource := fmt.Sprintf("postgresql://%s:%s@%s:%d/%s?sslmode=disable", config.PostgresUser, config.PostgresPassword, config.PostgresHost, config.PostgresPort, config.PostgresDatabase)

	err = runDBMigration(config.MigrationURL, dbSource)
	if err != nil {
		log.Fatal().Err(err).Msg("cannot run db migration")
	}

	log.Info().Msg("db migrated successfully")

	// running in waitgroup coroutine in order to wait for graceful shutdown
	waitGroup, ctx := errgroup.WithContext(ctx)

	redisOpt := asynq.RedisClientOpt{
		Addr: config.RedisAddress,
	}

	runTaskProcessor(ctx, waitGroup, config, connPool, redisOpt)
	runHTPPServer(ctx, waitGroup, config, connPool, redisOpt)

	err = waitGroup.Wait()
	if err != nil {
		log.Fatal().Err(err).Msg("error from wait group")
	}
}

func setupLogging(config config.Config) {
	// log to file ./logs/mybank/main.log and terminal
	logFolder := filepath.Join("logs", "mybank")
	err := os.MkdirAll(logFolder, os.ModePerm)
	if err != nil {
		log.Fatal().Err(err).Msg("cannot create log folder")
	}

	logFile := filepath.Join(logFolder, "main.log")

	f, err := os.OpenFile(logFile, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0644)
	if err != nil {
		log.Fatal().Err(err).Msg("cannot open log file")
	}

	// set up json or human readable logging
	if config.Environment == "development" {
		log.Logger = log.Output(zerolog.ConsoleWriter{Out: io.MultiWriter(os.Stdout, f)})
	} else {
		log.Logger = log.Output(io.MultiWriter(os.Stdout, f))
	}
}

func runDBMigration(migrationURL string, dbSource string) error {
	migration, err := migrate.New(migrationURL, dbSource)
	if err != nil {
		return fmt.Errorf("cannot create new migrate instance: %w", err)
	}

	if err = migration.Up(); err != nil && err != migrate.ErrNoChange {
		return fmt.Errorf("failed to run migrate up: %w", err)
	}

	return nil
}

func newServer(config config.Config, connPool *pgxpool.Pool, redisOpt asynq.RedisClientOpt) (*http.Server, error) {
	if config.Environment != "development" && config.Environment != "testing" {
		gin.SetMode(gin.ReleaseMode)
	}

	router := gin.New()
	router.Use(middleware.Logger())
	router.Use(gin.Recovery())
	router.Use(middleware.RateLimiter())
	router.Use(middleware.ErrorHandler())

	srv := &http.Server{
		Addr:              config.HTTPServerAddress,
		Handler:           router,
		ReadTimeout:       10 * time.Second,
		WriteTimeout:      10 * time.Second,
		ReadHeaderTimeout: 10 * time.Second,
		IdleTimeout:       10 * time.Second,
		MaxHeaderBytes:    1 << 20,
	}

	// init token maker
	tokenMaker, err := token.NewJWTMaker(config.JWTSecret)
	if err != nil {
		return nil, fmt.Errorf("cannot create token maker: %w", err)
	}

	// init user repo
	userRepo := postgresql.NewUserRepository(connPool)

	// init session repo
	sessionRepo := postgresql.NewSessionRepository(connPool)

	// init verify email repo
	verifyEmailRepo := postgresql.NewVerifyEmailRepository(connPool)

	// init auth service
	authService := service.NewAuthService(userRepo, sessionRepo, tokenMaker, config.AccessTokenDuration, config.RefreshTokenDuration, verifyEmailRepo)

	// init userverifymail repo
	userVerifyEmailRepo := redisRepo.NewUserMessageBrokerRepository(redisOpt)

	// init user service
	userService := service.NewUserService(userRepo, authService, userVerifyEmailRepo)

	// init user handler and register routes
	handler.NewUserHandler(userService).RegisterRoutes(router, tokenMaker)

	// init account repo
	accountRepo := postgresql.NewAccountRepository(connPool)

	// init account service
	accountService := service.NewAccountService(accountRepo)

	// init account handler and register routes
	handler.NewAccountHandler(accountService).RegisterRoutes(router, tokenMaker)

	// init transfer repo
	transferRepo := postgresql.NewTransferRepository(connPool)

	// init transfer service
	transferService := service.NewTransferService(transferRepo)

	// init transfer handler and register routes
	handler.NewTransferHandler(transferService, accountService).RegisterRoutes(router, tokenMaker)

	return srv, nil
}

func runHTPPServer(ctx context.Context, waitGroup *errgroup.Group, config config.Config, connPool *pgxpool.Pool, redisOpt asynq.RedisClientOpt) {
	server, err := newServer(config, connPool, redisOpt)
	if err != nil {
		log.Fatal().Err(err).Msg("cannot create HTTP server")
	}

	waitGroup.Go(func() error {
		log.Info().Msg(fmt.Sprintf("start HTTP server on %s", server.Addr))
		if err := server.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			return fmt.Errorf("cannot start HTTP server: %w", err)
		}
		return nil
	})

	waitGroup.Go(func() error {
		<-ctx.Done()
		log.Info().Msg("shutting down HTTP server gracefully, press Ctrl+C again to force")

		if err := server.Shutdown(ctx); err != nil {
			return fmt.Errorf("HTTP server forced to shutdown: %w", err)
		}

		log.Info().Msg("HTTP server stopped")

		return nil
	})

}

func runTaskProcessor(
	ctx context.Context,
	waitGroup *errgroup.Group,
	config config.Config,
	pool *pgxpool.Pool,
	redisOpt asynq.RedisClientOpt,
) {
	// init mailer service
	mailer := service.NewGmailSender(config.EmailSenderName, config.EmailSenderAddress, config.EmailSenderPassword)

	// init user repo
	userRepo := postgresql.NewUserRepository(pool)

	// init verify email repo
	verifyEmailRepo := postgresql.NewVerifyEmailRepository(pool)

	taskProcessor := redisSvc.NewRedisTaskProcessor(redisOpt, mailer, userRepo, verifyEmailRepo)

	waitGroup.Go(func() error {
		log.Info().Msg("start task processor")
		if err := taskProcessor.Start(); err != nil && err != http.ErrServerClosed {
			return fmt.Errorf("failed to start task processor")
		}
		return nil
	})

	waitGroup.Go(func() error {
		<-ctx.Done()
		log.Info().Msg("shutting down task processor gracefully, press Ctrl+C again to force")

		taskProcessor.Shutdown()
		log.Info().Msg("task processor is stopped")

		return nil
	})
}
