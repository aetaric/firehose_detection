package main

import (
	"context"
	"errors"
	"flag"
	"log/slog"
	"maps"
	"os"
	"os/signal"
	"slices"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/bluesky-social/jetstream"
)

func main() {
	host := flag.String("host", "jetstream.us-west.bsky.network", "Jetstream host")
	rulesPath := flag.String("rules", "rules.yaml", "path to rules file (reloaded on SIGHUP)")
	cursorPath := flag.String("cursor", "", "file to persist the resume cursor in (empty = always start at live tip)")
	flag.Parse()

	log := slog.New(slog.NewTextHandler(os.Stderr, nil))
	if err := run(log, *host, *rulesPath, *cursorPath); err != nil {
		log.Error("exiting", "err", err)
		os.Exit(1)
	}
}

func run(log *slog.Logger, host, rulesPath, cursorPath string) error {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	reporter, err := setupReporter(ctx, log)
	if err != nil {
		return err
	}

	m, err := LoadRules(rulesPath)
	if err != nil {
		return err
	}
	if err := checkAutoReport(m, reporter); err != nil {
		return err
	}
	var matcher atomic.Pointer[Matcher]
	matcher.Store(m)
	log.Info("loaded rules", "count", m.Len(), "path", rulesPath, "auto_report", m.HasAutoReport())
	go reloadOnHUP(ctx, log, rulesPath, reporter, &matcher)

	sink := MultiSink{NewJSONLSink(os.Stdout)}
	defer sink.Flush()
	if reporter != nil {
		auto := NewReportSink(log, reporter)
		sink = append(sink, auto)
		defer func() {
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			auto.Close(ctx)
		}()
	}
	discord, err := setupDiscord(log, reporter)
	if err != nil {
		return err
	}
	if discord != nil {
		sink = append(sink, discord)
		defer func() {
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			discord.Close(ctx)
		}()
	}

	cursor, err := loadCursor(cursorPath)
	if err != nil {
		return err
	}

	opts := []jetstream.Option{
		jetstream.WithCollections(slices.Collect(maps.Keys(extractors))),
		jetstream.WithKinds([]jetstream.Kind{jetstream.KindCommit}),
		jetstream.WithLogger(log),
	}
	if cursor > 0 {
		opts = append(opts, jetstream.WithLiveCursor(cursor))
		log.Info("resuming", "cursor", cursor)
	}
	client, err := jetstream.Subscribe(host, opts...)
	if err != nil {
		return err
	}
	defer client.Close()

	var (
		lastSave              time.Time
		seen, matched, errCnt uint64
	)
	statsTick := time.NewTicker(time.Minute)
	defer statsTick.Stop()

	for batch, err := range client.Events(ctx) {
		if err != nil {
			if errors.Is(err, jetstream.ErrFatal) {
				return err
			}
			errCnt++
			log.Warn("stream error", "err", err)
			continue
		}

		m := matcher.Load()
		for _, evt := range batch.Events() {
			c := evt.Commit
			if c == nil || (c.Operation != jetstream.OpCreate && c.Operation != jetstream.OpUpdate) {
				continue
			}
			extract, ok := extractors[c.Collection]
			if !ok {
				continue
			}
			seen++
			text := extract(c.Record)
			hits := m.Match(c.Collection, text)
			if len(hits) == 0 {
				continue
			}
			matched++
			createdAt, _ := c.Record["createdAt"].(string)
			if err := sink.Emit(Detection{
				URI:        "at://" + evt.DID + "/" + c.Collection + "/" + c.Rkey,
				CID:        c.CID,
				DID:        evt.DID,
				Collection: c.Collection,
				Operation:  string(c.Operation),
				Seq:        evt.Seq,
				TimeUS:     evt.TimeUS,
				CreatedAt:  createdAt,
				Text:       text,
				Media:      mediaSummary(c.Record),
				Hits:       hits,
			}); err != nil {
				return err
			}
		}

		// Flush detections before advancing the cursor so a crash re-delivers
		// rather than drops (at-least-once).
		if err := sink.Flush(); err != nil {
			return err
		}
		if cursorPath != "" && time.Since(lastSave) > 5*time.Second {
			if c := batch.LastCursor(); c > 0 {
				if err := saveCursor(cursorPath, c); err != nil {
					log.Warn("saving cursor", "err", err)
				}
				lastSave = time.Now()
			}
		}

		select {
		case <-statsTick.C:
			log.Info("stats", "records", seen, "matched", matched, "stream_errors", errCnt)
		default:
		}
	}
	return nil
}

// setupReporter builds the Bluesky reporter from the environment, or returns
// nil if BSKY_HANDLE/BSKY_APP_PASSWORD are unset. Secrets come from env, not
// flags, so they don't show up in process listings. The reporter is shared by
// auto_report rules and the Discord report button.
func setupReporter(ctx context.Context, log *slog.Logger) (*Reporter, error) {
	handle, pw := os.Getenv("BSKY_HANDLE"), os.Getenv("BSKY_APP_PASSWORD")
	if handle == "" || pw == "" {
		log.Warn("BSKY_HANDLE/BSKY_APP_PASSWORD unset; no auto_report and no discord report buttons")
		return nil, nil
	}
	entryway := os.Getenv("BSKY_PDS")
	if entryway == "" {
		entryway = "https://bsky.social"
	}
	reporter := NewReporter(entryway, handle, pw)
	if err := reporter.Login(ctx); err != nil {
		return nil, err
	}
	log.Info("bluesky reporting enabled", "handle", handle)
	return reporter, nil
}

// setupDiscord builds the Discord sink from the environment, or returns nil
// if DISCORD_TOKEN is unset.
func setupDiscord(log *slog.Logger, reporter *Reporter) (*DiscordSink, error) {
	token := os.Getenv("DISCORD_TOKEN")
	if token == "" {
		return nil, nil
	}
	channel := os.Getenv("DISCORD_CHANNEL_ID")
	if channel == "" {
		return nil, errors.New("DISCORD_CHANNEL_ID is required when DISCORD_TOKEN is set")
	}

	d, err := NewDiscordSink(log, token, channel, os.Getenv("DISCORD_REPORT_ROLE_ID"), reporter)
	if err != nil {
		return nil, err
	}
	log.Info("discord output enabled", "channel", channel)
	return d, nil
}

func reloadOnHUP(ctx context.Context, log *slog.Logger, path string, reporter *Reporter, dst *atomic.Pointer[Matcher]) {
	hup := make(chan os.Signal, 1)
	signal.Notify(hup, syscall.SIGHUP)
	defer signal.Stop(hup)
	for {
		select {
		case <-ctx.Done():
			return
		case <-hup:
			m, err := LoadRules(path)
			if err == nil {
				err = checkAutoReport(m, reporter)
			}
			if err != nil {
				log.Error("reload rules failed; keeping previous set", "err", err)
				continue
			}
			dst.Store(m)
			log.Info("reloaded rules", "count", m.Len(), "auto_report", m.HasAutoReport())
		}
	}
}
