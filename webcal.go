package main

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
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

func parseICalendar(data string, now time.Time) ([]Event, error) {
	var events []Event
	inEvent := false
	properties := make(map[string]icalProperty)
	startOfDay := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, now.Location())
	endOfDay := time.Date(now.Year(), now.Month(), now.Day()+1, 0, 0, 0, 0, now.Location())
	for _, line := range unfoldICal(data) {
		switch strings.ToUpper(line) {
		case "BEGIN:VEVENT":
			inEvent = true
			properties = make(map[string]icalProperty)
			continue
		case "END:VEVENT":
			if !inEvent {
				continue
			}
			inEvent = false
			summary := strings.TrimSpace(unescapeICalText(properties["SUMMARY"].Value))
			uid := strings.TrimSpace(properties["UID"].Value)
			status := strings.ToUpper(strings.TrimSpace(properties["STATUS"].Value))
			if summary == "" || properties["DTSTART"].Name == "" || status == "CANCELLED" || strings.Contains(summary, "已取消") || strings.Contains(strings.ToLower(summary), "cancelled") || strings.Contains(strings.ToLower(summary), "canceled") {
				continue
			}
			start, err := parseICalTime(properties["DTSTART"], now.Location())
			if err != nil {
				return nil, err
			}
			if start.Before(startOfDay) || !start.Before(endOfDay) {
				continue
			}
			events = append(events, Event{UID: uid, Title: summary, DueDate: start.Format("2006-01-02"), DueTime: start.Format("15:04")})
			continue
		}
		if !inEvent {
			continue
		}
		if property, ok := parseICalProperty(line); ok {
			properties[property.Name] = property
		}
	}
	return events, nil
}
