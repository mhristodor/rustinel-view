// Command rustinel-view parses a rustinel alerts file and a rustinel
// events log, loads them into an in-process Redis (miniredis), and serves
// an HTMX-driven killchain triage UI.
//
// Usage:
//
//	rustinel-view [flags] <alerts.log> <events.log>
//
// The viewer is fully self-contained: no external Redis to install, no
// JSON API surface, no JavaScript framework — only HTMX over server-rendered
// HTML fragments. Redis lives for the lifetime of the binary.
package main

import (
	"context"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"runtime"
	"syscall"
	"time"

	"github.com/alicebob/miniredis/v2"
	"github.com/redis/go-redis/v9"

	alertingest "github.com/mhristodor/rustinel-view/internal/ingest/alert"
	eventingest "github.com/mhristodor/rustinel-view/internal/ingest/event"
	"github.com/mhristodor/rustinel-view/internal/server"
	"github.com/mhristodor/rustinel-view/internal/store"
)

func main() {
	var (
		addr      = flag.String("addr", ":8080", "HTTP listen address")
		redisPort = flag.Int("redis-port", 0, "miniredis listen port (0 = OS-assigned)")
		logLevel  = flag.String("log-level", "info", "debug|info|warn|error")
		noFlush   = flag.Bool("no-flush", false, "skip FLUSHALL at startup (debugging aid)")
	)
	flag.Usage = func() {
		fmt.Fprintf(os.Stderr, "usage: %s [flags] <alerts.log> <events.log>\n\nflags:\n", os.Args[0])
		flag.PrintDefaults()
	}
	flag.Parse()

	if flag.NArg() < 2 {
		flag.Usage()
		os.Exit(2)
	}
	alertsPath := flag.Arg(0)
	eventsPath := flag.Arg(1)

	log := newLogger(*logLevel)

	if err := run(log, *addr, *redisPort, *noFlush, alertsPath, eventsPath); err != nil {
		log.Error("fatal", "err", err)
		os.Exit(1)
	}
}

func run(log *slog.Logger, addr string, redisPort int, noFlush bool, alertsPath, eventsPath string) error {
	ctx, cancel := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer cancel()

	// Spin up in-process Redis.
	mr := miniredis.NewMiniRedis()
	if err := startMiniredis(mr, redisPort); err != nil {
		return fmt.Errorf("start miniredis: %w", err)
	}
	defer mr.Close()
	log.Info("embedded redis started", "addr", mr.Addr())

	// Connect.
	rdb := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	defer rdb.Close()
	if err := rdb.Ping(ctx).Err(); err != nil {
		return fmt.Errorf("ping embedded redis: %w", err)
	}

	s := store.New(rdb)

	if !noFlush {
		if err := s.FlushAll(ctx); err != nil {
			return fmt.Errorf("flushall: %w", err)
		}
	}

	// Ingest both files.
	meta, err := ingest(ctx, log, s, alertsPath, eventsPath)
	if err != nil {
		return err
	}
	if err := s.WriteMeta(ctx, meta); err != nil {
		return fmt.Errorf("write meta: %w", err)
	}

	// Warm the in-memory read snapshot so the first page request is as
	// fast as every subsequent one (single decode pass, ~100-200ms here
	// instead of on the first user click).
	warmStart := time.Now()
	n, err := s.Warm(ctx)
	if err != nil {
		return fmt.Errorf("warm snapshot: %w", err)
	}
	log.Info("snapshot warmed", "rows", n, "ms", time.Since(warmStart).Milliseconds())

	logMemory(log)

	// Serve.
	srv := server.New(s, log)
	return srv.ListenAndServe(ctx, addr)
}

// startMiniredis starts the embedded server, optionally on a specific port.
func startMiniredis(mr *miniredis.Miniredis, port int) error {
	if port > 0 {
		return mr.StartAddr(fmt.Sprintf("127.0.0.1:%d", port))
	}
	return mr.Start()
}

// ingest runs both parsers and returns a populated Meta.
func ingest(ctx context.Context, log *slog.Logger, s *store.Store, alertsPath, eventsPath string) (store.Meta, error) {
	meta := store.Meta{StartedAt: time.Now().UTC()}

	// Alerts
	af, err := os.Open(alertsPath)
	if err != nil {
		return meta, fmt.Errorf("open alerts: %w", err)
	}
	batch := s.NewBatch()
	res := alertingest.Parse(ctx, af, batch)
	af.Close()
	if err := batch.Close(ctx); err != nil {
		return meta, fmt.Errorf("flush alerts batch: %w", err)
	}
	meta.AlertsParsed = res.Parsed
	meta.AlertsFailed = res.Failed
	meta.SigmaCount = res.EngineCounts["Sigma"]
	meta.YaraCount = res.EngineCounts["Yara"]
	meta.TopRule, meta.TopRuleCount = topByCount(res.RuleCounts)
	log.Info("alerts ingested",
		"parsed", res.Parsed, "failed", res.Failed,
		"sigma", meta.SigmaCount, "yara", meta.YaraCount,
		"top_rule", meta.TopRule)
	for _, e := range res.Errors {
		log.Warn("alert parse", "err", e)
	}

	// Events
	ef, err := os.Open(eventsPath)
	if err != nil {
		return meta, fmt.Errorf("open events: %w", err)
	}
	batch = s.NewBatch()
	eres := eventingest.Parse(ctx, ef, batch)
	ef.Close()
	if err := batch.Close(ctx); err != nil {
		return meta, fmt.Errorf("flush events batch: %w", err)
	}
	meta.EventsParsed = eres.Parsed
	meta.EventsFailed = eres.Failed
	meta.DnsCount = eres.CategoryCounts["Dns"]
	meta.FileCount = eres.CategoryCounts["File"]
	meta.NetworkCount = eres.CategoryCounts["Network"]
	meta.ProcessCount = eres.CategoryCounts["Process"]
	meta.EarliestEvent = eres.MinTime
	meta.LatestEvent = eres.MaxTime
	log.Info("events ingested",
		"parsed", eres.Parsed, "failed", eres.Failed,
		"dns", meta.DnsCount, "file", meta.FileCount,
		"net", meta.NetworkCount, "proc", meta.ProcessCount,
		"span", eres.MaxTime.Sub(eres.MinTime).String())
	for i, e := range eres.Errors {
		if i >= 10 {
			log.Warn("event parse", "more_errors", len(eres.Errors)-10)
			break
		}
		log.Warn("event parse", "err", e)
	}

	meta.FinishedAt = time.Now().UTC()
	meta.IngestMillis = meta.FinishedAt.Sub(meta.StartedAt).Milliseconds()
	log.Info("ingest complete", "ms", meta.IngestMillis)
	return meta, nil
}

// topByCount returns the name/count of the single most-frequent key in the
// supplied map. Used to surface the "what's noisy" top rule in the header.
func topByCount(m map[string]int) (string, int) {
	var topK string
	var topV int
	for k, v := range m {
		if v > topV {
			topK, topV = k, v
		}
	}
	return topK, topV
}

func logMemory(log *slog.Logger) {
	var ms runtime.MemStats
	runtime.ReadMemStats(&ms)
	log.Info("resident memory", "alloc_mb", ms.Alloc/(1024*1024), "sys_mb", ms.Sys/(1024*1024))
}

func newLogger(level string) *slog.Logger {
	var l slog.Level
	switch level {
	case "debug":
		l = slog.LevelDebug
	case "warn":
		l = slog.LevelWarn
	case "error":
		l = slog.LevelError
	default:
		l = slog.LevelInfo
	}
	return slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: l}))
}
