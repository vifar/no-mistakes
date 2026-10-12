package main

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

func TestMakeBuildPrioritizesDotEnvUmamiWebsiteID(t *testing.T) {
	skipMakeBuildTestsOnWindows(t)

	makePath, err := exec.LookPath("make")
	if err != nil {
		t.Skip("make not available")
	}

	workDir := writeTestMakeWorkspace(t)
	if err := os.WriteFile(filepath.Join(workDir, ".env"), []byte("NO_MISTAKES_UMAMI_WEBSITE_ID=website-from-dotenv\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	output := runMakeDryBuild(t, makePath, workDir, map[string]string{
		"UMAMI_WEBSITE_ID": "website-from-env",
	})

	if !strings.Contains(output, "TelemetryWebsiteID=website-from-dotenv") {
		t.Fatalf("make build output should embed .env website id, got:\n%s", output)
	}
	if strings.Contains(output, "TelemetryWebsiteID=website-from-env") {
		t.Fatalf("make build output should not prefer env website id when .env exists, got:\n%s", output)
	}
}

func TestMakeBuildUsesForkReleaseVersionAndHonorsOverrides(t *testing.T) {
	skipMakeBuildTestsOnWindows(t)
	makePath, err := exec.LookPath("make")
	if err != nil {
		t.Skip("make not available")
	}
	gitPath, err := exec.LookPath("git")
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv("GIT_CONFIG_COUNT", "0")
	t.Setenv("GIT_CONFIG_NOSYSTEM", "1")
	t.Setenv("GIT_CONFIG_GLOBAL", filepath.Join(t.TempDir(), "no-global-config"))
	for _, tt := range []struct {
		name  string
		env   map[string]string
		args  []string
		want  string
		dirty bool
	}{
		{name: "fork manifest", want: "v7.4.2"},
		{name: "dirty fork source", want: "v7.4.2", dirty: true},
		{name: "environment override", env: map[string]string{"VERSION": "v8.0.0-beta.1"}, want: "v8.0.0-beta.1"},
		{name: "release command override", args: []string{"VERSION=v8.0.0"}, want: "v8.0.0"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			workDir := writeTestMakeWorkspace(t)
			if err := os.WriteFile(filepath.Join(workDir, ".release-please-manifest.json"), []byte(`{".": "7.4.2"}`), 0o644); err != nil {
				t.Fatal(err)
			}
			for _, args := range [][]string{
				{"init", "--quiet"},
				{"add", "Makefile", ".release-please-manifest.json"},
				{"-c", "user.name=Test", "-c", "user.email=test@example.com", "commit", "--quiet", "-m", "imported upstream release"},
				{"tag", "v99.0.0"},
			} {
				cmd := exec.Command(gitPath, args...)
				cmd.Dir = workDir
				if output, err := cmd.CombinedOutput(); err != nil {
					t.Fatalf("git %v: %v\n%s", args, err, output)
				}
			}
			cmd := exec.Command(gitPath, "rev-parse", "--short", "HEAD")
			cmd.Dir = workDir
			commit, err := cmd.Output()
			if err != nil {
				t.Fatal(err)
			}
			if tt.dirty {
				file := filepath.Join(workDir, "Makefile")
				data, err := os.ReadFile(file)
				if err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(file, append(data, []byte("\n# tracked source edit\n")...), 0o644); err != nil {
					t.Fatal(err)
				}
			}
			version := tt.want
			if len(tt.env) == 0 && len(tt.args) == 0 {
				version += "+g" + strings.TrimSpace(string(commit))
				if tt.dirty {
					version += ".dirty"
				}
			}
			output := runMakeDryBuild(t, makePath, workDir, tt.env, tt.args...)
			want := "internal/buildinfo.Version=" + version + " "
			if !strings.Contains(output, want) {
				t.Fatalf("make build must embed %s, got:\n%s", version, output)
			}
		})
	}
}

func TestMakeBuildUsesEnvUmamiWebsiteIDWhenDotEnvMissing(t *testing.T) {
	skipMakeBuildTestsOnWindows(t)

	makePath, err := exec.LookPath("make")
	if err != nil {
		t.Skip("make not available")
	}

	workDir := writeTestMakeWorkspace(t)
	output := runMakeDryBuild(t, makePath, workDir, map[string]string{
		"UMAMI_WEBSITE_ID": "website-from-env",
	})

	if !strings.Contains(output, "TelemetryWebsiteID=website-from-env") {
		t.Fatalf("make build output should embed env website id when .env is absent, got:\n%s", output)
	}
}

func TestMakeBuildRejectsMetadataInForkVersionBaseline(t *testing.T) {
	skipMakeBuildTestsOnWindows(t)
	makePath, err := exec.LookPath("make")
	if err != nil {
		t.Skip("make not available")
	}
	workDir := writeTestMakeWorkspace(t)
	if err := os.WriteFile(filepath.Join(workDir, ".release-please-manifest.json"), []byte(`{".": "7.4.2+fork"}`), 0o644); err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command(makePath, "-n", "build")
	cmd.Dir = workDir
	cmd.Env = filteredEnv(os.Environ(), "VERSION")
	output, err := cmd.CombinedOutput()
	if err == nil || !strings.Contains(string(output), "Cannot read the fork release version") {
		t.Fatalf("make must reject metadata in the canonical baseline: err=%v\n%s", err, output)
	}
	output = []byte(runMakeDryBuild(t, makePath, workDir, nil, "VERSION=v8.0.0+release"))
	if !strings.Contains(string(output), "internal/buildinfo.Version=v8.0.0+release ") {
		t.Fatalf("explicit release VERSION must remain available: %s", output)
	}
}

func TestMakeBuildArchiveDoesNotInheritParentGitProvenance(t *testing.T) {
	skipMakeBuildTestsOnWindows(t)
	makePath, err := exec.LookPath("make")
	if err != nil {
		t.Skip("make not available")
	}
	gitPath, err := exec.LookPath("git")
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv("GIT_CONFIG_COUNT", "0")
	t.Setenv("GIT_CONFIG_NOSYSTEM", "1")
	t.Setenv("GIT_CONFIG_GLOBAL", filepath.Join(t.TempDir(), "no-global-config"))
	parent := t.TempDir()
	for _, args := range [][]string{
		{"init", "--quiet"},
		{"-c", "user.name=Test", "-c", "user.email=test@example.com", "commit", "--quiet", "--allow-empty", "-m", "unrelated checkout"},
	} {
		cmd := exec.Command(gitPath, args...)
		cmd.Dir = parent
		if output, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, output)
		}
	}
	workDir := filepath.Join(parent, "source-archive")
	if err := os.Mkdir(workDir, 0o755); err != nil {
		t.Fatal(err)
	}
	for _, file := range []string{"Makefile", ".release-please-manifest.json"} {
		data, err := os.ReadFile(file)
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(workDir, file), data, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(workDir, ".release-please-manifest.json"), []byte(`{".": "7.4.2"}`), 0o644); err != nil {
		t.Fatal(err)
	}
	output := runMakeDryBuild(t, makePath, workDir, nil)
	if !strings.Contains(output, "internal/buildinfo.Version=v7.4.2 ") || !strings.Contains(output, "internal/buildinfo.Commit=unknown ") {
		t.Fatalf("source archive must not inherit the unrelated parent commit:\n%s", output)
	}
}

func TestMakeBuildEmbedsDefaultSelfHostedTelemetryConfig(t *testing.T) {
	skipMakeBuildTestsOnWindows(t)

	makePath, err := exec.LookPath("make")
	if err != nil {
		t.Skip("make not available")
	}

	workDir := writeTestMakeWorkspace(t)
	output := runMakeDryBuild(t, makePath, workDir, nil)

	if !strings.Contains(output, "TelemetryHost=https://a.kunchenguid.com") {
		t.Fatalf("make build output should embed default telemetry host, got:\n%s", output)
	}
	if !strings.Contains(output, "TelemetryWebsiteID=f959e889-92f5-4121-8a1f-571b10861198") {
		t.Fatalf("make build output should embed default telemetry website id, got:\n%s", output)
	}
}

func TestMakeBuildPrioritizesDotEnvUmamiHost(t *testing.T) {
	skipMakeBuildTestsOnWindows(t)

	makePath, err := exec.LookPath("make")
	if err != nil {
		t.Skip("make not available")
	}

	workDir := writeTestMakeWorkspace(t)
	if err := os.WriteFile(filepath.Join(workDir, ".env"), []byte("NO_MISTAKES_UMAMI_HOST=https://dotenv.example\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	output := runMakeDryBuild(t, makePath, workDir, map[string]string{
		"UMAMI_HOST": "https://env.example",
	})

	if !strings.Contains(output, "TelemetryHost=https://dotenv.example") {
		t.Fatalf("make build output should embed .env telemetry host, got:\n%s", output)
	}
	if strings.Contains(output, "TelemetryHost=https://env.example") {
		t.Fatalf("make build output should not prefer env telemetry host when .env exists, got:\n%s", output)
	}
}

func TestMakeBuildIgnoresUnrelatedDotEnvEntries(t *testing.T) {
	skipMakeBuildTestsOnWindows(t)

	makePath, err := exec.LookPath("make")
	if err != nil {
		t.Skip("make not available")
	}

	workDir := writeTestMakeWorkspace(t)
	if err := os.WriteFile(filepath.Join(workDir, ".env"), []byte("VERSION=from-dotenv\nNO_MISTAKES_UMAMI_WEBSITE_ID=website-from-dotenv\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	output := runMakeDryBuild(t, makePath, workDir, nil)

	if !strings.Contains(output, "TelemetryWebsiteID=website-from-dotenv") {
		t.Fatalf("make build output should still embed dotenv website id, got:\n%s", output)
	}
	if strings.Contains(output, "/internal/buildinfo.Version=from-dotenv") {
		t.Fatalf("make build should ignore unrelated dotenv entries, got:\n%s", output)
	}
}

func TestMakeBuildStripsInlineCommentsFromDotEnvUmamiWebsiteID(t *testing.T) {
	skipMakeBuildTestsOnWindows(t)

	makePath, err := exec.LookPath("make")
	if err != nil {
		t.Skip("make not available")
	}

	workDir := writeTestMakeWorkspace(t)
	if err := os.WriteFile(filepath.Join(workDir, ".env"), []byte("NO_MISTAKES_UMAMI_WEBSITE_ID=website-from-dotenv # dev\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	output := runMakeDryBuild(t, makePath, workDir, nil)

	if !strings.Contains(output, "TelemetryWebsiteID=website-from-dotenv") {
		t.Fatalf("make build output should strip inline comments from dotenv website id, got:\n%s", output)
	}
	if strings.Contains(output, "TelemetryWebsiteID=website-from-dotenv # dev") {
		t.Fatalf("make build output should not embed inline comments in website id, got:\n%s", output)
	}
}

func TestMakeBuildPreservesQuotedHashInDotEnvUmamiWebsiteID(t *testing.T) {
	skipMakeBuildTestsOnWindows(t)

	makePath, err := exec.LookPath("make")
	if err != nil {
		t.Skip("make not available")
	}

	workDir := writeTestMakeWorkspace(t)
	if err := os.WriteFile(filepath.Join(workDir, ".env"), []byte("NO_MISTAKES_UMAMI_WEBSITE_ID=\"website # dev\"\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	output := runMakeDryBuild(t, makePath, workDir, nil)

	if !strings.Contains(output, "TelemetryWebsiteID=website # dev") {
		t.Fatalf("make build output should preserve quoted hashes in dotenv website id, got:\n%s", output)
	}
	if strings.Contains(output, "TelemetryWebsiteID=\"website") {
		t.Fatalf("make build output should not truncate quoted dotenv website id, got:\n%s", output)
	}
}

func skipMakeBuildTestsOnWindows(t *testing.T) {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("make build tests are POSIX-oriented")
	}
}

func writeTestMakeWorkspace(t *testing.T) string {
	t.Helper()

	data, err := os.ReadFile("Makefile")
	if err != nil {
		t.Fatal(err)
	}
	workDir := t.TempDir()
	if err := os.WriteFile(filepath.Join(workDir, "Makefile"), data, 0o644); err != nil {
		t.Fatal(err)
	}
	manifest, err := os.ReadFile(".release-please-manifest.json")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(workDir, ".release-please-manifest.json"), manifest, 0o644); err != nil {
		t.Fatal(err)
	}
	return workDir
}

func runMakeDryBuild(t *testing.T, makePath, workDir string, extraEnv map[string]string, makeArgs ...string) string {
	t.Helper()

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	cmd := exec.CommandContext(ctx, makePath, append([]string{"-n", "build"}, makeArgs...)...)
	cmd.Dir = workDir
	cmd.Env = filteredEnv(os.Environ(), "VERSION", "UMAMI_HOST", "UMAMI_WEBSITE_ID", "NO_MISTAKES_UMAMI_HOST", "NO_MISTAKES_UMAMI_WEBSITE_ID")
	for key, value := range extraEnv {
		cmd.Env = append(cmd.Env, key+"="+value)
	}
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("make -n build failed: %v\n%s", err, out)
	}
	return string(out)
}
