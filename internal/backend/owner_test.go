package backend

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/charliek/shed/internal/config"
)

func TestCheckOwner(t *testing.T) {
	ctx := context.Background()
	if err := CheckOwner(ctx, "box", ""); err != nil {
		t.Errorf("no required owner, admin shed = %v", err)
	}
	if err := CheckOwner(ctx, "box", "nocturn"); err != nil {
		t.Errorf("no required owner, owned shed = %v", err)
	}
	req := WithRequiredOwner(ctx, "nocturn")
	if err := CheckOwner(req, "nocturn-a", "nocturn"); err != nil {
		t.Errorf("own shed = %v", err)
	}
	for _, owner := range []string{"", "other"} {
		if err := CheckOwner(req, "nocturn-a", owner); !errors.Is(err, config.ErrShedNotFoundSentinel) {
			t.Errorf("shed owned by %q = %v, want not found", owner, err)
		}
	}
}

func TestCheckCreatedAt(t *testing.T) {
	ctx := context.Background()
	born := time.Date(2026, 11, 1, 9, 0, 0, 0, time.UTC)
	if err := CheckCreatedAt(ctx, "box", born); err != nil {
		t.Errorf("no required creation time = %v", err)
	}
	req := WithRequiredCreatedAt(ctx, born)
	// Equal, not ==: the listing's time and a reload of the metadata may
	// differ in location or monotonic reading.
	if err := CheckCreatedAt(req, "nocturn-a", born.In(time.FixedZone("x", 3600))); err != nil {
		t.Errorf("same incarnation = %v", err)
	}
	if err := CheckCreatedAt(req, "nocturn-a", born.Add(time.Hour)); !errors.Is(err, config.ErrShedNotFoundSentinel) {
		t.Errorf("recreated shed = %v, want not found", err)
	}
}
