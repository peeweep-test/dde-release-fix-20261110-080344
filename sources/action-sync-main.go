package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
)

const pullRequestBranchPrefix = "automation/templates-sync/"

type Config struct {
	Src        string   `json:"src"`
	Dest       string   `json:"dest"`
	DeleteList []string `json:"deletelist"`
	Branches   []string `json:"branches"`
	Branch     []string `json:"branch"`
}
type Branch struct {
	Owner  string
	Repo   string
	Branch string
	Base   string
}

func main() {
	var files, message string
	var dryRun, pullRequest bool
	flag.StringVar(&message, "message", "chore: Sync by .github", "commit message")
	flag.StringVar(&files, "files", "", "config files, separated by spaces")
	flag.BoolVar(&dryRun, "dryRun", false, "dry run")
	flag.BoolVar(&pullRequest, "pull-request", false, "publish template updates through pull requests")
	flag.Parse()

	if files == "" {
		flag.PrintDefaults()
		os.Exit(1)
	}
	ctx := context.Background()

	// Sync all repositories if do not repos changed
	for _, file := range strings.Fields(files) {
		data, err := os.ReadFile(file)
		if err != nil {
			log.Fatal(err)
		}
		var configs []Config
		err = json.Unmarshal(data, &configs)
		if err != nil {
			log.Fatal(err)
		}
		log.Println("Config", file)

		// 临时目录，用于存放git仓库
		tmpDir, err := os.MkdirTemp("./", "tmp_")
		if err != nil {
			log.Fatal(err)
		}

		// map[$repo][$branch]$changed
		changedBranches := make(map[string]map[string]bool)

		for _, config := range configs {
			if len(config.Branches) == 0 {
				config.Branches = config.Branch
			}
			owner, repo, path, err := split(config.Dest)
			if err != nil {
				log.Fatal(err)
			}
			log.Printf("\tSync %s to %s/%s/%s", config.Src, owner, repo, path)
			if dryRun {
				continue
			}
			workdir := filepath.Join(tmpDir, owner, repo)
			if changedBranches[workdir] == nil {
				changedBranches[workdir] = make(map[string]bool)
				_, err = execCommand(ctx, "./", "git", "clone", fmt.Sprintf("git@github.com:%s/%s.git", owner, repo), workdir)
				if err != nil {
					log.Fatal(err)
				}
			}
			var branches []string
			out, err := execCommand(context.Background(), workdir, "git", "branch", "-r")
			if err != nil {
				log.Fatal(err)
			}
			lines := strings.Split(strings.TrimSpace(string(out)), "\n")
			for i := range lines {
				line := strings.TrimSpace(lines[i])
				if strings.HasPrefix(line, "origin/HEAD") {
					continue
				}
				branch := strings.TrimPrefix(line, "origin/")
				if strings.HasPrefix(branch, pullRequestBranchPrefix) {
					continue
				}
				branches = append(branches, branch)
			}
			var syncBranches []string
			// match branch
			for i := range branches {
				match := false
				for j := range config.Branches {
					reg := regexp.MustCompile("^" + config.Branches[j])
					if reg.Match([]byte(branches[i])) {
						match = true
						break
					}
				}
				if match {
					syncBranches = append(syncBranches, branches[i])
				}
			}
			// all branch
			if len(config.Branches) == 0 {
				syncBranches = append(syncBranches, branches...)
			}

			for _, branch := range syncBranches {
				out, err = execCommand(ctx, workdir, "git", "checkout", branch)
				if err != nil {
					logError("err: %v output: %s", err, string(out))
					os.Exit(1)
				}
				// 删除文件， 如果有delete_list配置，就不再进行同步
				if len(config.DeleteList) > 0 {
					_, err = execCommand(ctx, workdir, "rm", append([]string{"-f"}, config.DeleteList...)...)
					if err != nil {
						log.Fatal(err)
					}
				} else {
					// 更新文件
					_, err = execCommand(ctx, workdir, "mkdir", "-p", filepath.Dir(path))
					if err != nil {
						log.Fatal(err)
					}
					_, err = execCommand(ctx, workdir, "cp", filepath.Join("../../../", config.Src), path)
					if err != nil {
						log.Fatal(err)
					}
				}
				_, err = execCommand(ctx, workdir, "git", "add", "--force", ":/")
				if err != nil {
					log.Fatal(err)
				}
				// 判断是否有文件变动
				out, err := execCommand(ctx, workdir, "git", "status", "-s")
				if err != nil {
					log.Fatal(err)
				}
				if len(out) == 0 {
					continue
				}
				// 多个变动合并成一个提交
				if changedBranches[workdir][branch] {
					_, err = execCommand(ctx, workdir, "git", "commit", "--amend", "-a", "-m", message)
					if err != nil {
						log.Fatal(err)
					}
				} else {
					_, err = execCommand(ctx, workdir, "git", "commit", "-a", "-m", message)
					if err != nil {
						log.Fatal(err)
					}
					changedBranches[workdir][branch] = true
				}
			}
		}

		// 提交所有变动的仓库分支
		for workdir := range changedBranches {
			for branch := range changedBranches[workdir] {
				var err error
				if pullRequest {
					err = syncPullRequest(ctx, workdir, branch, message)
				} else {
					_, err = execCommand(ctx, workdir, "git", "push", "origin", branch)
				}
				if err != nil {
					logError("err: %v", err)
					os.Exit(1)
				}
			}
		}
		// 清理临时目录
		err = os.RemoveAll(tmpDir)
		if err != nil {
			log.Fatal(err)
		}
	}
}

func syncPullRequest(ctx context.Context, workdir, base, message string) error {
	repo := filepath.Base(filepath.Dir(workdir)) + "/" + filepath.Base(workdir)
	branch := pullRequestBranchPrefix + base
	url, err := execCommand(ctx, workdir, "gh", "pr", "list", "--repo", repo,
		"--head", branch, "--base", base, "--state", "open", "--json", "url", "--jq", ".[0].url // empty")
	if err != nil {
		return err
	}
	previous, err := execCommand(ctx, workdir, "git", "rev-parse", "--verify", "-q", "refs/remotes/origin/"+branch)
	var exitError *exec.ExitError
	if err != nil && (!errors.As(err, &exitError) || exitError.ExitCode() != 1) {
		return err
	}
	// Only the tool-owned PR branch may be replaced, using the clone's expected head.
	lease := "--force-with-lease=refs/heads/" + branch + ":" + strings.TrimSpace(string(previous))
	if _, err := execCommand(ctx, workdir, "git", "push", lease, "origin", base+":refs/heads/"+branch); err != nil {
		return err
	}
	if len(strings.TrimSpace(string(url))) == 0 {
		url, err = execCommand(ctx, workdir, "gh", "pr", "create", "--repo", repo,
			"--head", branch, "--base", base, "--title", "chore: sync workflow templates", "--body", message)
		if err != nil {
			return err
		}
	}
	log.Printf("Pull request: %s", strings.TrimSpace(string(url)))
	return nil
}

func logError(format string, args ...interface{}) {
	msg := fmt.Sprintf(format, args...)
	fmt.Printf("::error::%s\n", msg)
}

func logDebug(format string, args ...interface{}) {
	msg := fmt.Sprintf(format, args...)
	fmt.Printf("::debug::%s\n", msg)
}

func execCommand(ctx context.Context, workdir string, command string, args ...string) ([]byte, error) {
	logDebug("exec at %s, command: %s", workdir, strings.Join(append([]string{command}, args...), " "))
	cmd := exec.Command(command, args...)
	if len(workdir) > 0 {
		cmd.Dir = workdir
	}
	data, err := cmd.CombinedOutput()
	if err != nil {
		logDebug("command: %s, error: %s", strings.Join(append([]string{command}, args...), " "), data)
		return nil, fmt.Errorf("%w: %s", err, string(data))
	}
	return data, nil
}

func split(dest string) (owner, repo, path string, err error) {
	arr := strings.SplitN(dest, "/", 3)
	if len(arr) != 3 {
		return "", "", "", fmt.Errorf("wrong dist. example: owner/repo/file")
	}
	return arr[0], arr[1], arr[2], nil
}
