// Команда shortener — точка входа сервиса сокращения ссылок.
package main

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"net/http"
	"net/url"
	"os"
	"os/signal"
	"sync"
	"syscall"
	"time"

	"github.com/Skufik2007/url-shortener/internal/analytics"
	"github.com/Skufik2007/url-shortener/internal/auth"
	"github.com/Skufik2007/url-shortener/internal/cache"
	"github.com/Skufik2007/url-shortener/internal/config"
	"github.com/Skufik2007/url-shortener/internal/httpapi"
	"github.com/Skufik2007/url-shortener/internal/service"
	"github.com/Skufik2007/url-shortener/internal/shortcode"
	"github.com/Skufik2007/url-shortener/internal/storage"
	"github.com/Skufik2007/url-shortener/internal/storage/memory"
	"github.com/Skufik2007/url-shortener/internal/storage/postgres"
	"github.com/Skufik2007/url-shortener/web"
)

const (
	bcryptCost      = 12
	shutdownTimeout = 15 * time.Second
	// dataSaveDebounce — задержка перед записью снимка после изменения данных (DATA_FILE).
	dataSaveDebounce = 500 * time.Millisecond
)

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, "ошибка:", err)
		os.Exit(1)
	}
}

func run() error {
	cfg, err := config.Load()
	if err != nil {
		return fmt.Errorf("конфигурация: %w", err)
	}
	level, _ := config.ParseLevel(cfg.LogLevel)
	log := slog.New(slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{Level: level}))
	slog.SetDefault(log)

	switch {
	case cfg.SecretFile != "":
		log.Info("AUTH_SECRET не задан: секрет подписи токенов хранится в файле", "path", cfg.SecretFile)
	case cfg.GeneratedSecret:
		log.Warn("AUTH_SECRET не задан: сгенерирован случайный секрет, токены перестанут действовать после перезапуска (демо-режим)")
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	// Хранилище.
	var store storage.Store
	switch {
	case cfg.DatabaseURL != "":
		pg, err := postgres.New(ctx, cfg.DatabaseURL)
		if err != nil {
			return fmt.Errorf("postgres: %w", err)
		}
		store = pg
		log.Info("хранилище: PostgreSQL")
	case cfg.DataFile != "":
		ps, err := memory.Open(cfg.DataFile, dataSaveDebounce, log)
		if err != nil {
			return fmt.Errorf("хранилище в файле: %w", err)
		}
		store = ps
		log.Info("хранилище: память с сохранением в файл", "path", cfg.DataFile)
	default:
		store = memory.New()
		log.Warn("хранилище: память (DATABASE_URL и DATA_FILE не заданы), данные пропадут при остановке")
	}
	defer closeQuietly(log, "store", store.Close)

	// Кэш.
	var lc cache.LinkCache
	if cfg.RedisURL != "" {
		rc, err := cache.NewRedis(ctx, cfg.RedisURL)
		if err != nil {
			return fmt.Errorf("redis: %w", err)
		}
		lc = rc
		log.Info("кэш: Redis")
	} else {
		lc = cache.NewMemory()
		log.Warn("кэш: память (REDIS_URL не задан)")
	}
	defer closeQuietly(log, "cache", lc.Close)

	// GeoIP.
	var geo analytics.GeoResolver = analytics.NoopGeo{}
	if cfg.GeoIPDB != "" {
		mm, err := analytics.OpenMaxMind(cfg.GeoIPDB)
		if err != nil {
			return fmt.Errorf("geoip: %w", err)
		}
		defer closeQuietly(log, "geoip", mm.Close)
		geo = mm
		log.Info("GeoIP включён", "db", cfg.GeoIPDB)
	}

	base, err := url.Parse(cfg.BaseURL)
	if err != nil {
		return fmt.Errorf("BASE_URL: %w", err)
	}

	// Сборщик аналитики.
	collector := analytics.NewCollector(analytics.Config{
		Workers:       cfg.AnalyticsWorkers,
		Buffer:        cfg.AnalyticsBuffer,
		BatchSize:     cfg.AnalyticsBatch,
		FlushInterval: cfg.AnalyticsFlush,
		SelfHost:      base.Hostname(),
		Salt:          "visitor:" + cfg.AuthSecret,
	}, store, store, geo, log)

	collectorCtx, cancelCollector := context.WithCancel(context.Background())
	var collectorDone sync.WaitGroup
	collectorDone.Add(1)
	go func() {
		defer collectorDone.Done()
		collector.Run(collectorCtx)
	}()

	// Сервисы.
	authSvc := auth.New(store, []byte(cfg.AuthSecret), cfg.TokenTTL, bcryptCost)
	opts := service.DefaultOptions()
	opts.PublicHost = base.Host
	links := service.New(store, lc, shortcode.NewRandom(cfg.CodeLength), collector, opts, log)

	dashboard, err := fs.Sub(web.Static, "static")
	if err != nil {
		cancelCollector()
		collectorDone.Wait()
		return fmt.Errorf("встроенная статика: %w", err)
	}

	handler := httpapi.NewServer(httpapi.Deps{
		Links: links,
		Auth:  authSvc,
		Health: []httpapi.HealthCheck{
			{Name: "database", Ping: store.Ping},
			{Name: "cache", Ping: lc.Ping},
		},
		CollectorStats: func() httpapi.CollectorStats {
			s := collector.Stats()
			return httpapi.CollectorStats{Accepted: s.Accepted, Dropped: s.Dropped, Written: s.Written, Failed: s.Failed}
		},
		BaseURL:        cfg.BaseURL,
		TrustProxy:     cfg.TrustProxy,
		RateLimitRPS:   cfg.RateLimitRPS,
		RateLimitBurst: cfg.RateLimitBurst,
		Dashboard:      dashboard,
		Logger:         log,
	})

	srv := &http.Server{
		Addr:              cfg.HTTPAddr,
		Handler:           handler,
		ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout:       15 * time.Second,
		WriteTimeout:      30 * time.Second,
		IdleTimeout:       90 * time.Second,
		MaxHeaderBytes:    1 << 16,
	}

	serveErr := make(chan error, 1)
	go func() {
		log.Info("сервер запущен", "addr", cfg.HTTPAddr, "base_url", cfg.BaseURL)
		if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			serveErr <- err
		}
		close(serveErr)
	}()

	var runErr error
	select {
	case <-ctx.Done():
		log.Info("получен сигнал остановки")
	case err := <-serveErr:
		if err != nil {
			runErr = fmt.Errorf("http-сервер: %w", err)
		}
	}

	// Корректная остановка: сервер -> сборщик (дренаж очереди) -> store/cache (defer).
	shutdownCtx, cancel := context.WithTimeout(context.Background(), shutdownTimeout)
	defer cancel()
	if err := srv.Shutdown(shutdownCtx); err != nil {
		log.Error("остановка http-сервера", "err", err)
	}
	cancelCollector()
	collectorDone.Wait()
	s := collector.Stats()
	log.Info("сборщик аналитики остановлен",
		"accepted", s.Accepted, "dropped", s.Dropped, "written", s.Written, "failed", s.Failed)
	return runErr
}

func closeQuietly(log *slog.Logger, name string, closeFn func() error) {
	if err := closeFn(); err != nil {
		log.Warn("ошибка при закрытии", "component", name, "err", err)
	}
}
