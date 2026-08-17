package telemetry_test

import (
	"bytes"
	"encoding/json"
	"log/slog"
	"strings"
	"testing"

	"github.com/OzanAlpkaya/pulsego/internal/telemetry"
)

func TestNewLoggerEmitsJSONWithServiceAttributes(t *testing.T) {
	t.Parallel()

	var buf bytes.Buffer
	log := telemetry.NewLogger(&buf, slog.LevelInfo, "pulsego-api", "development")

	log.Info("listening", slog.String("addr", ":8080"))

	var record map[string]any
	if err := json.Unmarshal(buf.Bytes(), &record); err != nil {
		t.Fatalf("log output is not valid JSON: %v\noutput: %s", err, buf.String())
	}

	want := map[string]string{
		"msg":     "listening",
		"level":   "INFO",
		"service": "pulsego-api",
		"env":     "development",
		"addr":    ":8080",
	}
	for key, wantValue := range want {
		got, ok := record[key].(string)
		if !ok {
			t.Errorf("record is missing string field %q: %v", key, record)
			continue
		}
		if got != wantValue {
			t.Errorf("record[%q] = %q, want %q", key, got, wantValue)
		}
	}
	if _, ok := record["time"]; !ok {
		t.Errorf("record is missing a time field: %v", record)
	}
}

func TestNewLoggerRespectsLevel(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name      string
		level     slog.Level
		wantDebug bool
	}{
		{name: "info suppresses debug", level: slog.LevelInfo, wantDebug: false},
		{name: "debug emits debug", level: slog.LevelDebug, wantDebug: true},
		{name: "warn suppresses debug", level: slog.LevelWarn, wantDebug: false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			var buf bytes.Buffer
			log := telemetry.NewLogger(&buf, tt.level, "pulsego-worker", "development")

			log.Debug("poll tick")

			if gotDebug := strings.Contains(buf.String(), "poll tick"); gotDebug != tt.wantDebug {
				t.Errorf("debug record emitted = %v, want %v (output: %q)", gotDebug, tt.wantDebug, buf.String())
			}
		})
	}
}
