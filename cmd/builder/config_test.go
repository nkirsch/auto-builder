package main

import "testing"

func TestLoadConfigVersion(t *testing.T) {
	t.Setenv("DATABASE_URL", "postgres://x")
	t.Setenv("VERSION", "v26.10.24-9")
	cfg, err := loadConfig()
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Version != "v26.10.24-9" {
		t.Errorf("Version = %q", cfg.Version)
	}
	t.Setenv("VERSION", "unset")
	if cfg, _ := loadConfig(); cfg.Version != "" {
		t.Errorf("unset Version = %q", cfg.Version)
	}
}

func TestLoadConfigIssueSync(t *testing.T) {
	t.Setenv("DATABASE_URL", "postgres://x")
	if cfg, _ := loadConfig(); !cfg.IssueSync {
		t.Error("IssueSync off by default")
	}
	t.Setenv("ISSUE_SYNC", "off")
	if cfg, _ := loadConfig(); cfg.IssueSync {
		t.Error("ISSUE_SYNC=off left IssueSync on")
	}
}

func TestLoadConfigFeedbackURL(t *testing.T) {
	t.Setenv("DATABASE_URL", "postgres://x")
	if cfg, _ := loadConfig(); cfg.FeedbackURL != "https://builder.extreme.org/items/new" {
		t.Errorf("default FeedbackURL = %q", cfg.FeedbackURL)
	}
	t.Setenv("FEEDBACK_URL", "off")
	if cfg, _ := loadConfig(); cfg.FeedbackURL != "" {
		t.Errorf("FEEDBACK_URL=off left %q", cfg.FeedbackURL)
	}
}
