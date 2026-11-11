package backend

import (
	"context"
	"errors"
	"testing"

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
