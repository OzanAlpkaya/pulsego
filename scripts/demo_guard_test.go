// Package scripts_test checks the repository's shell tooling. The directory
// holds no Go code besides this test.
package scripts_test

import (
	"bytes"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// TestDemoRefusesWithoutCallerOptIn runs scripts/demo.sh with every
// combination short of ENVIRONMENT=development ALLOW_PRIVATE_TARGETS=true and
// expects it to exit 2 before it builds or starts anything.
func TestDemoRefusesWithoutCallerOptIn(t *testing.T) {
	t.Parallel()

	bash, err := exec.LookPath("bash")
	if err != nil {
		t.Skip("bash not available")
	}

	tests := []struct {
		name string
		env  []string
	}{
		{"neither", nil},
		{"only environment", []string{"ENVIRONMENT=development"}},
		{"only allow", []string{"ALLOW_PRIVATE_TARGETS=true"}},
		{"production", []string{"ENVIRONMENT=production", "ALLOW_PRIVATE_TARGETS=true"}},
		{"allow false", []string{"ENVIRONMENT=development", "ALLOW_PRIVATE_TARGETS=false"}},
		{"allow TRUE", []string{"ENVIRONMENT=development", "ALLOW_PRIVATE_TARGETS=TRUE"}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			// A PATH without go makes any build attempt fail loudly instead of
			// silently doing work, and DATABASE_URL points nowhere.
			cmd := exec.CommandContext(t.Context(), bash, "demo.sh")
			cmd.Env = append([]string{
				"PATH=/usr/bin:/bin",
				"HOME=" + t.TempDir(),
				"DATABASE_URL=postgres://nobody@127.0.0.1:1/none?sslmode=disable",
			}, tc.env...)
			var stderr bytes.Buffer
			cmd.Stderr = &stderr
			err := cmd.Run()

			var exitErr *exec.ExitError
			if !errors.As(err, &exitErr) || exitErr.ExitCode() != 2 {
				t.Fatalf("demo.sh: err = %v, want exit status 2; stderr:\n%s", err, stderr.String())
			}
			if !strings.Contains(stderr.String(), "refusing to start") {
				t.Errorf("stderr does not explain the refusal:\n%s", stderr.String())
			}
			if strings.Contains(stderr.String(), "building binaries") {
				t.Error("demo.sh started building before refusing")
			}
		})
	}
}

// TestNoCommittedConfigEnablesPrivateTargets guards CLAUDE.md Rule 8: no
// committed configuration, Makefile, CI workflow or script turns SSRF
// protection off. The opt-in is passed by the caller on the command line.
func TestNoCommittedConfigEnablesPrivateTargets(t *testing.T) {
	t.Parallel()

	root := ".."
	files := []string{
		".env.example",
		"Makefile",
		"deployments/compose.yaml",
		"scripts/demo.sh",
		"scripts/seed-demo.sh",
	}
	workflows, err := filepath.Glob(filepath.Join(root, ".github/workflows/*.y*ml"))
	if err != nil {
		t.Fatal(err)
	}
	for _, w := range workflows {
		rel, _ := filepath.Rel(root, w)
		files = append(files, rel)
	}

	// Forms that would set the variable to true: dotenv/shell assignment or
	// export, a command prefix, a make assignment, YAML (also as a default).
	setters := []*regexp.Regexp{
		regexp.MustCompile(`(?m)^\s*(export\s+)?ALLOW_PRIVATE_TARGETS\s*(:?=|\?=|\+=)\s*["']?true["']?\s*(#.*)?$`),
		regexp.MustCompile(`(?m)^\s*export\b[^#\n]*ALLOW_PRIVATE_TARGETS=["']?true`),
		regexp.MustCompile(`(?m)ALLOW_PRIVATE_TARGETS\s*:\s*["']?(\$\{ALLOW_PRIVATE_TARGETS:-)?true`),
		regexp.MustCompile(`(?m)^\t[^#\n]*ALLOW_PRIVATE_TARGETS=true`), // Makefile recipe line
		regexp.MustCompile(`(?m)ALLOW_PRIVATE_TARGETS=["']?true["']?\s+("?\$work|"?\$\(GO\)|go\s|exec\s|\./|bin/)`),
	}
	for _, f := range files {
		data, err := os.ReadFile(filepath.Join(root, f))
		if err != nil {
			t.Errorf("read %s: %v", f, err)
			continue
		}
		for _, re := range setters {
			if m := re.Find(data); m != nil {
				t.Errorf("%s sets ALLOW_PRIVATE_TARGETS=true: %q", f, m)
			}
		}
	}

	env, err := os.ReadFile(filepath.Join(root, ".env.example"))
	if err != nil {
		t.Fatal(err)
	}
	if !regexp.MustCompile(`(?m)^ALLOW_PRIVATE_TARGETS=false$`).Match(env) {
		t.Error(".env.example must set ALLOW_PRIVATE_TARGETS=false")
	}
}
