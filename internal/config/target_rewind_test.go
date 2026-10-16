package config

import "testing"

func TestTargetAdvanceRewindBudgetDefaultsAndTrust(t *testing.T) {
	for _, tc := range []struct {
		global, trusted, pushed string
		want                    int
	}{
		{"{}", "{}", "{}", 3},
		{"rebase: {max_target_advance_rewinds: 5}", "{}", "{}", 5},
		{"rebase: {max_target_advance_rewinds: 5}", "rebase: {max_target_advance_rewinds: 0}", "rebase: {max_target_advance_rewinds: 9}", 0},
		{"{}", "{}", "allow_repo_commands: true\nrebase: {max_target_advance_rewinds: 9}", 3},
	} {
		g, err := LoadGlobalFromBytes([]byte(tc.global))
		if err != nil {
			t.Fatal(err)
		}
		trusted, err := LoadRepoFromBytes([]byte(tc.trusted))
		if err != nil {
			t.Fatal(err)
		}
		pushed, err := LoadRepoFromBytes([]byte(tc.pushed))
		if err != nil {
			t.Fatal(err)
		}
		if got := Merge(g, EffectiveRepoConfig(pushed, trusted, false)).Rebase.MaxTargetAdvanceRewinds; got != tc.want {
			t.Fatalf("budget=%d want=%d", got, tc.want)
		}
	}
	for _, load := range []func([]byte) error{
		func(b []byte) error { _, err := LoadRepoFromBytes(b); return err },
		func(b []byte) error { _, err := LoadGlobalFromBytes(b); return err },
	} {
		if err := load([]byte("rebase: {max_target_advance_rewinds: -1}")); err == nil {
			t.Fatal("accepted a negative budget")
		}
	}
}
