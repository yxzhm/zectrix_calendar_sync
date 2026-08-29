package main

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func TestParseICalendar(t *testing.T) {
	location := time.FixedZone("UTC+8", 8*60*60)
	now := time.Date(2026, 8, 27, 10, 0, 0, 0, location)
	ics := "BEGIN:VCALENDAR\r\n" +
		"BEGIN:VEVENT\r\nUID:meeting-1\r\nSUMMARY:Planning\\, phase\r\n two\r\nDTSTART:20260827T113000\r\nEND:VEVENT\r\n" +
		"BEGIN:VEVENT\r\nUID:all-day\r\nSUMMARY:All day\r\nDTSTART;VALUE=DATE:20260827\r\nEND:VEVENT\r\n" +
		"BEGIN:VEVENT\r\nUID:cancelled\r\nSUMMARY:Cancelled meeting\r\nDTSTART:20260827T120000\r\nSTATUS:CANCELLED\r\nEND:VEVENT\r\n" +
		"BEGIN:VEVENT\r\nUID:tomorrow\r\nSUMMARY:Tomorrow\r\nDTSTART:20260828T090000\r\nEND:VEVENT\r\nEND:VCALENDAR\r\n"

	events, err := parseICalendar(ics, now)
	if err != nil {
		t.Fatal(err)
	}
	if len(events) != 1 {
		t.Fatalf("got %d events, want 1: %#v", len(events), events)
	}
	want := Event{UID: "meeting-1", Title: "Planning, phasetwo", DueDate: "2026-08-27", DueTime: "11:30"}
	if events[0] != want {
		t.Fatalf("got %#v, want %#v", events[0], want)
	}
}

func TestParseICalendarUTCAndTZID(t *testing.T) {
	shanghai, err := time.LoadLocation("Asia/Shanghai")
	if err != nil {
		t.Skipf("timezone database unavailable: %v", err)
	}
	now := time.Date(2026, 8, 27, 8, 0, 0, 0, shanghai)
	ics := "BEGIN:VCALENDAR\n" +
		"BEGIN:VEVENT\nUID:utc\nSUMMARY:UTC event\nDTSTART:20260827T020000Z\nEND:VEVENT\n" +
		"BEGIN:VEVENT\nUID:tokyo\nSUMMARY:Tokyo event\nDTSTART;TZID=Asia/Tokyo:20260827T120000\nEND:VEVENT\n" +
		"END:VCALENDAR\n"
	events, err := parseICalendar(ics, now)
	if err != nil {
		t.Fatal(err)
	}
	if len(events) != 2 {
		t.Fatalf("got %d events, want 2", len(events))
	}
	if events[0].DueTime != "10:00" || events[1].DueTime != "11:00" {
		t.Fatalf("unexpected local times: %#v", events)
	}
}

func TestParseICalendarRecurringEvent(t *testing.T) {
	location := time.FixedZone("UTC+8", 8*60*60)
	now := time.Date(2026, 8, 27, 10, 0, 0, 0, location)
	ics := "BEGIN:VCALENDAR\n" +
		"BEGIN:VEVENT\nUID:daily\nSUMMARY:Daily stand-up\nDTSTART:20260825T093000\nRRULE:FREQ=DAILY;COUNT=5\nEND:VEVENT\n" +
		"END:VCALENDAR\n"

	events, err := parseICalendar(ics, now)
	if err != nil {
		t.Fatal(err)
	}
	want := []Event{{
		UID: "daily::20260827T013000Z", Title: "Daily stand-up",
		DueDate: "2026-08-27", DueTime: "09:30",
	}}
	if len(events) != len(want) || events[0] != want[0] {
		t.Fatalf("got %#v, want %#v", events, want)
	}
}

func TestParseICalendarRecurringExceptions(t *testing.T) {
	location := time.FixedZone("UTC+8", 8*60*60)
	now := time.Date(2026, 8, 27, 10, 0, 0, 0, location)
	ics := "BEGIN:VCALENDAR\n" +
		"BEGIN:VEVENT\nUID:moved\nSUMMARY:Original\nDTSTART:20260825T090000\nRRULE:FREQ=DAILY;COUNT=5\nEXDATE:20260826T090000\nEND:VEVENT\n" +
		"BEGIN:VEVENT\nUID:moved\nRECURRENCE-ID:20260827T090000\nSUMMARY:Moved instance\nDTSTART:20260827T140000\nEND:VEVENT\n" +
		"BEGIN:VEVENT\nUID:cancelled-series\nSUMMARY:Cancelable\nDTSTART:20260825T110000\nRRULE:FREQ=DAILY;COUNT=5\nEND:VEVENT\n" +
		"BEGIN:VEVENT\nUID:cancelled-series\nRECURRENCE-ID:20260827T110000\nSUMMARY:Cancelable\nDTSTART:20260827T110000\nSTATUS:CANCELLED\nEND:VEVENT\n" +
		"END:VCALENDAR\n"

	events, err := parseICalendar(ics, now)
	if err != nil {
		t.Fatal(err)
	}
	want := Event{
		UID: "moved::20260827T010000Z", Title: "Moved instance",
		DueDate: "2026-08-27", DueTime: "14:00",
	}
	if len(events) != 1 || events[0] != want {
		t.Fatalf("got %#v, want %#v", events, []Event{want})
	}
}

func TestGoogleCalendarFeedURL(t *testing.T) {
	got, err := googleCalendarFeedURL("webcal://calendar.google.com/calendar/ical/example/basic.ics")
	if err != nil {
		t.Fatal(err)
	}
	if want := "https://calendar.google.com/calendar/ical/example/basic.ics"; got != want {
		t.Fatalf("got %q, want %q", got, want)
	}

	if _, err := googleCalendarFeedURL("ftp://calendar.google.com/example.ics"); err == nil {
		t.Fatal("expected unsupported URL scheme error")
	}
}

func TestGoogleCalendarUsesExplicitWebCalProxy(t *testing.T) {
	var targetRequests atomic.Int32
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		targetRequests.Add(1)
		if r.Method != http.MethodGet || r.URL.Path != "/calendar.ics" {
			http.Error(w, "unexpected request", http.StatusNotFound)
			return
		}
		if accept := r.Header.Get("Accept"); accept != "text/calendar" {
			t.Errorf("Accept = %q, want text/calendar", accept)
		}
		w.Header().Set("Content-Type", "text/calendar")
		_, _ = io.WriteString(w, "BEGIN:VCALENDAR\nBEGIN:VEVENT\nUID:proxy-event\nSUMMARY:Via proxy\nDTSTART:20260827T120000\nEND:VEVENT\nEND:VCALENDAR\n")
	}))
	defer target.Close()

	var proxyRequests atomic.Int32
	proxy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		proxyRequests.Add(1)
		forward := r.Clone(r.Context())
		forward.RequestURI = ""
		forward.Header.Del("Proxy-Connection")
		resp, err := http.DefaultTransport.RoundTrip(forward)
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadGateway)
			return
		}
		defer resp.Body.Close()
		for key, values := range resp.Header {
			for _, value := range values {
				w.Header().Add(key, value)
			}
		}
		w.WriteHeader(resp.StatusCode)
		_, _ = io.Copy(w, resp.Body)
	}))
	defer proxy.Close()

	cfg := Config{
		ZectrixAPIBase: target.URL, GoogleCalendarURL: target.URL + "/calendar.ics",
		WebCalProxy: proxy.URL, RequestTimeout: 2 * time.Second,
	}
	syncer, err := newCalendarSyncer(cfg, false)
	if err != nil {
		t.Fatal(err)
	}
	syncer.now = func() time.Time { return time.Date(2026, 8, 27, 10, 0, 0, 0, time.Local) }
	syncer.sleep = func(time.Duration) {}
	events, err := syncer.fetchGoogleCalendarEvents(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(events) != 1 || events[0].UID != "proxy-event" {
		t.Fatalf("unexpected events: %#v", events)
	}
	if proxyRequests.Load() != 1 || targetRequests.Load() != 1 {
		t.Fatalf("proxy requests=%d target requests=%d, want 1 each", proxyRequests.Load(), targetRequests.Load())
	}

	// A Zectrix request uses its own client and must not consume WEBCAL_PROXY.
	before := proxyRequests.Load()
	_, _ = syncer.zectrixRequest(context.Background(), http.MethodGet, "/unrelated", nil)
	if proxyRequests.Load() != before {
		t.Fatalf("Zectrix request unexpectedly used webcal proxy")
	}
}

func TestSyncEventsCreatesUpdatesAndDeletes(t *testing.T) {
	var mu sync.Mutex
	var requests []string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		mu.Lock()
		requests = append(requests, r.Method+" "+r.URL.Path+" "+string(body))
		mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(apiResponse{Code: 0})
	}))
	defer server.Close()

	cfg := Config{ZectrixAPIBase: server.URL, RequestTimeout: 2 * time.Second}
	syncer, err := newCalendarSyncer(cfg, false)
	if err != nil {
		t.Fatal(err)
	}
	syncer.sleep = func(time.Duration) {}
	changed := Todo{ID: json.RawMessage(`1`), Title: "[日历] Old title", Description: "UID: changed", DueDate: "2026-08-27", DueTime: "10:30", Status: 0}
	removed := Todo{ID: json.RawMessage(`"two"`), Title: "[日历] Removed", Description: "UID: removed", DueDate: "2026-08-27", DueTime: "14:00", Status: 0}
	syncer.existingTodos = []Todo{changed, removed}
	syncer.uidMap = map[string]Todo{"changed": changed, "removed": removed}

	events := []Event{
		{UID: "changed", Title: "New title", DueDate: "2026-08-27", DueTime: "11:00"},
		{UID: "created", Title: "Brand new", DueDate: "2026-08-27", DueTime: "12:00"},
	}
	if err := syncer.syncEvents(context.Background(), events); err != nil {
		t.Fatal(err)
	}
	mu.Lock()
	got := strings.Join(requests, "\n")
	mu.Unlock()
	for _, expected := range []string{"PUT /todos/1 ", "POST /todos ", "DELETE /todos/two "} {
		if !strings.Contains(got, expected) {
			t.Errorf("missing %q in requests:\n%s", expected, got)
		}
	}
	if !strings.Contains(got, `"title":"[日历] New title"`) || !strings.Contains(got, `"deviceId":""`) {
		// The empty device ID belongs to create payload in this isolated test.
		t.Errorf("request payloads do not preserve expected fields:\n%s", got)
	}
}

func TestInvalidWebCalProxy(t *testing.T) {
	_, _, err := newHTTPClients(Config{WebCalProxy: "://bad", RequestTimeout: time.Second})
	if err == nil {
		t.Fatal("expected invalid proxy error")
	}
}
