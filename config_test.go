package main

import (
	"fmt"
	"testing"
	"time"

	"golang.org/x/sys/windows/registry"
)

type fakeRegistryKey struct {
	strings  map[string]string
	integers map[string]uint64
}

func newFakeRegistryKey() *fakeRegistryKey {
	return &fakeRegistryKey{
		strings:  make(map[string]string),
		integers: make(map[string]uint64),
	}
}

func (k *fakeRegistryKey) GetStringValue(name string) (string, uint32, error) {
	value, ok := k.strings[name]
	if !ok {
		return "", 0, registry.ErrNotExist
	}
	return value, registry.SZ, nil
}

func (k *fakeRegistryKey) GetIntegerValue(name string) (uint64, uint32, error) {
	value, ok := k.integers[name]
	if !ok {
		return 0, 0, registry.ErrNotExist
	}
	return value, registry.DWORD, nil
}

func (k *fakeRegistryKey) SetStringValue(name, value string) error {
	k.strings[name] = value
	return nil
}

func (k *fakeRegistryKey) SetDWordValue(name string, value uint32) error {
	k.integers[name] = uint64(value)
	return nil
}

func TestLoadConfigFromRegistryCreatesDefaults(t *testing.T) {
	key := newFakeRegistryKey()

	cfg, err := loadConfigFromRegistry(key)
	if err != nil {
		t.Fatal(err)
	}

	if len(key.strings) != len(defaultStringValues) {
		t.Fatalf("created %d string values, want %d", len(key.strings), len(defaultStringValues))
	}
	for name, want := range defaultStringValues {
		if got := key.strings[name]; got != want {
			t.Errorf("%s = %q, want %q", name, got, want)
		}
	}
	if got := key.integers[valueExpireHours]; got != uint64(defaultExpireHours) {
		t.Errorf("%s = %d, want %d", valueExpireHours, got, defaultExpireHours)
	}
	if cfg.ZectrixAPIBase != defaultStringValues[valueZectrixAPIBase] || cfg.ExpireHours != int(defaultExpireHours) {
		t.Fatalf("unexpected defaults: %#v", cfg)
	}
	if cfg.RequestTimeout != 10*time.Second {
		t.Fatalf("RequestTimeout = %s, want 10s", cfg.RequestTimeout)
	}
}

func TestLoadConfigFromRegistryUsesExistingValues(t *testing.T) {
	key := newFakeRegistryKey()
	for name, value := range defaultStringValues {
		key.strings[name] = value
	}
	key.strings[valueZectrixAPIBase] = "https://example.test/api/"
	key.strings[valueZectrixAPIKey] = "secret"
	key.strings[valueGoogleCalendarURL] = "  webcal://calendar.google.com/calendar/ical/example/basic.ics  "
	key.strings[valueWebCalProxy] = "  http://proxy.test:8080  "
	key.integers[valueExpireHours] = 24

	cfg, err := loadConfigFromRegistry(key)
	if err != nil {
		t.Fatal(err)
	}

	if cfg.ZectrixAPIBase != "https://example.test/api" || cfg.ZectrixAPIKey != "secret" {
		t.Fatalf("unexpected string config: %#v", cfg)
	}
	if cfg.GoogleCalendarURL != "webcal://calendar.google.com/calendar/ical/example/basic.ics" || cfg.WebCalProxy != "http://proxy.test:8080" || cfg.ExpireHours != 24 {
		t.Fatalf("unexpected proxy/expiry config: %#v", cfg)
	}
}

func TestLoadConfigFromRegistryReportsReadError(t *testing.T) {
	key := &failingRegistryKey{fakeRegistryKey: newFakeRegistryKey()}
	_, err := loadConfigFromRegistry(key)
	if err == nil {
		t.Fatal("expected registry read error")
	}
}

type failingRegistryKey struct {
	*fakeRegistryKey
}

func (k *failingRegistryKey) GetStringValue(name string) (string, uint32, error) {
	return "", 0, fmt.Errorf("access denied")
}
