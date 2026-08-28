package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/url"
	"strings"
	"time"
)

const calendarPrefix = "[日历]"

type Todo struct {
	ID          json.RawMessage `json:"id"`
	Title       string          `json:"title"`
	Description string          `json:"description"`
	DueDate     string          `json:"dueDate"`
	DueTime     string          `json:"dueTime"`
	Status      int             `json:"status"`
}

func (t Todo) idString() string { return strings.Trim(string(t.ID), `"`) }

type apiResponse struct {
	Code int             `json:"code"`
	Msg  string          `json:"msg"`
	Data json.RawMessage `json:"data"`
}

func newZectrixHTTPClient(cfg Config) *http.Client {
	transport := http.DefaultTransport.(*http.Transport).Clone()
	// WEBCAL_PROXY is deliberately not applied to Zectrix writes. Standard
	// HTTP(S)_PROXY variables continue to follow Go's normal process policy.
	return &http.Client{Transport: transport, Timeout: cfg.RequestTimeout}
}

func (s *CalendarSyncer) zectrixRequest(ctx context.Context, method, path string, body any) (apiResponse, error) {
	var payload io.Reader
	if body != nil {
		encoded, err := json.Marshal(body)
		if err != nil {
			return apiResponse{}, err
		}
		payload = bytes.NewReader(encoded)
	}
	req, err := http.NewRequestWithContext(ctx, method, s.cfg.ZectrixAPIBase+path, payload)
	if err != nil {
		return apiResponse{}, err
	}
	req.Header.Set("X-API-Key", s.cfg.ZectrixAPIKey)
	req.Header.Set("Content-Type", "application/json")
	resp, err := s.zectrixClient.Do(req)
	if err != nil {
		return apiResponse{}, err
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		preview, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		return apiResponse{}, fmt.Errorf("HTTP %s: %s", resp.Status, strings.TrimSpace(string(preview)))
	}
	var result apiResponse
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return apiResponse{}, err
	}
	if result.Code != 0 {
		return apiResponse{}, fmt.Errorf("API code %d: %s", result.Code, result.Msg)
	}
	return result, nil
}

func (s *CalendarSyncer) getExistingTodos(ctx context.Context) error {
	return s.retry("get todos", func() error {
		query := url.Values{"status": {"0"}, "deviceId": {s.cfg.ZectrixDeviceID}}
		result, err := s.zectrixRequest(ctx, http.MethodGet, "/todos?"+query.Encode(), nil)
		if err != nil {
			return err
		}
		var todos []Todo
		if len(result.Data) > 0 && string(result.Data) != "null" {
			if err := json.Unmarshal(result.Data, &todos); err != nil {
				return fmt.Errorf("decode todos: %w", err)
			}
		}
		s.existingTodos = todos
		s.uidMap = make(map[string]Todo, len(todos))
		for _, todo := range todos {
			if uid := extractUID(todo.Description); uid != "" {
				s.uidMap[uid] = todo
			}
		}
		log.Printf("found %d existing todos", len(todos))
		return nil
	})
}

func (s *CalendarSyncer) activeCalendarTodos() []Todo {
	result := make([]Todo, 0)
	for _, todo := range s.existingTodos {
		if todo.Status == 0 && strings.HasPrefix(todo.Title, calendarPrefix) {
			result = append(result, todo)
		}
	}
	return result
}

func (s *CalendarSyncer) isExpired(todo Todo) bool {
	due, err := time.ParseInLocation("2006-01-02 15:04", todo.DueDate+" "+todo.DueTime, s.now().Location())
	if err != nil {
		log.Printf("cannot parse due time %q %q: %v", todo.DueDate, todo.DueTime, err)
		return false
	}
	return s.now().Sub(due) >= time.Duration(s.cfg.ExpireHours)*time.Hour
}

func (s *CalendarSyncer) completeExpiredTodos(ctx context.Context) error {
	var failures []error
	count := 0
	for _, todo := range s.activeCalendarTodos() {
		if !s.isExpired(todo) {
			continue
		}
		if err := s.completeTodo(ctx, todo); err != nil {
			failures = append(failures, err)
			continue
		}
		count++
	}
	log.Printf("completed %d expired calendar todos", count)
	return errors.Join(failures...)
}

func (s *CalendarSyncer) completeTodo(ctx context.Context, todo Todo) error {
	if s.dryRun {
		log.Printf("[DRY RUN] would complete todo id=%s", todo.idString())
		return nil
	}
	return s.retry("complete todo "+todo.idString(), func() error {
		_, err := s.zectrixRequest(ctx, http.MethodPut, "/todos/"+url.PathEscape(todo.idString())+"/complete", nil)
		return err
	})
}

func (s *CalendarSyncer) deleteTodo(ctx context.Context, todo Todo) error {
	if s.dryRun {
		log.Printf("[DRY RUN] would delete todo id=%s", todo.idString())
		return nil
	}
	return s.retry("delete todo "+todo.idString(), func() error {
		_, err := s.zectrixRequest(ctx, http.MethodDelete, "/todos/"+url.PathEscape(todo.idString()), nil)
		return err
	})
}

func buildDescription(uid string) string {
	description := "从邮箱日历同步"
	if uid != "" {
		description += "\nUID: " + uid
	}
	return description
}

func extractUID(description string) string {
	for _, line := range strings.Split(description, "\n") {
		line = strings.TrimSpace(line)
		if strings.HasPrefix(line, "UID:") {
			return strings.TrimSpace(strings.TrimPrefix(line, "UID:"))
		}
	}
	return ""
}

func todoPayload(event Event, includeDevice bool, deviceID string) map[string]any {
	payload := map[string]any{
		"title":       calendarPrefix + " " + strings.TrimSpace(event.Title),
		"description": buildDescription(event.UID),
		"dueDate":     event.DueDate,
		"dueTime":     event.DueTime,
	}
	if includeDevice {
		payload["repeatType"] = "none"
		payload["priority"] = 1
		payload["deviceId"] = deviceID
	}
	return payload
}

func (s *CalendarSyncer) createTodo(ctx context.Context, event Event) error {
	if s.dryRun {
		log.Printf("[DRY RUN] would create event %q at %s %s", event.Title, event.DueDate, event.DueTime)
		return nil
	}
	return s.retry("create event "+event.UID, func() error {
		_, err := s.zectrixRequest(ctx, http.MethodPost, "/todos", todoPayload(event, true, s.cfg.ZectrixDeviceID))
		return err
	})
}

func (s *CalendarSyncer) updateTodo(ctx context.Context, todo Todo, event Event) error {
	if s.dryRun {
		log.Printf("[DRY RUN] would update todo id=%s from event %q", todo.idString(), event.Title)
		return nil
	}
	return s.retry("update todo "+todo.idString(), func() error {
		_, err := s.zectrixRequest(ctx, http.MethodPut, "/todos/"+url.PathEscape(todo.idString()), todoPayload(event, false, ""))
		return err
	})
}

func (s *CalendarSyncer) syncEvents(ctx context.Context, events []Event) error {
	currentUIDs := make(map[string]struct{}, len(events))
	created, updated, deleted := 0, 0, 0
	var failures []error
	for _, event := range events {
		if event.UID == "" {
			continue
		}
		currentUIDs[event.UID] = struct{}{}
		existing, found := s.uidMap[event.UID]
		if !found {
			if err := s.createTodo(ctx, event); err != nil {
				failures = append(failures, err)
			} else {
				created++
			}
			continue
		}
		existingTitle := strings.TrimSpace(strings.TrimPrefix(existing.Title, calendarPrefix))
		if existingTitle != event.Title || existing.DueDate != event.DueDate || existing.DueTime != event.DueTime {
			if err := s.updateTodo(ctx, existing, event); err != nil {
				failures = append(failures, err)
			} else {
				updated++
			}
		}
	}
	for _, todo := range s.activeCalendarTodos() {
		uid := extractUID(todo.Description)
		if uid == "" {
			continue
		}
		if _, exists := currentUIDs[uid]; exists {
			continue
		}
		if err := s.deleteTodo(ctx, todo); err != nil {
			failures = append(failures, err)
		} else {
			deleted++
		}
	}
	log.Printf("sync complete: created=%d updated=%d deleted=%d", created, updated, deleted)
	return errors.Join(failures...)
}
