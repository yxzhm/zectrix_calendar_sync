package main

import (
	"errors"
	"fmt"
	"strings"
	"time"

	"golang.org/x/sys/windows/registry"
)

const registryConfigPath = `Software\ZectrixCalendarSync`

const (
	valueZectrixAPIBase          = "ZECTRIX_API_BASE"
	valueZectrixAPIKey           = "ZECTRIX_API_KEY"
	valueZectrixDeviceID         = "ZECTRIX_DEVICE_ID"
	valueExpireHours             = "EXPIRE_HOURS"
	valueCompletedRetentionHours = "COMPLETED_RETENTION_HOURS"
	valueGoogleCalendarURL       = "GOOGLE_CALENDAR_URL"
	valueWebCalProxy             = "WEBCAL_PROXY"
)

var defaultStringValues = map[string]string{
	valueZectrixAPIBase:    "https://cloud.zectrix.com/open/v1",
	valueZectrixAPIKey:     "",
	valueZectrixDeviceID:   "",
	valueGoogleCalendarURL: "",
	valueWebCalProxy:       "",
}

const (
	defaultExpireHours             uint32 = 12
	defaultCompletedRetentionHours uint32 = 24
	maxDurationHours                      = uint64((1<<63 - 1) / time.Hour)
)

var defaultIntegerValues = map[string]uint32{
	valueExpireHours:             defaultExpireHours,
	valueCompletedRetentionHours: defaultCompletedRetentionHours,
}

type Config struct {
	ZectrixAPIBase          string
	ZectrixAPIKey           string
	ZectrixDeviceID         string
	ExpireHours             int
	CompletedRetentionHours int
	GoogleCalendarURL       string
	WebCalProxy             string
	RequestTimeout          time.Duration
}

type configRegistryKey interface {
	GetStringValue(name string) (string, uint32, error)
	GetIntegerValue(name string) (uint64, uint32, error)
	SetStringValue(name, value string) error
	SetDWordValue(name string, value uint32) error
}

func loadConfig() (Config, error) {
	key, _, err := registry.CreateKey(registry.CURRENT_USER, registryConfigPath, registry.QUERY_VALUE|registry.SET_VALUE)
	if err != nil {
		return Config{}, fmt.Errorf("open registry key HKCU\\%s: %w", registryConfigPath, err)
	}
	defer key.Close()

	return loadConfigFromRegistry(key)
}

func loadConfigFromRegistry(key configRegistryKey) (Config, error) {
	values := make(map[string]string, len(defaultStringValues))
	for name, fallback := range defaultStringValues {
		value, _, err := key.GetStringValue(name)
		switch {
		case err == nil:
			values[name] = value
		case errors.Is(err, registry.ErrNotExist):
			if err := key.SetStringValue(name, fallback); err != nil {
				return Config{}, fmt.Errorf("create registry value %s: %w", name, err)
			}
			values[name] = fallback
		default:
			return Config{}, fmt.Errorf("read registry value %s: %w", name, err)
		}
	}

	integerValues := make(map[string]uint64, len(defaultIntegerValues))
	for name, fallback := range defaultIntegerValues {
		value, _, err := key.GetIntegerValue(name)
		switch {
		case err == nil:
			integerValues[name] = value
		case errors.Is(err, registry.ErrNotExist):
			if err := key.SetDWordValue(name, fallback); err != nil {
				return Config{}, fmt.Errorf("create registry value %s: %w", name, err)
			}
			integerValues[name] = uint64(fallback)
		default:
			return Config{}, fmt.Errorf("read registry value %s: %w", name, err)
		}
		if integerValues[name] > maxDurationHours {
			return Config{}, fmt.Errorf("registry value %s is too large", name)
		}
	}

	return Config{
		ZectrixAPIBase:          strings.TrimRight(values[valueZectrixAPIBase], "/"),
		ZectrixAPIKey:           values[valueZectrixAPIKey],
		ZectrixDeviceID:         values[valueZectrixDeviceID],
		ExpireHours:             int(integerValues[valueExpireHours]),
		CompletedRetentionHours: int(integerValues[valueCompletedRetentionHours]),
		GoogleCalendarURL:       strings.TrimSpace(values[valueGoogleCalendarURL]),
		WebCalProxy:             strings.TrimSpace(values[valueWebCalProxy]),
		RequestTimeout:          10 * time.Second,
	}, nil
}
