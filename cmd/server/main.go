package main

import (
	"context"
	"fmt"
	"os"
	"os/signal"
	"syscall"

	"github.com/soulteary/gorge-webhook/internal/config"
	"github.com/soulteary/gorge-webhook/internal/delivery"
	"github.com/soulteary/gorge-webhook/internal/httpapi"

	"github.com/labstack/echo/v4"
	"github.com/labstack/echo/v4/middleware"
)

func main() {
	cfg := config.LoadFromEnv()

	store, err := delivery.NewStore(cfg.HeraldDSN(), cfg.ErrorBackoffSec, cfg.ErrorThreshold)
	if err != nil {
		fmt.Fprintf(os.Stderr, "failed to connect to database: %v\n", err)
		os.Exit(1)
	}
	defer store.Close()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	dispatcher := delivery.NewDispatcher(
		store,
		cfg.DeliveryTimeout,
		cfg.PollIntervalMs,
		cfg.MaxConcurrent,
		cfg.ErrorBackoffSec,
	)
	go dispatcher.Run(ctx)

	e := echo.New()
	e.Use(middleware.RequestLoggerWithConfig(middleware.RequestLoggerConfig{
		LogStatus: true, LogURI: true, LogMethod: true,
		LogValuesFunc: func(c echo.Context, v middleware.RequestLoggerValues) error {
			c.Logger().Infof("%s %s %d", v.Method, v.URI, v.Status)
			return nil
		},
	}))
	e.Use(middleware.Recover())

	httpapi.RegisterRoutes(e, &httpapi.Deps{
		Store: store,
		Token: cfg.ServiceToken,
	})

	go func() {
		sigCh := make(chan os.Signal, 1)
		signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)
		<-sigCh
		cancel()
		_ = e.Shutdown(context.Background())
	}()

	e.Logger.Fatal(e.Start(cfg.ListenAddr))
}
