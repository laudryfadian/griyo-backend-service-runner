package internal

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"github.com/laudryfadian/griyo-backend-service-runner/internal/domain"
	"io"
	"io/fs"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"
)

type Runner struct {
	endpoint string
	token    string
	client   *http.Client
}
type chatMessage struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}
type chatRequest struct {
	Model     string        `json:"model"`
	Messages  []chatMessage `json:"messages"`
	MaxTokens int           `json:"max_tokens"`
}
type chatResponse struct {
	Choices []struct {
		Message chatMessage `json:"message"`
	} `json:"choices"`
	Usage struct {
		PromptTokens     int `json:"prompt_tokens"`
		CompletionTokens int `json:"completion_tokens"`
	} `json:"usage"`
}
type proposal struct {
	Summary string `json:"summary"`
	Files   []struct {
		Path    string `json:"path"`
		Content string `json:"content"`
	} `json:"files"`
	Tests []string `json:"tests"`
}

func New(endpoint, token string) *Runner {
	return &Runner{strings.TrimRight(endpoint, "/"), token, &http.Client{Timeout: 60 * time.Second}}
}
func (r *Runner) call(ctx context.Context, path string, input, output interface{}) error {
	b, e := json.Marshal(input)
	if e != nil {
		return e
	}
	req, e := http.NewRequestWithContext(ctx, "POST", r.endpoint+"/api/runner/"+path, bytes.NewReader(b))
	if e != nil {
		return e
	}
	req.Header.Set("Authorization", "Bearer "+r.token)
	req.Header.Set("Content-Type", "application/json")
	res, e := r.client.Do(req)
	if e != nil {
		return e
	}
	defer res.Body.Close()
	if res.StatusCode >= 300 {
		return fmt.Errorf("runner API status %d", res.StatusCode)
	}
	return json.NewDecoder(io.LimitReader(res.Body, 4<<20)).Decode(output)
}
func (r *Runner) Run(ctx context.Context) error {
	ticker := time.NewTicker(20 * time.Second)
	defer ticker.Stop()
	var workers sync.WaitGroup
	defer workers.Wait()
	for {
		metrics := memoryMetrics()
		var response json.RawMessage
		if e := r.call(ctx, "heartbeat", metrics, &response); e != nil {
			fmt.Fprintln(os.Stderr, e)
		} else {
			var task *domain.Task
			if e := r.call(ctx, "claim", struct{}{}, &task); e == nil && task != nil && task.Id != "" {
				workers.Add(1)
				go func() { defer workers.Done(); r.execute(ctx, *task) }()
			}
		}
		select {
		case <-ctx.Done():
			return nil
		case <-ticker.C:
		}
	}
}
func memoryMetrics() domain.Metrics {
	result := domain.Metrics{Containers: []domain.Container{}}
	b, _ := os.ReadFile("/proc/meminfo")
	var total, available float64
	for _, line := range strings.Split(string(b), "\n") {
		fields := strings.Fields(line)
		if len(fields) > 1 {
			switch fields[0] {
			case "MemTotal:":
				fmt.Sscan(fields[1], &total)
			case "MemAvailable:":
				fmt.Sscan(fields[1], &available)
			}
		}
	}
	result.MemoryTotalGb = total / 1048576
	result.MemoryUsedGb = (total - available) / 1048576
	return result
}

var credentialPattern = regexp.MustCompile(`^AI_[A-Z0-9_]+$`)

func safePath(root, path string) (string, error) {
	if path == "" || filepath.IsAbs(path) || strings.Contains(path, "\\") {
		return "", errors.New("invalid file path")
	}
	clean := filepath.Clean(path)
	if clean == ".." || strings.HasPrefix(clean, "../") || strings.HasPrefix(clean, ".git/") || clean == ".git" {
		return "", errors.New("path outside workspace")
	}
	target := filepath.Join(root, clean)
	for current := target; current != root; current = filepath.Dir(current) {
		info, e := os.Lstat(current)
		if e == nil && info.Mode()&os.ModeSymlink != 0 {
			return "", errors.New("symlinks are not allowed")
		}
	}
	return target, nil
}
func (r *Runner) execute(parent context.Context, task domain.Task) {
	duration := time.Duration(task.Execution.TimeoutMinutes) * time.Minute
	if duration <= 0 {
		duration = 10 * time.Minute
	}
	ctx, cancel := context.WithTimeout(parent, duration)
	defer cancel()
	done := make(chan struct{})
	defer close(done)
	go func() {
		ticker := time.NewTicker(20 * time.Second)
		defer ticker.Stop()
		for {
			select {
			case <-done:
				return
			case <-ctx.Done():
				return
			case <-ticker.C:
				var response json.RawMessage
				if r.call(ctx, "tasks/"+task.Id, domain.TaskUpdate{Status: "Working", Progress: 10, Spent: task.Spent, LeaseId: task.LeaseId}, &response) != nil {
					cancel()
					return
				}
			}
		}
	}()
	result, spent, e := r.generate(ctx, task)
	update := domain.TaskUpdate{Status: "Review", Progress: 100, Result: result, Spent: spent, LeaseId: task.LeaseId}
	if e != nil {
		update.Status = "Failed"
		update.Error = e.Error()
		update.Progress = 0
	}
	reportCtx, reportCancel := context.WithTimeout(parent, 15*time.Second)
	defer reportCancel()
	var response json.RawMessage
	if e = r.call(reportCtx, "tasks/"+task.Id, update, &response); e != nil {
		fmt.Fprintln(os.Stderr, "report task:", e)
	}
}
func (r *Runner) generate(ctx context.Context, task domain.Task) (string, float64, error) {
	if !credentialPattern.MatchString(task.Execution.CredentialRef) {
		return "", task.Spent, errors.New("credentialRef must reference an AI_ environment variable")
	}
	key := os.Getenv(task.Execution.CredentialRef)
	if key == "" {
		return "", task.Spent, errors.New("AI credential is missing on this runner")
	}
	root := filepath.Join(env("WORKSPACE_ROOT", "/workspaces"), task.Id)
	repoContext, setupError := prepareWorkspace(ctx, root, task.Execution.RepositoryUrl)
	if setupError != nil {
		return "", task.Spent, setupError
	}
	request := chatRequest{Model: task.Execution.Model, MaxTokens: 4096, Messages: []chatMessage{{Role: "system", Content: task.Execution.Instructions + "\nReturn only JSON with summary:string, files:[{path:string,content:string}], tests:string[]. Produce a reviewable proposal; do not claim tests ran. Use the provided repository context. Preserve unrelated code."}, {Role: "user", Content: task.Title + "\n" + task.Brief + "\nAcceptance criteria:\n" + task.Criteria + "\nRepository context:\n" + repoContext}}}
	inputRate, inputError := strconv.ParseFloat(os.Getenv("AI_INPUT_USD_PER_MILLION"), 64)
	outputRate, outputError := strconv.ParseFloat(os.Getenv("AI_OUTPUT_USD_PER_MILLION"), 64)
	if inputError != nil || outputError != nil || inputRate <= 0 || outputRate <= 0 {
		return "", task.Spent, errors.New("configure positive token prices before executing tasks")
	}
	b, _ := json.Marshal(request)
	upperCost := (float64(len(b)+1000)*inputRate + 4096*outputRate) / 1000000
	if task.Spent+upperCost > task.Budget {
		return "", task.Spent, errors.New("remaining budget insufficient for token reservation")
	}
	req, e := http.NewRequestWithContext(ctx, "POST", strings.TrimRight(task.Execution.Endpoint, "/")+"/chat/completions", bytes.NewReader(b))
	if e != nil {
		return "", task.Spent, e
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+key)
	res, e := r.client.Do(req)
	if e != nil {
		return "", task.Spent, e
	}
	defer res.Body.Close()
	if res.StatusCode >= 300 {
		return "", task.Spent, fmt.Errorf("model provider status %d", res.StatusCode)
	}
	var chat chatResponse
	if e = json.NewDecoder(io.LimitReader(res.Body, 4<<20)).Decode(&chat); e != nil {
		return "", task.Spent, e
	}
	spent := task.Spent + (float64(chat.Usage.PromptTokens)*inputRate+float64(chat.Usage.CompletionTokens)*outputRate)/1000000
	if chat.Usage.PromptTokens == 0 && chat.Usage.CompletionTokens == 0 {
		spent = task.Spent + upperCost
	}
	if len(chat.Choices) == 0 {
		return "", spent, errors.New("model returned no answer")
	}
	answer := strings.TrimSpace(chat.Choices[0].Message.Content)
	answer = strings.TrimPrefix(answer, "```json")
	answer = strings.TrimPrefix(answer, "```")
	answer = strings.TrimSuffix(answer, "```")
	var plan proposal
	decoder := json.NewDecoder(strings.NewReader(answer))
	decoder.DisallowUnknownFields()
	if e = decoder.Decode(&plan); e != nil {
		return "", spent, fmt.Errorf("invalid proposal: %w", e)
	}
	if e = os.MkdirAll(root, 0700); e != nil {
		return "", spent, e
	}
	for _, file := range plan.Files {
		target, pathError := safePath(root, file.Path)
		if pathError != nil {
			return "", spent, pathError
		}
		if len(file.Content) > 1<<20 {
			return "", spent, errors.New("proposal file too large")
		}
		if e = os.MkdirAll(filepath.Dir(target), 0700); e != nil {
			return "", spent, e
		}
		if e = os.WriteFile(target, []byte(file.Content), 0600); e != nil {
			return "", spent, e
		}
	}
	result, _ := json.MarshalIndent(plan, "", "  ")
	if task.Execution.RepositoryUrl != "" {
		cmd := exec.CommandContext(ctx, "git", "-C", root, "--no-pager", "diff", "--no-ext-diff", "--no-textconv")
		diff, diffError := cmd.Output()
		if diffError == nil {
			result = append(result, []byte("\n\nTracked file diff:\n"+string(diff))...)
		}
	}
	return string(result), spent, nil
}
func env(key, fallback string) string {
	if value := os.Getenv(key); value != "" {
		return value
	}
	return fallback
}

func prepareWorkspace(ctx context.Context, root, repository string) (string, error) {
	if repository != "" {
		parsed, e := url.Parse(repository)
		if e != nil || parsed.Scheme != "https" || parsed.User != nil || parsed.Host == "" {
			return "", errors.New("repository must be credential-free HTTPS")
		}
		if _, e = os.Stat(filepath.Join(root, ".git")); os.IsNotExist(e) {
			if e = os.MkdirAll(filepath.Dir(root), 0700); e != nil {
				return "", e
			}
			cmd := exec.CommandContext(ctx, "git", "-c", "core.hooksPath=/dev/null", "-c", "protocol.file.allow=never", "clone", "--depth", "1", "--", repository, root)
			cmd.Env = append(os.Environ(), "GIT_TERMINAL_PROMPT=0")
			if e = cmd.Run(); e != nil {
				return "", errors.New("repository clone failed; public HTTPS repositories are supported")
			}
		}
	} else if e := os.MkdirAll(root, 0700); e != nil {
		return "", e
	}
	var content strings.Builder
	e := filepath.WalkDir(root, func(path string, entry fs.DirEntry, walkError error) error {
		if walkError != nil {
			return walkError
		}
		relative, e := filepath.Rel(root, path)
		if e != nil {
			return e
		}
		if entry.IsDir() {
			if entry.Name() == ".git" || entry.Name() == "node_modules" || entry.Name() == "vendor" || entry.Name() == "dist" {
				return filepath.SkipDir
			}
			return nil
		}
		if entry.Type()&os.ModeSymlink != 0 || strings.HasPrefix(entry.Name(), ".env") {
			return nil
		}
		if content.Len() > 32000 {
			return nil
		}
		info, e := entry.Info()
		if e != nil || info.Size() > 8000 {
			return nil
		}
		b, e := os.ReadFile(path)
		if e != nil {
			return nil
		}
		if bytes.IndexByte(b, 0) >= 0 {
			return nil
		}
		content.WriteString("\nFILE " + relative + "\n" + string(b))
		return nil
	})
	return content.String(), e
}
