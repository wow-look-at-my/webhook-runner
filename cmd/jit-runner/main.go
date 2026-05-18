// Command jit-runner handles GitHub workflow_job webhook events by requesting
// a JIT runner configuration and executing the Actions runner binary.
package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

func main() {
	if err := run(); err != nil {
		fmt.Fprintf(os.Stderr, "error: %v\n", err)
		os.Exit(1)
	}
}

func run() error {
	token := os.Getenv("JIT_GITHUB_TOKEN")
	if token == "" {
		return fmt.Errorf("JIT_GITHUB_TOKEN not set")
	}
	runnerDir := os.Getenv("RUNNER_DIR")
	if runnerDir == "" {
		runnerDir = "/opt/actions-runner"
	}

	ev, err := parseEvent()
	if err != nil {
		return err
	}
	if ev.Action != "queued" {
		fmt.Printf("ignoring action=%s\n", ev.Action)
		return nil
	}
	if !hasLabel(ev.WorkflowJob.Labels, "self-hosted") {
		fmt.Println("ignoring: no self-hosted label")
		return nil
	}

	labels := customLabels(ev.WorkflowJob.Labels)
	if len(labels) == 0 {
		return fmt.Errorf("no custom labels in %v; add at least one (e.g. 'jit')", ev.WorkflowJob.Labels)
	}

	fmt.Printf("job queued: repo=%s job=%d labels=%v\n",
		ev.Repo.FullName, ev.WorkflowJob.ID, ev.WorkflowJob.Labels)

	jitConfig, err := requestJITConfig(token, ev.Repo.FullName, ev.WorkflowJob.ID, labels)
	if err != nil {
		return fmt.Errorf("jit config: %w", err)
	}
	fmt.Println("jit config obtained, starting runner")

	cmd := exec.Command(filepath.Join(runnerDir, "run.sh"), "--jitconfig", jitConfig)
	cmd.Dir = runnerDir
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("runner: %w", err)
	}
	fmt.Println("runner finished")
	return nil
}

type event struct {
	Action      string      `json:"action"`
	WorkflowJob workflowJob `json:"workflow_job"`
	Repo        repository  `json:"repository"`
}

type workflowJob struct {
	ID     int64    `json:"id"`
	Labels []string `json:"labels"`
}

type repository struct {
	FullName string `json:"full_name"`
}

func parseEvent() (*event, error) {
	path := os.Getenv("HOOK_PAYLOAD_FILE")
	if path == "" {
		return nil, fmt.Errorf("HOOK_PAYLOAD_FILE not set")
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read payload: %w", err)
	}
	var ev event
	if err := json.Unmarshal(data, &ev); err != nil {
		return nil, fmt.Errorf("parse payload: %w", err)
	}
	return &ev, nil
}

func hasLabel(labels []string, target string) bool {
	for _, l := range labels {
		if strings.EqualFold(l, target) {
			return true
		}
	}
	return false
}

// defaultRunnerLabels are automatically applied to self-hosted runners and
// should not be passed to the generate-jitconfig API (which expects custom
// labels only).
var defaultRunnerLabels = map[string]bool{
	"self-hosted": true, "linux": true, "macos": true, "windows": true,
	"x64": true, "arm": true, "arm64": true,
}

func customLabels(all []string) []string {
	var out []string
	for _, l := range all {
		if !defaultRunnerLabels[strings.ToLower(l)] {
			out = append(out, l)
		}
	}
	return out
}

func requestJITConfig(token, repoFullName string, jobID int64, labels []string) (string, error) {
	body, _ := json.Marshal(map[string]any{
		"name":            fmt.Sprintf("jit-%d", jobID),
		"runner_group_id": 1,
		"labels":          labels,
	})
	url := fmt.Sprintf("https://api.github.com/repos/%s/actions/runners/generate-jitconfig", repoFullName)
	req, err := http.NewRequest("POST", url, bytes.NewReader(body))
	if err != nil {
		return "", err
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("X-GitHub-Api-Version", "2022-11-28")

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusCreated {
		b, _ := io.ReadAll(resp.Body)
		return "", fmt.Errorf("api %d: %s", resp.StatusCode, b)
	}
	var result struct {
		EncodedJITConfig string `json:"encoded_jit_config"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return "", err
	}
	return result.EncodedJITConfig, nil
}
