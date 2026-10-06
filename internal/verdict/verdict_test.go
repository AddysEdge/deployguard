package verdict

import "testing"

func TestWorst(t *testing.T) {
	tests := []struct {
		name string
		in   []Outcome
		want Outcome
	}{
		{"empty is pass", nil, Pass},
		{"skipped ignored", []Outcome{Skipped, Pass}, Pass},
		{"only skipped", []Outcome{Skipped}, Pass},
		{"warn over pass", []Outcome{Pass, Warn}, Warn},
		{"inconclusive over warn", []Outcome{Warn, Inconclusive, Pass}, Inconclusive},
		{"fail over inconclusive", []Outcome{Inconclusive, Fail, Warn}, Fail},
		{"fail regardless of order", []Outcome{Fail, Inconclusive}, Fail},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := Worst(tt.in...); got != tt.want {
				t.Fatalf("Worst(%v) = %s, want %s", tt.in, got, tt.want)
			}
		})
	}
}

func TestExitCode(t *testing.T) {
	cases := map[Outcome]int{
		Pass:         0,
		Warn:         0,
		Fail:         1,
		Inconclusive: 2,
		Skipped:      3,
		Outcome("?"): 3,
	}
	for o, want := range cases {
		if got := ExitCode(o); got != want {
			t.Errorf("ExitCode(%s) = %d, want %d", o, got, want)
		}
	}
}
