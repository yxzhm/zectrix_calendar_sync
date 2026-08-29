package main

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"sort"
	"strings"
	"time"

	"github.com/teambition/rrule-go"
)

type Event struct {
	UID     string
	Title   string
	DueDate string
	DueTime string
}

func newWebCalHTTPClient(cfg Config) (*http.Client, error) {
	transport := http.DefaultTransport.(*http.Transport).Clone()
	transport.Proxy = http.ProxyFromEnvironment
	if cfg.WebCalProxy != "" {
		proxyURL, err := url.Parse(cfg.WebCalProxy)
		if err != nil || proxyURL.Scheme == "" || proxyURL.Host == "" {
			return nil, fmt.Errorf("invalid WEBCAL_PROXY %q", cfg.WebCalProxy)
		}
		transport.Proxy = http.ProxyURL(proxyURL)
	}
	return &http.Client{Transport: transport, Timeout: cfg.RequestTimeout}, nil
}

func googleCalendarFeedURL(rawURL string) (string, error) {
	rawURL = strings.TrimSpace(rawURL)
	if strings.HasPrefix(strings.ToLower(rawURL), "webcal://") {
		rawURL = "https://" + rawURL[len("webcal://"):]
	}
	parsed, err := url.Parse(rawURL)
	if err != nil || parsed.Scheme == "" || parsed.Host == "" {
		return "", fmt.Errorf("invalid GOOGLE_CALENDAR_URL %q", rawURL)
	}
	if parsed.Scheme != "http" && parsed.Scheme != "https" {
		return "", fmt.Errorf("unsupported GOOGLE_CALENDAR_URL scheme %q", parsed.Scheme)
	}
	return parsed.String(), nil
}

func (s *CalendarSyncer) fetchGoogleCalendarEvents(ctx context.Context) ([]Event, error) {
	feedURL, err := googleCalendarFeedURL(s.cfg.GoogleCalendarURL)
	if err != nil {
		return nil, err
	}

	var events []Event
	err = s.retry("fetch Google Calendar", func() error {
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, feedURL, nil)
		if err != nil {
			return err
		}
		req.Header.Set("Accept", "text/calendar")
		resp, err := s.calendarClient.Do(req)
		if err != nil {
			return err
		}
		defer resp.Body.Close()
		if resp.StatusCode < 200 || resp.StatusCode >= 300 {
			preview, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
			return fmt.Errorf("Google Calendar HTTP %s: %s", resp.Status, strings.TrimSpace(string(preview)))
		}
		calendarData, err := io.ReadAll(resp.Body)
		if err != nil {
			return fmt.Errorf("read Google Calendar response: %w", err)
		}
		parsed, err := parseICalendar(string(calendarData), s.now())
		if err != nil {
			return fmt.Errorf("parse Google Calendar: %w", err)
		}
		events = parsed
		return nil
	})
	return events, err
}

type icalProperty struct {
	Name   string
	Params map[string]string
	Value  string
}

func unfoldICal(data string) []string {
	data = strings.ReplaceAll(data, "\r\n", "\n")
	raw := strings.Split(data, "\n")
	lines := make([]string, 0, len(raw))
	for _, line := range raw {
		if len(line) > 0 && (line[0] == ' ' || line[0] == '\t') && len(lines) > 0 {
			lines[len(lines)-1] += line[1:]
		} else {
			lines = append(lines, strings.TrimSuffix(line, "\r"))
		}
	}
	return lines
}

func parseICalProperty(line string) (icalProperty, bool) {
	colon := strings.IndexByte(line, ':')
	if colon < 0 {
		return icalProperty{}, false
	}
	parts := strings.Split(line[:colon], ";")
	property := icalProperty{Name: strings.ToUpper(parts[0]), Params: make(map[string]string), Value: line[colon+1:]}
	for _, parameter := range parts[1:] {
		name, value, ok := strings.Cut(parameter, "=")
		if ok {
			property.Params[strings.ToUpper(name)] = strings.Trim(value, `"`)
		}
	}
	return property, true
}

func unescapeICalText(value string) string {
	replacer := strings.NewReplacer(`\n`, "\n", `\N`, "\n", `\,`, ",", `\;`, ";", `\\`, `\`)
	return replacer.Replace(value)
}

func parseICalTime(property icalProperty, local *time.Location) (time.Time, error) {
	value := property.Value
	if strings.EqualFold(property.Params["VALUE"], "DATE") || (len(value) == 8 && !strings.Contains(value, "T")) {
		date, err := time.ParseInLocation("20060102", value, local)
		if err != nil {
			return time.Time{}, err
		}
		return time.Date(date.Year(), date.Month(), date.Day(), 9, 0, 0, 0, local), nil
	}
	if strings.HasSuffix(value, "Z") {
		for _, layout := range []string{"20060102T150405Z", "20060102T1504Z"} {
			if parsed, err := time.Parse(layout, value); err == nil {
				return parsed.In(local), nil
			}
		}
	}
	location := local
	if tzid := property.Params["TZID"]; tzid != "" {
		loaded, err := time.LoadLocation(tzid)
		if err != nil {
			return time.Time{}, fmt.Errorf("unknown TZID %q: %w", tzid, err)
		}
		location = loaded
	}
	for _, layout := range []string{"20060102T150405", "20060102T1504"} {
		if parsed, err := time.ParseInLocation(layout, value, location); err == nil {
			return parsed.In(local), nil
		}
	}
	return time.Time{}, fmt.Errorf("unsupported DTSTART %q", value)
}

type icalEvent struct {
	properties map[string][]icalProperty
}

func (event icalEvent) first(name string) icalProperty {
	properties := event.properties[name]
	if len(properties) == 0 {
		return icalProperty{}
	}
	return properties[0]
}

func parseICalEvents(data string) []icalEvent {
	var events []icalEvent
	var current *icalEvent
	for _, line := range unfoldICal(data) {
		switch strings.ToUpper(line) {
		case "BEGIN:VEVENT":
			current = &icalEvent{properties: make(map[string][]icalProperty)}
		case "END:VEVENT":
			if current != nil {
				events = append(events, *current)
				current = nil
			}
		default:
			if current == nil {
				continue
			}
			if property, ok := parseICalProperty(line); ok {
				current.properties[property.Name] = append(current.properties[property.Name], property)
			}
		}
	}
	return events
}

func parseICalTimes(properties []icalProperty, local *time.Location) ([]time.Time, error) {
	var result []time.Time
	for _, property := range properties {
		for _, value := range strings.Split(property.Value, ",") {
			// An RDATE can be a PERIOD; its first value is the occurrence start.
			value, _, _ = strings.Cut(value, "/")
			item := property
			item.Value = value
			parsed, err := parseICalTime(item, local)
			if err != nil {
				return nil, err
			}
			result = append(result, parsed)
		}
	}
	return result, nil
}

func recurrenceKey(value time.Time) string {
	return value.UTC().Format("20060102T150405Z")
}

func recurringUID(uid string, recurrenceID time.Time) string {
	return uid + "::" + recurrenceKey(recurrenceID)
}

func eventDetails(event icalEvent) (uid, summary, status string) {
	uid = strings.TrimSpace(event.first("UID").Value)
	summary = strings.TrimSpace(unescapeICalText(event.first("SUMMARY").Value))
	status = strings.ToUpper(strings.TrimSpace(event.first("STATUS").Value))
	return
}

func isUsableEvent(summary, status string, start icalProperty) bool {
	lowerSummary := strings.ToLower(summary)
	isAllDay := strings.EqualFold(start.Params["VALUE"], "DATE") || (len(start.Value) == 8 && !strings.Contains(start.Value, "T"))
	return summary != "" && start.Name != "" && !isAllDay && status != "CANCELLED" &&
		!strings.Contains(summary, "已取消") &&
		!strings.Contains(lowerSummary, "cancelled") &&
		!strings.Contains(lowerSummary, "canceled")
}

func appendEventForDay(events []Event, seen map[string]struct{}, uid, summary string, start, startOfDay, endOfDay time.Time) []Event {
	if start.Before(startOfDay) || !start.Before(endOfDay) {
		return events
	}
	if _, exists := seen[uid]; exists {
		return events
	}
	seen[uid] = struct{}{}
	return append(events, Event{UID: uid, Title: summary, DueDate: start.Format("2006-01-02"), DueTime: start.Format("15:04")})
}

func parseICalendar(data string, now time.Time) ([]Event, error) {
	startOfDay := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, now.Location())
	endOfDay := time.Date(now.Year(), now.Month(), now.Day()+1, 0, 0, 0, 0, now.Location())
	components := parseICalEvents(data)

	// Changed and cancelled instances are separate VEVENTs. Index them first so
	// they can replace the matching occurrence generated from the series master.
	overrides := make(map[string]icalEvent)
	for _, component := range components {
		recurrenceID := component.first("RECURRENCE-ID")
		if recurrenceID.Name == "" {
			continue
		}
		parsed, err := parseICalTime(recurrenceID, now.Location())
		if err != nil {
			return nil, fmt.Errorf("parse RECURRENCE-ID: %w", err)
		}
		uid, _, _ := eventDetails(component)
		overrides[uid+"\x00"+recurrenceKey(parsed)] = component
	}

	var events []Event
	seen := make(map[string]struct{})
	for _, component := range components {
		uid, summary, status := eventDetails(component)
		if uid == "" {
			continue
		}
		startProperty := component.first("DTSTART")
		recurrenceIDProperty := component.first("RECURRENCE-ID")
		if recurrenceIDProperty.Name != "" {
			recurrenceID, err := parseICalTime(recurrenceIDProperty, now.Location())
			if err != nil {
				return nil, fmt.Errorf("parse RECURRENCE-ID: %w", err)
			}
			if !isUsableEvent(summary, status, startProperty) {
				continue
			}
			start, err := parseICalTime(startProperty, now.Location())
			if err != nil {
				return nil, err
			}
			events = appendEventForDay(events, seen, recurringUID(uid, recurrenceID), summary, start, startOfDay, endOfDay)
			continue
		}
		if !isUsableEvent(summary, status, startProperty) {
			continue
		}
		start, err := parseICalTime(startProperty, now.Location())
		if err != nil {
			return nil, err
		}

		recurring := len(component.properties["RRULE"]) > 0 || len(component.properties["RDATE"]) > 0
		if !recurring {
			events = appendEventForDay(events, seen, uid, summary, start, startOfDay, endOfDay)
			continue
		}

		occurrences := []time.Time{start}
		if rules := component.properties["RRULE"]; len(rules) > 0 {
			option, err := rrule.StrToROption(rules[0].Value)
			if err != nil {
				return nil, fmt.Errorf("parse RRULE for %q: %w", uid, err)
			}
			option.Dtstart = start
			rule, err := rrule.NewRRule(*option)
			if err != nil {
				return nil, fmt.Errorf("build RRULE for %q: %w", uid, err)
			}
			occurrences = rule.Between(startOfDay, endOfDay, true)
		}
		rdates, err := parseICalTimes(component.properties["RDATE"], now.Location())
		if err != nil {
			return nil, fmt.Errorf("parse RDATE for %q: %w", uid, err)
		}
		occurrences = append(occurrences, rdates...)
		exdates, err := parseICalTimes(component.properties["EXDATE"], now.Location())
		if err != nil {
			return nil, fmt.Errorf("parse EXDATE for %q: %w", uid, err)
		}
		excluded := make(map[string]struct{}, len(exdates))
		for _, date := range exdates {
			excluded[recurrenceKey(date)] = struct{}{}
		}
		for _, occurrence := range occurrences {
			key := recurrenceKey(occurrence)
			if _, exists := excluded[key]; exists {
				continue
			}
			if _, replaced := overrides[uid+"\x00"+key]; replaced {
				continue
			}
			events = appendEventForDay(events, seen, recurringUID(uid, occurrence), summary, occurrence, startOfDay, endOfDay)
		}
	}
	sort.Slice(events, func(i, j int) bool {
		if events[i].DueDate != events[j].DueDate {
			return events[i].DueDate < events[j].DueDate
		}
		if events[i].DueTime != events[j].DueTime {
			return events[i].DueTime < events[j].DueTime
		}
		return events[i].UID < events[j].UID
	})
	return events, nil
}
