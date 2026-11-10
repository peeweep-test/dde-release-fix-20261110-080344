// SPDX-FileCopyrightText: 2026 UnionTech Software Technology Co., Ltd.
// SPDX-License-Identifier: GPL-3.0-or-later

package main

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

var testBinary string

func TestMain(m *testing.M) {
	directory, err := os.MkdirTemp("", "action-sync-test-")
	if err != nil {
		panic(err)
	}
	testBinary = filepath.Join(directory, "action-sync")
	if output, err := exec.Command("go", "build", "-o", testBinary, ".").CombinedOutput(); err != nil {
		os.RemoveAll(directory)
		panic(string(output))
	}
	code := m.Run()
	os.RemoveAll(directory)
	os.Exit(code)
}

type fixture struct {
	root, remote, before string
	env                  []string
}

func writeTestFile(t *testing.T, name, text string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(name), 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(name, []byte(text), 0755); err != nil {
		t.Fatal(err)
	}
}

func testCommand(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command(args[0], args[1:]...)
	cmd.Dir = dir
	output, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("%v: %v\n%s", args, err, output)
	}
	return strings.TrimSpace(string(output))
}

func newFixture(t *testing.T) fixture {
	t.Helper()
	root := t.TempDir()
	remote := filepath.Join(root, "repo.git")
	seed := filepath.Join(root, "seed")
	testCommand(t, root, "git", "init", "--bare", "--initial-branch=master", remote)
	testCommand(t, root, "git", "init", "--initial-branch=master", seed)
	testCommand(t, seed, "git", "config", "user.name", "Sync test")
	testCommand(t, seed, "git", "config", "user.email", "sync@example.invalid")
	writeTestFile(t, filepath.Join(seed, ".github/workflows/example.yml"), "old template\n")
	writeTestFile(t, filepath.Join(seed, "obsolete.yml"), "obsolete\n")
	testCommand(t, seed, "git", "add", ".")
	testCommand(t, seed, "git", "commit", "-m", "fixture")
	testCommand(t, seed, "git", "push", remote, "master")
	before := testCommand(t, seed, "git", "rev-parse", "HEAD")
	writeTestFile(t, filepath.Join(remote, "hooks/pre-receive"), `#!/bin/sh
while read old new ref; do
  if [ "$ref" = refs/heads/master ]; then
    echo 'protected branch: use a pull request' >&2
    exit 1
  fi
done
`)
	writeTestFile(t, filepath.Join(root, "templates/example.yml"), "new template\n")
	configs := []Config{
		{Src: "templates/example.yml", Dest: "owner/repo/.github/workflows/example.yml", Branches: []string{"master"}},
		{Src: "templates/example.yml", Dest: "owner/repo/obsolete.yml", DeleteList: []string{"obsolete.yml"}, Branches: []string{"master"}},
	}
	data, err := json.Marshal(configs)
	if err != nil {
		t.Fatal(err)
	}
	writeTestFile(t, filepath.Join(root, "configs.json"), string(data))
	writeTestFile(t, filepath.Join(root, "gitconfig"), "[user]\nname = Sync test\nemail = sync@example.invalid\n"+
		"[url \"file://"+remote+"\"]\ninsteadOf = git@github.com:owner/repo.git\n")
	writeTestFile(t, filepath.Join(root, "bin/gh"), `#!/bin/sh
printf '%s\n' "$*" >> "$TEST_ROOT/gh.log"
if [ "$2" = "$TEST_GH_FAILURE" ]; then
  echo 'GitHub API unavailable' >&2
  exit 1
fi
case "$1 $2" in
  'pr list')
    if [ -n "$TEST_RACE_SHA" ]; then
      git --git-dir="$TEST_ROOT/repo.git" update-ref refs/heads/automation/templates-sync/master "$TEST_RACE_SHA"
    fi
    if [ -f "$TEST_ROOT/pr-created" ]; then
      echo 'https://github.com/owner/repo/pull/1'
    fi
    ;;
  'pr create')
    touch "$TEST_ROOT/pr-created"
    echo 'https://github.com/owner/repo/pull/1'
    ;;
  *) exit 2 ;;
esac
`)
	return fixture{root: root, remote: remote, before: before, env: append(os.Environ(),
		"GIT_CONFIG_GLOBAL="+filepath.Join(root, "gitconfig"), "GIT_CONFIG_NOSYSTEM=1",
		"PATH="+filepath.Join(root, "bin")+string(os.PathListSeparator)+os.Getenv("PATH"), "TEST_ROOT="+root)}
}

func (f fixture) run(pullRequest bool, extraEnv ...string) ([]byte, error) {
	mode := "false"
	if pullRequest {
		mode = "true"
	}
	cmd := exec.Command(testBinary, "-files=configs.json", "-pull-request="+mode, "-message=test: sync templates")
	cmd.Dir = f.root
	cmd.Env = append(append([]string{}, f.env...), extraEnv...)
	return cmd.CombinedOutput()
}

func TestPullRequestKeepsProtectedBaseAndReusesProposal(t *testing.T) {
	f := newFixture(t)
	for _, revision := range []string{"first update\n", "second update\n"} {
		writeTestFile(t, filepath.Join(f.root, "templates/example.yml"), revision)
		if output, err := f.run(true); err != nil {
			t.Fatalf("%v\n%s", err, output)
		}
		if got := testCommand(t, f.remote, "git", "rev-parse", "master"); got != f.before {
			t.Fatalf("protected base changed: %s", got)
		}
		got := testCommand(t, f.remote, "git", "show", "automation/templates-sync/master:.github/workflows/example.yml")
		if got != strings.TrimSpace(revision) {
			t.Fatalf("template not updated: %q", got)
		}
		files := testCommand(t, f.remote, "git", "ls-tree", "--name-only", "automation/templates-sync/master")
		if strings.Contains(files, "obsolete.yml") {
			t.Fatal("configured deletion was lost")
		}
	}
	log, err := os.ReadFile(filepath.Join(f.root, "gh.log"))
	if err != nil || strings.Count(string(log), "pr create ") != 1 {
		t.Fatalf("proposal was duplicated: %v\n%s", err, log)
	}
}

func TestDirectPushRejectionFailsTheProcess(t *testing.T) {
	f := newFixture(t)
	if output, err := f.run(false); err == nil || !strings.Contains(string(output), "protected branch") {
		t.Fatalf("push rejection did not fail the process: %v\n%s", err, output)
	}
	if got := testCommand(t, f.remote, "git", "rev-parse", "master"); got != f.before {
		t.Fatalf("protected base changed: %s", got)
	}
}

func TestPullRequestAPIFailureFailsTheProcess(t *testing.T) {
	for _, operation := range []string{"list", "create"} {
		t.Run(operation, func(t *testing.T) {
			f := newFixture(t)
			if output, err := f.run(true, "TEST_GH_FAILURE="+operation); err == nil || !strings.Contains(string(output), "GitHub API unavailable") {
				t.Fatalf("API failure was ignored: %v\n%s", err, output)
			}
			if got := testCommand(t, f.remote, "git", "rev-parse", "master"); got != f.before {
				t.Fatalf("protected base changed: %s", got)
			}
		})
	}
}

func TestConcurrentProposalChangeIsNotOverwritten(t *testing.T) {
	f := newFixture(t)
	output, err := f.run(true, "TEST_RACE_SHA="+f.before)
	if err == nil || !strings.Contains(string(output), "stale info") {
		t.Fatalf("concurrent proposal change was not rejected: %v\n%s", err, output)
	}
	if got := testCommand(t, f.remote, "git", "rev-parse", "automation/templates-sync/master"); got != f.before {
		t.Fatalf("concurrent commit was overwritten: %s", got)
	}
}

func TestUnrestrictedSyncSkipsProposalBranchesAndUnchangedFiles(t *testing.T) {
	f := newFixture(t)
	writeTestFile(t, filepath.Join(f.root, "configs.json"), `[{"src":"templates/example.yml","dest":"owner/repo/.github/workflows/example.yml"}]`)
	writeTestFile(t, filepath.Join(f.root, "templates/example.yml"), "old template\n")
	if output, err := f.run(true); err != nil {
		t.Fatalf("%v\n%s", err, output)
	}
	if _, err := os.Stat(filepath.Join(f.root, "gh.log")); !os.IsNotExist(err) {
		t.Fatal("unchanged files should not create a PR")
	}
	for _, revision := range []string{"first update\n", "second update\n"} {
		writeTestFile(t, filepath.Join(f.root, "templates/example.yml"), revision)
		if output, err := f.run(true); err != nil {
			t.Fatalf("%v\n%s", err, output)
		}
	}
	branches := testCommand(t, f.remote, "git", "for-each-ref", "--format=%(refname:short)", "refs/heads/")
	if branches != "automation/templates-sync/master\nmaster" {
		t.Fatalf("unexpected recursive template PR branches: %s", branches)
	}
}

func TestCompositePreservesLiteralInputsAndBinaryExitCode(t *testing.T) {
	data, err := os.ReadFile("action.yml")
	if err != nil {
		t.Fatal(err)
	}
	workflow := string(data)
	marker := "      run: |\n"
	start := strings.LastIndex(workflow, marker)
	if start < 0 {
		t.Fatal("missing composite run step")
	}
	script := workflow[start+len(marker):]
	dir := t.TempDir()
	writeTestFile(t, filepath.Join(dir, "action-sync"), "#!/bin/sh\nprintf '%s\\n' \"$@\" > \"$TEST_ARGS\"\nexit \"$TEST_EXIT\"\n")
	injected := filepath.Join(dir, "injected")
	message := "chore: $(touch " + injected + ")\n`touch " + injected + "`"
	for _, exit := range []string{"0", "13"} {
		cmd := exec.Command("bash", "-e", "-c", script)
		cmd.Env = append(os.Environ(), "GITHUB_ACTION_PATH="+dir, "INPUT_FILES=one.json two.json",
			"INPUT_FILES_FROM="+filepath.Join(dir, "missing"), "INPUT_MESSAGE="+message,
			"INPUT_PULL_REQUEST=true", "TEST_ARGS="+filepath.Join(dir, "args"), "TEST_EXIT="+exit)
		output, err := cmd.CombinedOutput()
		if (exit == "0") != (err == nil) {
			t.Fatalf("binary failure not propagated: %v\n%s", err, output)
		}
		args, err := os.ReadFile(filepath.Join(dir, "args"))
		if err != nil || string(args) != "-files=one.json two.json\n-message="+message+"\n-pull-request=true\n" {
			t.Fatalf("inputs changed: %v\n%s", err, args)
		}
	}
	if _, err := os.Stat(injected); !os.IsNotExist(err) {
		t.Fatal("commit message was executed by the shell")
	}
}

func TestLegacyBranchSelectorDoesNotOpenPRsForOtherBranches(t *testing.T) {
	f := newFixture(t)
	testCommand(t, f.remote, "git", "update-ref", "refs/heads/development", f.before)
	writeTestFile(t, filepath.Join(f.root, "configs.json"), `[{"src":"templates/example.yml","dest":"owner/repo/.github/workflows/example.yml","branch":["master"]}]`)
	if output, err := f.run(true); err != nil {
		t.Fatalf("%v\n%s", err, output)
	}
	branches := testCommand(t, f.remote, "git", "for-each-ref", "--format=%(refname:short)", "refs/heads/")
	if branches != "automation/templates-sync/master\ndevelopment\nmaster" {
		t.Fatalf("legacy branch selector ignored: %s", branches)
	}
}
