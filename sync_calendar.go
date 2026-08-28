package main

import (
	"context"
	"flag"
	"fmt"
	"log"
	"net/http"
	"time"
)

type CalendarSyncer struct {
	cfg            Config
	dryRun         bool
	zectrixClient  *http.Client
	calendarClient *http.Client
	now            func() time.Time
	sleep          func(time.Duration)
	maxRetries     int
	existingTodos  []Todo
	uidMap         map[string]Todo
}

func newHTTPClients(cfg Config) (*http.Client, *http.Client, error) {
	zectrixClient := newZectrixHTTPClient(cfg)
	calendarClient, err := newWebCalHTTPClient(cfg)
	if err != nil {
		return nil, nil, err
	}
	return zectrixClient, calendarClient, nil
}

func newCalendarSyncer(cfg Config, dryRun bool) (*CalendarSyncer, error) {
	zectrixClient, calendarClient, err := newHTTPClients(cfg)
	if err != nil {
		return nil, err
	}
	return &CalendarSyncer{
		cfg: cfg, dryRun: dryRun,
		zectrixClient: zectrixClient, calendarClient: calendarClient,
		now: time.Now, sleep: time.Sleep, maxRetries: 3,
		uidMap: make(map[string]Todo),
	}, nil
}

func (s *CalendarSyncer) retry(operation string, fn func() error) error {
	var lastErr error
	for attempt := 0; attempt < s.maxRetries; attempt++ {
		if err := fn(); err == nil {
			return nil
		} else {
			lastErr = err
		}
		if attempt+1 < s.maxRetries {
			delay := time.Second << attempt
			log.Printf("%s failed (attempt %d/%d): %v; retrying in %s", operation, attempt+1, s.maxRetries, lastErr, delay)
			s.sleep(delay)
		}
	}
	return fmt.Errorf("%s failed after %d attempts: %w", operation, s.maxRetries, lastErr)
}

func (s *CalendarSyncer) run(ctx context.Context) error {
	log.Printf("starting calendar sync at %s", s.now().Format(time.RFC3339))

	events, err := s.fetchGoogleCalendarEvents(ctx)
	if err != nil {
		return err
	}
	// Preserve the original script's safety behavior: an empty result does not
	// trigger deletion, because an empty successful response can be caused by a
	// temporarily unavailable calendar feed.
	if len(events) == 0 {
		log.Print("no remaining events returned; skipping event synchronization")
		return nil
	}

	if err := s.getExistingTodos(ctx); err != nil {
		return err
	}
	if err := s.completeExpiredTodos(ctx); err != nil {
		log.Printf("some expired todos could not be completed: %v", err)
	}

	return s.syncEvents(ctx, events)
}

func main() {
	dryRun := flag.Bool("dry-run", false, "show changes without writing to Zectrix")
	flag.Parse()
	cfg, err := loadConfig()
	if err != nil {
		log.Fatal(err)
	}
	if cfg.GoogleCalendarURL == "" {
		log.Fatal("GOOGLE_CALENDAR_URL is required")
	}
	syncer, err := newCalendarSyncer(cfg, *dryRun)
	if err != nil {
		log.Fatal(err)
	}
	if *dryRun {
		log.Print("DRY RUN mode: no Zectrix writes will be made")
	}
	if err := syncer.run(context.Background()); err != nil {
		log.Fatal(err)
	}
}
