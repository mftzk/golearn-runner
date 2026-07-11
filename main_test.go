package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestHandleRunPassesStdinToProgram(t *testing.T) {
	requestBody := `{"code":"package main\nimport \"fmt\"\nfunc main(){var value string; fmt.Scan(&value); fmt.Println(\"got:\", value)}","stdin":"hello\n"}`
	request := httptest.NewRequest(http.MethodPost, "/run", strings.NewReader(requestBody))
	request.Header.Set("X-Runner-Token", "test-token")
	recorder := httptest.NewRecorder()

	handleRun(recorder, request, "test-token")

	if recorder.Code != http.StatusOK {
		t.Fatalf("expected status 200, got %d: %s", recorder.Code, recorder.Body.String())
	}

	var response runResponse
	if err := json.Unmarshal(recorder.Body.Bytes(), &response); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if !response.OK {
		t.Fatalf("expected program to run successfully: %s", response.Stderr)
	}
	if response.Stdout != "got: hello\n" {
		t.Fatalf("expected stdin to reach program, got %q", response.Stdout)
	}
}

func TestHandleRunRejectsOversizedStdin(t *testing.T) {
	requestBody := `{"code":"package main\nfunc main(){}","stdin":"` + strings.Repeat("x", maxInputBytes+1) + `"}`
	request := httptest.NewRequest(http.MethodPost, "/run", strings.NewReader(requestBody))
	request.Header.Set("X-Runner-Token", "test-token")
	recorder := httptest.NewRecorder()

	handleRun(recorder, request, "test-token")

	if recorder.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("expected status 413, got %d: %s", recorder.Code, recorder.Body.String())
	}
}

func TestHandleRunRejectsInvalidToken(t *testing.T) {
	request := httptest.NewRequest(http.MethodPost, "/run", strings.NewReader(`{"code":"package main\nfunc main(){}"}`))
	request.Header.Set("X-Runner-Token", "wrong-token")
	recorder := httptest.NewRecorder()

	handleRun(recorder, request, "test-token")

	if recorder.Code != http.StatusUnauthorized {
		t.Fatalf("expected status 401, got %d", recorder.Code)
	}
}
