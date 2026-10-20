// Package main implements a minimal static initialization binary that discovers
// Docker secret files with the "gatus_" prefix and maps them into environment
// variables before executing the Gatus binary.
package main

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"
)

const (
	defaultBinaryPath = "/gatus"
	defaultCommand    = ""
	defaultSecretsDir = "/run/secrets"
	defaultWaitDelay  = 5 * time.Second
	secretPrefix      = "gatus_"
)

var (
	errInvalidSecretKey = errors.New("invalid characters in secret environment key")
	errMockExec         = errors.New("simulated execution failure")
	errMockReadDir      = errors.New("simulated readdir failure")
	errMockReadFile     = errors.New("simulated readfile failure")
)

func defaultReadFile(path string) ([]byte, error) {
	cleanPath := filepath.Clean(path)
	data, err := os.ReadFile(cleanPath)
	if err != nil {
		return nil, fmt.Errorf("reading secret file %q: %w", cleanPath, err)
	}
	return data, nil
}

func defaultReadDir(dirname string) ([]fs.DirEntry, error) {
	cleanDir := filepath.Clean(dirname)
	entries, err := os.ReadDir(cleanDir)
	if err != nil {
		return nil, fmt.Errorf("reading secrets directory %q: %w", cleanDir, err)
	}
	return entries, nil
}

func cancelCommand(cmd *exec.Cmd) func() error {
	return func() error {
		if cmd.Process != nil {
			return cmd.Process.Signal(syscall.SIGTERM)
		}
		return nil
	}
}

func defaultCreateCommand(ctx context.Context, args []string) *exec.Cmd {
	cmd := exec.CommandContext(ctx, defaultBinaryPath)
	if len(args) > 0 {
		cmd.Args = append([]string{defaultBinaryPath}, args...)
	}
	cmd.Cancel = cancelCommand(cmd)
	cmd.WaitDelay = defaultWaitDelay
	return cmd
}

func defaultExec(ctx context.Context, args, env []string) error {
	cmd := createCommand(ctx, args)
	cmd.Env = env
	cmd.Stdin = os.Stdin
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr

	if err := cmd.Run(); err != nil {
		return fmt.Errorf("running target binary: %w", err)
	}
	return nil
}

var (
	createCommand = defaultCreateCommand
	execProcess   = defaultExec
	exitFunc      = os.Exit
	readDir       = defaultReadDir
	readFile      = defaultReadFile
)

// PrepareEnvironment scans the specified secrets directory for files matching
// the application prefix and merges their contents into the base environment
// so container inspection metadata does not expose sensitive credentials.
func PrepareEnvironment(baseEnv []string, secretsDir string) ([]string, error) {
	entries, err := readDir(secretsDir)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return baseEnv, nil
		}
		return nil, fmt.Errorf("scanning secrets directory: %w", err)
	}

	discovered := make([]string, 0, len(entries))
	for _, entry := range entries {
		if entry.IsDir() {
			continue
		}

		name := entry.Name()
		lower := strings.ToLower(name)
		if !strings.HasPrefix(lower, secretPrefix) || len(name) <= len(secretPrefix) {
			continue
		}

		key := strings.ToUpper(name)
		if strings.ContainsAny(key, "= \t\r\n") {
			return nil, fmt.Errorf("key %q: %w", key, errInvalidSecretKey)
		}

		secretPath := filepath.Join(secretsDir, name)
		data, readErr := readFile(secretPath)
		if readErr != nil {
			return nil, fmt.Errorf("loading secret %q: %w", name, readErr)
		}

		val := strings.TrimSpace(string(data))
		if val == "" {
			continue
		}
		discovered = append(discovered, fmt.Sprintf("%s=%s", key, val))
	}

	env := make([]string, 0, len(baseEnv)+len(discovered))
	env = append(env, baseEnv...)
	env = append(env, discovered...)

	return env, nil
}

// BuildCommandArguments constructs the execution argument vector, defaulting
// to the service daemon command when no explicit subcommands are provided.
func BuildCommandArguments(defaultCmd string, args []string) []string {
	if len(args) == 0 {
		if defaultCmd == "" {
			return []string{}
		}
		return []string{defaultCmd}
	}

	if args[0] == defaultBinaryPath {
		if len(args) == 1 {
			if defaultCmd == "" {
				return []string{}
			}
			return []string{defaultCmd}
		}
		res := make([]string, len(args)-1)
		copy(res, args[1:])
		return res
	}

	res := make([]string, len(args))
	copy(res, args)
	return res
}

// IsTermination determines whether a command termination was initiated by
// intentional orchestration shutdown signals to prevent benign shutdowns
// from being misclassified as container runtime failures.
func IsTermination(ctx context.Context, err error) bool {
	if errors.Is(ctx.Err(), context.Canceled) {
		return true
	}
	if exitErr, ok := errors.AsType[*exec.ExitError](err); ok {
		if status, statusOk := exitErr.Sys().(syscall.WaitStatus); statusOk && status.Signaled() {
			sig := status.Signal()
			return sig == syscall.SIGTERM || sig == syscall.SIGINT || sig == syscall.SIGKILL
		}
	}
	return false
}

// ExitCode extracts the process exit code from an execution error to ensure
// container health checks and parent process status monitors receive accurate termination signals.
func ExitCode(err error) int {
	if err == nil {
		return 0
	}
	if exitErr, ok := errors.AsType[*exec.ExitError](err); ok {
		if status, statusOk := exitErr.Sys().(syscall.WaitStatus); statusOk && status.Signaled() {
			sig := status.Signal()
			if sig == syscall.SIGTERM || sig == syscall.SIGINT {
				return 0
			}
			return 128 + int(sig)
		}
		return exitErr.ExitCode()
	}
	return 1
}

// Run executes the initialization workflow by discovering secrets, preparing
// the runtime environment, and launching the application binary.
func Run(ctx context.Context, args []string, secretsDir string) error {
	env, err := PrepareEnvironment(os.Environ(), secretsDir)
	if err != nil {
		return fmt.Errorf("preparing runtime environment: %w", err)
	}

	cmdArgs := BuildCommandArguments(defaultCommand, args)
	if execErr := execProcess(ctx, cmdArgs, env); execErr != nil {
		return fmt.Errorf("transferring execution to binary: %w", execErr)
	}

	return nil
}

func main() {
	var args []string
	if len(os.Args) > 1 {
		args = os.Args[1:]
	}

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	if err := Run(ctx, args, defaultSecretsDir); err != nil {
		if !IsTermination(ctx, err) {
			fmt.Fprintf(os.Stderr, "init error: %v\n", err)
		}
		if IsTermination(ctx, err) {
			exitFunc(0)
			return
		}
		exitFunc(ExitCode(err))
	}
}
