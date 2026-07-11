package main

import (
	"bytes"
	"context"
	"encoding/json"
	"log"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"
)

const (
	maxSourceBytes = 64 * 1024
	maxOutputBytes = 64 * 1024
	runTimeout     = 5 * time.Second
)

type runRequest struct {
	Code string `json:"code"`
}

type runResponse struct {
	OK         bool   `json:"ok"`
	Stdout     string `json:"stdout"`
	Stderr     string `json:"stderr"`
	ExitCode   int    `json:"exitCode"`
	DurationMs int64  `json:"durationMs"`
}

// sharedGoCache holds compiled stdlib package objects so each /run request
// doesn't pay a full cold-cache compile; it never stores user source.
var sharedGoCache string

func main() {
	token := os.Getenv("RUNNER_TOKEN")
	if token == "" {
		log.Fatal("RUNNER_TOKEN env var is required")
	}

	var err error
	sharedGoCache, err = os.MkdirTemp("", "golearn-gocache-*")
	if err != nil {
		log.Fatalf("failed to create shared go cache: %v", err)
	}

	// A cold GOCACHE makes the first real /run request pay for compiling
	// the whole fmt/os/reflect dependency graph, which can blow past the
	// per-request timeout. Pay that cost once at boot instead.
	log.Print("warming go build cache...")
	warmup := execute("package main\nimport (\"fmt\"; \"os\"; \"strings\"; \"time\")\nfunc main(){fmt.Println(strings.ToUpper(\"warm\"), time.Now().Year(), os.Getpid())}\n")
	if !warmup.OK {
		log.Printf("cache warmup did not complete cleanly: %s", warmup.Stderr)
	} else {
		log.Print("go build cache warm")
	}

	mux := http.NewServeMux()
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		w.Write([]byte("ok"))
	})
	mux.HandleFunc("/run", func(w http.ResponseWriter, r *http.Request) {
		handleRun(w, r, token)
	})

	port := os.Getenv("PORT")
	if port == "" {
		port = "3000"
	}
	log.Printf("golearn-runner listening on :%s", port)
	log.Fatal(http.ListenAndServe("0.0.0.0:"+port, mux))
}

func handleRun(w http.ResponseWriter, r *http.Request, token string) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	if r.Header.Get("X-Runner-Token") != token {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}

	var req runRequest
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, maxSourceBytes+1024)).Decode(&req); err != nil {
		http.Error(w, "invalid request body", http.StatusBadRequest)
		return
	}
	if len(req.Code) == 0 {
		http.Error(w, "code is required", http.StatusBadRequest)
		return
	}
	if len(req.Code) > maxSourceBytes {
		http.Error(w, "code exceeds max size", http.StatusRequestEntityTooLarge)
		return
	}

	resp := execute(req.Code)
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(resp)
}

func execute(code string) runResponse {
	workDir, err := os.MkdirTemp("", "golearn-run-*")
	if err != nil {
		return runResponse{OK: false, Stderr: "internal error: failed to create sandbox"}
	}
	defer os.RemoveAll(workDir)

	if err := os.WriteFile(filepath.Join(workDir, "main.go"), []byte(code), 0o600); err != nil {
		return runResponse{OK: false, Stderr: "internal error: failed to write source"}
	}
	goModContent := "module sandbox\n\ngo 1.22\n"
	if err := os.WriteFile(filepath.Join(workDir, "go.mod"), []byte(goModContent), 0o600); err != nil {
		return runResponse{OK: false, Stderr: "internal error: failed to init module"}
	}

	ctx, cancel := context.WithTimeout(context.Background(), runTimeout)
	defer cancel()

	// We deliberately avoid shell-level ulimits here: -v (RLIMIT_AS) kills
	// legitimate compiles because the go runtime reserves huge unbacked
	// virtual address ranges, -u (RLIMIT_NPROC) counts threads for the
	// whole UID rather than this process tree, and -t (RLIMIT_CPU) sums
	// CPU-seconds across every thread the parallel compiler spawns, so it
	// can blow past a "5 second" budget in under 2 seconds of wall time.
	// The real defense against runaway/fork-bombing code is the
	// wall-clock context timeout below, which SIGKILLs the entire process
	// group regardless of what it spawned.
	cmd := exec.CommandContext(ctx, "/bin/sh", "-c", "exec go run .")
	cmd.Dir = workDir
	cmd.Env = append(sandboxBaseEnv(),
		"HOME="+workDir,
		"GOPATH="+filepath.Join(workDir, "gopath"),
		"GOCACHE="+sharedGoCache,
		"GOPROXY=off",
		"GOFLAGS=-mod=mod",
		"GOTOOLCHAIN=local",
		"CGO_ENABLED=0",
	)
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}

	// "go run" execs into the shell's PID but then forks the compiled
	// binary as a *child* of that process, inheriting the stdout/stderr
	// pipes. The default context-cancel behavior only kills the single
	// tracked process (the "go run" wrapper) — an orphaned infinite-loop
	// binary would keep the pipe write-end open forever, and cmd.Wait()
	// blocks until pipes hit EOF, hanging the request past the timeout.
	// Killing the whole process group on cancel takes the child with it.
	cmd.Cancel = func() error {
		if cmd.Process == nil {
			return nil
		}
		return syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
	}
	// Backstop in case something outside the group still holds a pipe
	// open (e.g. a reparented process) — force-close pipes so Wait()
	// can't hang forever even then.
	cmd.WaitDelay = 2 * time.Second

	var stdout, stderr limitedBuffer
	stdout.limit = maxOutputBytes
	stderr.limit = maxOutputBytes
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr

	start := time.Now()
	runErr := cmd.Run()
	duration := time.Since(start)

	if ctx.Err() == context.DeadlineExceeded {
		return runResponse{
			OK:         false,
			Stdout:     stdout.String(),
			Stderr:     stderr.String() + "\n[timed out after 5s]",
			ExitCode:   -1,
			DurationMs: duration.Milliseconds(),
		}
	}

	exitCode := 0
	if runErr != nil {
		if exitErr, ok := runErr.(*exec.ExitError); ok {
			exitCode = exitErr.ExitCode()
		} else {
			exitCode = -1
		}
	}

	return runResponse{
		OK:         runErr == nil,
		Stdout:     stdout.String(),
		Stderr:     stderr.String(),
		ExitCode:   exitCode,
		DurationMs: duration.Milliseconds(),
	}
}

// limitedBuffer caps writes to `limit` bytes, silently dropping the rest.
type limitedBuffer struct {
	buf   bytes.Buffer
	limit int
}

func (l *limitedBuffer) Write(p []byte) (int, error) {
	remaining := l.limit - l.buf.Len()
	if remaining <= 0 {
		return len(p), nil
	}
	if len(p) > remaining {
		l.buf.Write(p[:remaining])
		l.buf.WriteString("\n[output truncated at " + strconv.Itoa(l.limit) + " bytes]")
		return len(p), nil
	}
	l.buf.Write(p)
	return len(p), nil
}

func (l *limitedBuffer) String() string {
	return l.buf.String()
}

// sandboxBaseEnv returns the server's environment with secrets stripped.
// User code runs with os.Getenv access, so RUNNER_TOKEN must never reach it
// — otherwise a learner could print it and call /run directly, unmetered.
func sandboxBaseEnv() []string {
	env := os.Environ()
	filtered := make([]string, 0, len(env))
	for _, kv := range env {
		if strings.HasPrefix(kv, "RUNNER_TOKEN=") {
			continue
		}
		filtered = append(filtered, kv)
	}
	return filtered
}
