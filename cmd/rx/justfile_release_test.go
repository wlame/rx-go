package main

// The release path hands a git tag name to the justfile twice:
// release.yml passes the pushed tag's version to `just release-notes`,
// and every recipe that stamps a build reads `git describe`, which
// prints the nearest tag. git accepts a tag name that holds a quote, a
// `;`, a `$(` or a backtick, so a recipe that let the shell read the
// name as code would run whatever the tag says, in a job that may write
// releases. These tests run the recipes on such names, in a copy of the
// justfile in a scratch directory, and check that no command spelled in
// a name runs.

import (
	"bytes"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// markerName is the file each crafted name tries to create with
// `touch`. The recipes run in the directory of the justfile, so a
// command that ran leaves the marker there.
const markerName = "marker"

// stubGo stands in for `go build` in the build recipes: it writes the
// value of -ldflags, the version stamp, as one line per call to the file
// $STUB_GO_LOG, and creates the empty file -o names. A tag name cannot
// hold a line break, so one line is one call.
const stubGo = `#!/bin/sh
while [ "$#" -gt 0 ]; do
    case "$1" in
        -ldflags) printf '%s\n' "$2" >> "$STUB_GO_LOG"; shift ;;
        -o) : > "$2"; shift ;;
    esac
    shift
done
`

// stubSha256sum stands in for sha256sum, which not every macOS has: it
// prints a digest of zeros for each file named.
const stubSha256sum = `#!/bin/sh
for f in "$@"; do printf '%064d  %s\n' 0 "$f"; done
`

// copyJustfile copies the repository justfile into dir and returns the
// copy's path.
func copyJustfile(t *testing.T, dir string) string {
	t.Helper()
	text, err := os.ReadFile(filepath.Join("..", "..", "justfile"))
	if err != nil {
		t.Fatalf("read the justfile: %v", err)
	}
	justfile := filepath.Join(dir, "justfile")
	if err := os.WriteFile(justfile, text, 0o644); err != nil {
		t.Fatalf("copy the justfile: %v", err)
	}
	return justfile
}

// lookPathOrSkip returns the path of the program name, and skips the
// test when it is not on PATH.
func lookPathOrSkip(t *testing.T, name string) string {
	t.Helper()
	path, err := exec.LookPath(name)
	if err != nil {
		t.Skipf("%s is not on PATH", name)
	}
	return path
}

// justRun is what one run of a recipe gave.
type justRun struct {
	stdout, stderr string
	err            error
}

// runJust runs `just ARGS` on the justfile in dir, from dir, with env.
func runJust(t *testing.T, justPath, dir string, env []string, args ...string) justRun {
	t.Helper()
	cmd := exec.Command(justPath, append([]string{"--justfile=" + filepath.Join(dir, "justfile")}, args...)...)
	cmd.Dir = dir
	cmd.Env = env
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	err := cmd.Run()
	return justRun{stdout: stdout.String(), stderr: stderr.String(), err: err}
}

// justEnviron is the environment the recipes run in: the test's, with
// HOME moved to home so the justfile's $HOME/go/bin on PATH holds only
// what a test puts there, and with git reading no configuration of the
// developer's (signing, hooks, a default branch).
func justEnviron(home string, extra ...string) []string {
	return testEnviron(append([]string{
		"HOME=" + home,
		"GIT_CONFIG_GLOBAL=" + os.DevNull,
		"GIT_CONFIG_NOSYSTEM=1",
	}, extra...)...)
}

// requireNoMarker fails the test when a command spelled in a crafted
// name ran and created the marker in dir.
func requireNoMarker(t *testing.T, dir string) {
	t.Helper()
	_, err := os.Stat(filepath.Join(dir, markerName))
	if err == nil {
		t.Fatalf("%s exists: the shell ran a command spelled in the name", markerName)
	}
	if !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("stat the marker: %v", err)
	}
}

// releaseNotesChangelog is a changelog whose sections a version must
// pick exactly: 0.1.0 sits between 0.10.0 and 0.0.9.
const releaseNotesChangelog = `# Changelog

## [Unreleased]

### Added

- Pending.

## [0.10.0] - 2026-03-01

### Fixed

- Ten.

## [0.1.0] - 2026-02-01

### Added

- One.
- One, a second line.

## [0.0.9] - 2026-01-01

- Nine.
`

// writeReleaseNotesFixture puts a copy of the justfile and
// releaseNotesChangelog into a new directory and returns it.
func writeReleaseNotesFixture(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	copyJustfile(t, dir)
	if err := os.WriteFile(filepath.Join(dir, "CHANGELOG.md"), []byte(releaseNotesChangelog), 0o644); err != nil {
		t.Fatalf("write the changelog: %v", err)
	}
	return dir
}

// `just release-notes X.Y.Z` prints the lines of that version's section,
// between its heading and the next one, and nothing of the others.
func TestJustfileReleaseNotes_PrintsTheSectionOfOneVersion(t *testing.T) {
	justPath := lookPathOrSkip(t, "just")
	dir := writeReleaseNotesFixture(t)

	run := runJust(t, justPath, dir, justEnviron(t.TempDir()), "release-notes", "0.1.0")

	if run.err != nil {
		t.Fatalf("just release-notes 0.1.0: %v: %s", run.err, run.stderr)
	}
	want := "\n### Added\n\n- One.\n- One, a second line.\n\n"
	if run.stdout != want {
		t.Errorf("just release-notes 0.1.0 printed %q, want %q", run.stdout, want)
	}
}

// `just release-notes` refuses a version that is not X.Y.Z and prints
// nothing, and no command spelled in the version runs. The first value
// is the one a tag `v1.0.0';touch${IFS}marker;'` gives release.yml; the
// others try the other ways a value can become shell code.
func TestJustfileReleaseNotes_RefusesAVersionThatIsNotXYZAndRunsNoPartOfIt(t *testing.T) {
	justPath := lookPathOrSkip(t, "just")
	versions := []struct {
		name, version string
	}{
		{"ends the single quotes", "1.0.0';touch${IFS}" + markerName + ";'"},
		{"ends double quotes", `1.0.0";touch${IFS}` + markerName + `;"`},
		{"command substitution", "1.0.0$(touch${IFS}" + markerName + ")"},
		{"backticks", "1.0.0`touch${IFS}" + markerName + "`"},
		{"a command separator", "1.0.0;touch${IFS}" + markerName},
		{"a second line", "1.0.0\ntouch " + markerName},
		{"a leading v", "v1.0.0"},
		{"two parts", "1.0"},
	}
	for _, tc := range versions {
		t.Run(tc.name, func(t *testing.T) {
			dir := writeReleaseNotesFixture(t)

			run := runJust(t, justPath, dir, justEnviron(t.TempDir()), "release-notes", tc.version)

			requireNoMarker(t, dir)
			if run.err == nil {
				t.Errorf("just release-notes %q succeeded; want a refusal", tc.version)
			}
			if run.stdout != "" {
				t.Errorf("just release-notes %q printed %q; want nothing", tc.version, run.stdout)
			}
		})
	}
}

// commitAndTag makes dir a git repository with one commit of its
// justfile and puts the lightweight tag on that commit, so
// `git describe --tags --dirty --always` prints exactly tag.
func commitAndTag(t *testing.T, gitPath, dir string, env []string, tag string) {
	t.Helper()
	steps := [][]string{
		{"init", "--quiet"},
		{"add", "justfile"},
		{"-c", "user.name=rx test", "-c", "user.email=rx-test@example.invalid", "commit", "--quiet", "--no-verify", "--message=Add the justfile."},
		{"tag", tag},
	}
	for _, args := range steps {
		cmd := exec.Command(gitPath, args...)
		cmd.Dir = dir
		cmd.Env = env
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %s: %v: %s", strings.Join(args, " "), err, out)
		}
	}
}

// writeStubTools puts stubGo and stubSha256sum into home/go/bin, which
// the justfile puts first on PATH.
func writeStubTools(t *testing.T, home string) {
	t.Helper()
	bin := filepath.Join(home, "go", "bin")
	if err := os.MkdirAll(bin, 0o755); err != nil {
		t.Fatalf("create %s: %v", bin, err)
	}
	for name, script := range map[string]string{"go": stubGo, "sha256sum": stubSha256sum} {
		if err := os.WriteFile(filepath.Join(bin, name), []byte(script), 0o755); err != nil {
			t.Fatalf("write the stub %s: %v", name, err)
		}
	}
}

// The recipes that read the version from `git describe` (version, build,
// build-all) stamp the tag name exactly as git prints it, and run no
// command spelled in it. Each tag tries one of the quoting contexts a
// recipe line can put a pasted value in. BUILD_VERSION is set in the
// calling shell too: the stamp comes from the tag all the same.
func TestJustfileBuildRecipes_StampTheTagNameAndRunNoPartOfIt(t *testing.T) {
	justPath := lookPathOrSkip(t, "just")
	gitPath := lookPathOrSkip(t, "git")
	tags := []struct {
		name, tag string
	}{
		{"ends single quotes", "v1.0.0';touch${IFS}" + markerName + ";'"},
		{"command substitution in double quotes", "v1.0.0$(touch${IFS}" + markerName + ")"},
		{"backticks", "v1.0.0`touch${IFS}" + markerName + "`"},
		{"a command separator", "v1.0.0;touch${IFS}" + markerName},
	}
	for _, tc := range tags {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			home := t.TempDir()
			copyJustfile(t, dir)
			writeStubTools(t, home)
			stubLog := filepath.Join(home, "go-build.log")
			env := justEnviron(home, "STUB_GO_LOG="+stubLog, "BUILD_VERSION=set-by-the-calling-shell")
			commitAndTag(t, gitPath, dir, env, tc.tag)

			version := runJust(t, justPath, dir, env, "version")
			build := runJust(t, justPath, dir, env, "build")
			buildAll := runJust(t, justPath, dir, env, "build-all")

			requireNoMarker(t, dir)
			if version.err != nil || version.stdout != tc.tag+"\n" {
				t.Errorf("just version printed %q (%v: %s); want the tag %q", version.stdout, version.err, version.stderr, tc.tag)
			}
			for recipe, run := range map[string]justRun{"build": build, "build-all": buildAll} {
				if run.err != nil {
					t.Errorf("just %s: %v: %s", recipe, run.err, run.stderr)
				}
			}
			logged, err := os.ReadFile(stubLog)
			if err != nil {
				t.Fatalf("read what go build was given: %v", err)
			}
			// One build for `build`, four targets for `build-all`.
			const wantBuilds = 5
			stamps := strings.Split(strings.TrimSuffix(string(logged), "\n"), "\n")
			if len(stamps) != wantBuilds {
				t.Errorf("go build ran %d times, want %d: %q", len(stamps), wantBuilds, stamps)
			}
			wantStamp := "-s -w -X main.appVersion=" + tc.tag
			for i, stamp := range stamps {
				if stamp != wantStamp {
					t.Errorf("go build %d got -ldflags %q, want %q", i+1, stamp, wantStamp)
				}
			}
		})
	}
}
