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

func TestCheckValue(t *testing.T) {
	for _, tc := range []struct {
		key, value string
		ok         bool
	}{
		{"WARN_KICK_THRESHOLD", "5", true},
		{"WARN_KICK_THRESHOLD", "five", false},
		{"MOD_LOG", "123456789012345678", true},
		{"MOD_LOG", "#mod-log", false},
		{"MOD_ROLE", "0", false},
		{"JAIL_PROTECTED_ROLES", "123, 456", true},
		{"JAIL_PROTECTED_ROLES", "123,abc", false},
		{ModSupportMsgKey, "x", false},
		{"CURATED_EMOTE", "anything", true},
		{"SOME_OLD_KEY", "anything", true},
		{"MOD_LOG", "", true},
	} {
		if err := CheckValue(tc.key, tc.value); (err == nil) != tc.ok {
			t.Errorf("CheckValue(%q, %q) = %v, want ok=%v", tc.key, tc.value, err, tc.ok)
		}
	}
	if !Known(JailSetupKey) || !Known("MOD_LOG") || Known("SOME_OLD_KEY") {
		t.Error("Known gives the wrong result")
	}
}
