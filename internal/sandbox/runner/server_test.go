package runner

import (
	"log/slog"
	"testing"
	"time"
)

func TestConfig_Validate_RejectsMissingFields(t *testing.T) {
	t.Parallel()
	cases := []Config{
		{Logger: slog.New(slog.DiscardHandler), Image: "img"},
		{Addr: ":9090", Image: "img"},
		{Addr: ":9090", Logger: slog.New(slog.DiscardHandler)},
	}
	for i, cfg := range cases {
		if err := cfg.validate(); err == nil {
			t.Errorf("case %d: validate() error = nil, want an error for an incomplete config", i)
		}
	}
}

func TestConfig_Validate_AcceptsCompleteConfig(t *testing.T) {
	t.Parallel()
	cfg := Config{Addr: ":9090", Logger: slog.New(slog.DiscardHandler), Image: "img"}
	if err := cfg.validate(); err != nil {
		t.Errorf("validate() error = %v, want nil for a complete config", err)
	}
}

func TestConfig_SetDefaults_FillsUnsetDurations(t *testing.T) {
	t.Parallel()
	cfg := Config{}
	cfg.setDefaults()

	if cfg.MaxLifetime != 30*time.Minute {
		t.Errorf("MaxLifetime = %v, want 30m", cfg.MaxLifetime)
	}
	if cfg.ExecTimeout != 300*time.Second {
		t.Errorf("ExecTimeout = %v, want 300s", cfg.ExecTimeout)
	}
	if cfg.PreviewTTL != 2*time.Hour {
		t.Errorf("PreviewTTL = %v, want 2h", cfg.PreviewTTL)
	}
}

func TestConfig_SetDefaults_PreservesExplicitValues(t *testing.T) {
	t.Parallel()
	cfg := Config{MaxLifetime: time.Minute, ExecTimeout: time.Second, PreviewTTL: time.Hour}
	cfg.setDefaults()

	if cfg.MaxLifetime != time.Minute || cfg.ExecTimeout != time.Second || cfg.PreviewTTL != time.Hour {
		t.Errorf("setDefaults() overwrote explicit values: %+v", cfg)
	}
}

func TestServer_TrackContainer_UntrackContainer(t *testing.T) {
	t.Parallel()
	s := &Server{containers: make(map[string]time.Time), previews: make(map[string]previewInfo)}

	s.trackContainer("c1")
	if _, ok := s.containers["c1"]; !ok {
		t.Fatal("trackContainer did not record the container")
	}
	s.untrackContainer("c1")
	if _, ok := s.containers["c1"]; ok {
		t.Error("untrackContainer left the container tracked")
	}
}

func TestServer_TrackPreview_LookupPreview_UntrackPreview(t *testing.T) {
	t.Parallel()
	s := &Server{containers: make(map[string]time.Time), previews: make(map[string]previewInfo)}

	if _, ok := s.lookupPreview("job1"); ok {
		t.Fatal("lookupPreview found a preview before one was tracked")
	}

	s.trackPreview("job1", "c1")
	containerID, ok := s.lookupPreview("job1")
	if !ok || containerID != "c1" {
		t.Errorf("lookupPreview() = (%q, %v), want (c1, true)", containerID, ok)
	}

	s.untrackPreview("job1")
	if _, ok := s.lookupPreview("job1"); ok {
		t.Error("untrackPreview left the preview tracked")
	}
}
