package eval

import (
	"context"
	"testing"
)

// TestSeededSuite runs every seeded case through the real engine against
// real local servers and requires each to match its ground truth.
func TestSeededSuite(t *testing.T) {
	if testing.Short() {
		t.Skip("seeded suite takes several seconds; skipped with -short")
	}
	res, err := Run(context.Background(), "../..", Cases, nil)
	if err != nil {
		t.Fatal(err)
	}
	for _, c := range res.Cases {
		if !c.Match {
			t.Errorf("%s (%s): expected %s, observed %s; error=%q reasons=%v", c.ID, c.Title, c.Expected, c.Observed, c.Error, c.Reasons)
		}
		if c.Classification == "FP" || c.Classification == "FN" {
			t.Errorf("%s classified %s", c.ID, c.Classification)
		}
	}
	for _, id := range []string{"R5", "N1"} {
		for _, c := range res.Cases {
			if c.ID == id && len(c.Performance) != 2 {
				t.Errorf("%s should record two measured rounds, got %d", id, len(c.Performance))
			}
		}
	}
}
