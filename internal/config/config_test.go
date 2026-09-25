package config

import (
	"slices"
	"testing"
)

func TestParseRoleList(t *testing.T) {
	got := ParseRoleList(" 123, 456 abc\n789,, ")
	want := []string{"123", "456", "789"}
	if !slices.Equal(got, want) {
		t.Fatalf("ParseRoleList = %v, want %v", got, want)
	}
	if got := ParseRoleList(""); len(got) != 0 {
		t.Fatalf("ParseRoleList(\"\") = %v, want empty", got)
	}
}

func TestStoreFallbacks(t *testing.T) {
	t.Setenv("MOD_ROLE_ID", "")
	t.Setenv("CURATED_THRESHOLD", "")
	s := NewStore(nil)

	if got := s.Int("CURATED_THRESHOLD"); got != 7 {
		t.Errorf("default Int = %d, want 7", got)
	}
	if got := s.Source("CURATED_THRESHOLD"); got != "default" {
		t.Errorf("Source = %q, want default", got)
	}

	t.Setenv("CURATED_THRESHOLD", "9")
	if got, src := s.Int("CURATED_THRESHOLD"), s.Source("CURATED_THRESHOLD"); got != 9 || src != ".env" {
		t.Errorf("env Int = %d (%s), want 9 (.env)", got, src)
	}

	s.Set("CURATED_THRESHOLD", "11")
	if got, src := s.Int("CURATED_THRESHOLD"), s.Source("CURATED_THRESHOLD"); got != 11 || src != "database" {
		t.Errorf("db Int = %d (%s), want 11 (database)", got, src)
	}

	s.Set("CURATED_THRESHOLD", "not a number")
	if got := s.Int("CURATED_THRESHOLD"); got != 7 {
		t.Errorf("bad Int = %d, want default 7", got)
	}

	if got := s.ID("MOD_ROLE"); got != "" {
		t.Errorf("unset ID = %q, want empty", got)
	}
	s.Set("MOD_ROLE", "<@&123>")
	if got := s.ID("MOD_ROLE"); got != "" {
		t.Errorf("non-numeric ID = %q, want empty", got)
	}
	s.Set("MOD_ROLE", "1234567890")
	if got := s.ID("MOD_ROLE"); got != "1234567890" {
		t.Errorf("ID = %q, want 1234567890", got)
	}
}
