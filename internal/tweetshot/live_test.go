//go:build live

package tweetshot

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestLiveRender gets real posts from X and saves their images:
//
//	TWEETSHOT_IDS=123,456 TWEETSHOT_OUT=/tmp go test -tags live -run TestLiveRender ./internal/tweetshot/
func TestLiveRender(t *testing.T) {
	ids := os.Getenv("TWEETSHOT_IDS")
	if ids == "" {
		t.Skip("TWEETSHOT_IDS is not set")
	}
	out := os.Getenv("TWEETSHOT_OUT")
	if out == "" {
		out = t.TempDir()
	}
	c := NewClient()
	for _, id := range strings.Split(ids, ",") {
		p, err := c.Fetch(context.Background(), id)
		if err != nil {
			t.Errorf("%s: %v", id, err)
			continue
		}
		data, err := PNG(p)
		if err != nil {
			t.Fatalf("%s: %v", id, err)
		}
		path := filepath.Join(out, id+".png")
		if err := os.WriteFile(path, data, 0o644); err != nil {
			t.Fatal(err)
		}
		t.Logf("%s: %d bytes → %s", id, len(data), path)
	}
}
