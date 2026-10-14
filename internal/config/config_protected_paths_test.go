package config

import (
	"reflect"
	"strings"
	"testing"
)

func TestProtectedPaths_TrustedPolicySurvivesPushedConfig(t *testing.T) {
	t.Parallel()
	trusted, err := LoadRepoFromBytes([]byte("protected_paths: [' *.lock ', '.github/**']\n"))
	if err != nil {
		t.Fatal(err)
	}
	for _, allow := range []bool{false, true} {
		for _, pushedYAML := range []string{"", "protected_paths: []", "protected_paths: ['other.json']"} {
			pushed, err := LoadRepoFromBytes([]byte(pushedYAML))
			if err != nil {
				t.Fatal(err)
			}
			got := Merge(DefaultGlobalConfig(), EffectiveRepoConfig(pushed, trusted, allow)).ProtectedPaths
			if !reflect.DeepEqual(got, []string{"*.lock", ".github/**"}) {
				t.Errorf("allow_repo_commands=%v, pushed=%q: protected_paths=%q", allow, pushedYAML, got)
			}
			if got := Merge(DefaultGlobalConfig(), EffectiveRepoConfig(pushed, nil, allow)).ProtectedPaths; len(got) != 0 {
				t.Errorf("pushed-only rule became active: %q", got)
			}
		}
	}
}

func TestProtectedPaths_InvalidRulesFailConfigLoading(t *testing.T) {
	t.Parallel()
	for _, value := range []string{"['']", "['  ']", "['[']", "['/**']", "not-a-list"} {
		if _, err := LoadRepoFromBytes([]byte("protected_paths: " + value)); err == nil {
			t.Errorf("accepted invalid protected_paths: %s", value)
		} else if value != "not-a-list" && !strings.Contains(err.Error(), "protected_paths") {
			t.Errorf("error does not identify the setting: %v", err)
		}
	}
}

func TestStagingPolicy_TrustedPolicySurvivesPushedConfig(t *testing.T) {
	t.Parallel()
	trusted, err := LoadRepoFromBytes([]byte("staging_exclude: [' *.bin ', 'tmp/**']\nmax_file_size_bytes: 1024\n"))
	if err != nil {
		t.Fatal(err)
	}
	for _, allow := range []bool{false, true} {
		for _, pushedYAML := range []string{"", "staging_exclude: []\nmax_file_size_bytes: 0", "staging_exclude: ['other']\nmax_file_size_bytes: 999999"} {
			pushed, err := LoadRepoFromBytes([]byte(pushedYAML))
			if err != nil {
				t.Fatal(err)
			}
			got := Merge(DefaultGlobalConfig(), EffectiveRepoConfig(pushed, trusted, allow))
			if !reflect.DeepEqual(got.StagingExclude, []string{"*.bin", "tmp/**"}) || got.MaxFileSizeBytes != 1024 {
				t.Errorf("allow_repo_commands=%v, pushed=%q: exclude=%q max=%d", allow, pushedYAML, got.StagingExclude, got.MaxFileSizeBytes)
			}
			got = Merge(DefaultGlobalConfig(), EffectiveRepoConfig(pushed, nil, allow))
			if len(got.StagingExclude) != 0 || got.MaxFileSizeBytes != DefaultMaxFileSizeBytes {
				t.Errorf("pushed-only staging policy became active: exclude=%q max=%d", got.StagingExclude, got.MaxFileSizeBytes)
			}
		}
	}
}

func TestStagingPolicy_DefaultsAndExplicitZero(t *testing.T) {
	t.Parallel()
	unset, err := LoadRepoFromBytes([]byte(""))
	if err != nil {
		t.Fatal(err)
	}
	if got := Merge(DefaultGlobalConfig(), unset).MaxFileSizeBytes; got != 10485760 {
		t.Errorf("default max_file_size_bytes = %d, want 10485760", got)
	}
	zero, err := LoadRepoFromBytes([]byte("max_file_size_bytes: 0"))
	if err != nil {
		t.Fatal(err)
	}
	if got := Merge(DefaultGlobalConfig(), zero).MaxFileSizeBytes; got != 0 {
		t.Errorf("explicit 0 = %d, want 0 (check disabled)", got)
	}
}

func TestStagingPolicy_InvalidValuesFailConfigLoading(t *testing.T) {
	t.Parallel()
	for _, doc := range []string{"staging_exclude: ['']", "staging_exclude: ['[']", "staging_exclude: ['/**']", "max_file_size_bytes: -1"} {
		_, err := LoadRepoFromBytes([]byte(doc))
		if err == nil {
			t.Errorf("accepted invalid config: %s", doc)
		} else if !strings.Contains(err.Error(), "staging_exclude") && !strings.Contains(err.Error(), "max_file_size_bytes") {
			t.Errorf("error does not identify the setting: %v", err)
		}
	}
}
