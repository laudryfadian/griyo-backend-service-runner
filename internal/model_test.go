package internal

import (
	"context"
	"github.com/laudryfadian/griyo-backend-service-runner/internal/domain"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
)

func TestProviderProposalAndCost(t *testing.T) {
	root := t.TempDir()
	t.Setenv("WORKSPACE_ROOT", root)
	t.Setenv("AI_TEST_KEY", "test-only-key")
	t.Setenv("AI_INPUT_USD_PER_MILLION", "1")
	t.Setenv("AI_OUTPUT_USD_PER_MILLION", "2")
	provider := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/chat/completions" || r.Header.Get("Authorization") != "Bearer test-only-key" {
			t.Error("provider request mismatch")
		}
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"choices":[{"message":{"role":"assistant","content":"{\"summary\":\"Added handler\",\"files\":[{\"path\":\"src/handler.ts\",\"content\":\"export const ready = true;\"}],\"tests\":[\"Review handler\"]}"}}],"usage":{"prompt_tokens":100,"completion_tokens":50}}`))
	}))
	defer provider.Close()
	runner := New("http://unused", "token")
	task := domain.Task{Id: "test-task", Budget: 1, Execution: domain.TaskExecution{CredentialRef: "AI_TEST_KEY", Endpoint: provider.URL + "/v1", Model: "test-model"}}
	result, spent, e := runner.generate(context.Background(), task)
	if e != nil {
		t.Fatal(e)
	}
	if result == "" || spent != 0.0002 {
		t.Fatalf("incorrect result/cost %v", spent)
	}
	b, e := os.ReadFile(filepath.Join(root, "test-task/src/handler.ts"))
	if e != nil || string(b) != "export const ready = true;" {
		t.Fatal("proposal not saved")
	}
}
