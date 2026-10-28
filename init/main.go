// Package main implements a minimal static initialization binary that discovers
// Docker secret files with the "gatus_" prefix and maps them into environment
// variables before executing the Gatus binary.
package main

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"net"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"runtime"
	"strconv"
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

const (
	defaultLoopbackAddress  = "127.0.0.1"
	defaultServerPort       = "8080"
	defaultProcDir          = "/proc"
	defaultReadinessTimeout = 30 * time.Second
	childReadinessInterval  = 50 * time.Millisecond
	maxEnvRangeSize         = 32 * 1024 * 1024
	minStatFieldsAfterComm  = 49
	statEnvStartIndex       = 47
	statEnvEndIndex         = 48
)

var (
	errInvalidSecretKey = errors.New("invalid characters in secret environment key")
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

func defaultCloseFile(f *os.File) error {
	if err := f.Close(); err != nil {
		return fmt.Errorf("closing file: %w", err)
	}
	return nil
}

func defaultWriteMemory(f *os.File, b []byte, off int64) (int, error) {
	n, err := f.WriteAt(b, off)
	if err != nil {
		return n, fmt.Errorf("writing memory: %w", err)
	}
	return n, nil
}

func defaultOpenFile(name string, flag int, perm os.FileMode) (*os.File, error) {
	cleanPath := filepath.Clean(name)
	file, err := os.OpenFile(cleanPath, flag, perm)
	if err != nil {
		return nil, fmt.Errorf("opening file %q: %w", cleanPath, err)
	}
	return file, nil
}

type socketDialer interface {
	DialContext(ctx context.Context, network, address string) (net.Conn, error)
}

func defaultCreateDialer() socketDialer {
	return &net.Dialer{}
}

func defaultDialTCP(ctx context.Context, network, address string) (net.Conn, error) {
	d := createDialer()
	conn, err := d.DialContext(ctx, network, address)
	if err != nil {
		return nil, fmt.Errorf("dialing %s %s: %w", network, address, err)
	}
	return conn, nil
}

func defaultProcPath(pid int, entry string) string {
	return filepath.Join(defaultProcDir, strconv.Itoa(pid), entry)
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

func parseProcStat(statContent string) (envStart, envEnd int64, err error) {
	_, remainder, found := strings.CutLast(statContent, ")")
	if !found {
		return 0, 0, errors.New("missing closing parenthesis for comm in stat content")
	}

	fields := strings.Fields(remainder)
	if len(fields) < minStatFieldsAfterComm {
		return 0, 0, fmt.Errorf("stat fields count %d below required minimum %d", len(fields), minStatFieldsAfterComm)
	}

	envStart, err = strconv.ParseInt(fields[statEnvStartIndex], 10, 64)
	if err != nil {
		return 0, 0, fmt.Errorf("parsing env_start from stat: %w", err)
	}

	envEnd, err = strconv.ParseInt(fields[statEnvEndIndex], 10, 64)
	if err != nil {
		return 0, 0, fmt.Errorf("parsing env_end from stat: %w", err)
	}

	if envStart <= 0 || envEnd <= envStart {
		return 0, 0, fmt.Errorf("invalid environment boundaries: env_start (%d) >= env_end (%d)", envStart, envEnd)
	}

	if envEnd-envStart > maxEnvRangeSize {
		return 0, 0, fmt.Errorf("environment range size %d exceeds limit of %d bytes", envEnd-envStart, maxEnvRangeSize)
	}

	return envStart, envEnd, nil
}

func waitForChildReadiness(ctx context.Context, addr string, timeout time.Duration) error {
	waitCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	for {
		if err := ctx.Err(); err != nil {
			return fmt.Errorf("readiness context canceled: %w", err)
		}
		conn, err := dialTCP(waitCtx, "tcp", addr)
		if err == nil {
			if closeErr := conn.Close(); closeErr != nil {
				return fmt.Errorf("closing readiness socket: %w", closeErr)
			}
			return nil
		}
		timer := time.NewTimer(childReadinessInterval)
		select {
		case <-waitCtx.Done():
			timer.Stop()
			if errors.Is(waitCtx.Err(), context.DeadlineExceeded) {
				return nil
			}
			return fmt.Errorf("readiness context canceled: %w", ctx.Err())
		case <-timer.C:
		}
	}
}

func isIgnorableMemoryError(err error) bool {
	return errors.Is(err, fs.ErrNotExist) || errors.Is(err, fs.ErrPermission)
}

func defaultScrubChildMemory(ctx context.Context, pid int) (err error) {
	targetAddr := net.JoinHostPort(defaultLoopbackAddress, defaultServerPort)
	if readinessErr := waitForChildReadiness(ctx, targetAddr, defaultReadinessTimeout); readinessErr != nil {
		if isIgnorableMemoryError(readinessErr) || errors.Is(readinessErr, context.Canceled) {
			return nil
		}
		return fmt.Errorf("synchronizing child readiness: %w", readinessErr)
	}

	statPath := procPath(pid, "stat")
	statBytes, readErr := readFile(statPath)
	if readErr != nil {
		if isIgnorableMemoryError(readErr) {
			return nil
		}
		return fmt.Errorf("reading stat file %q: %w", statPath, readErr)
	}

	envStart, envEnd, parseErr := parseProcStat(string(statBytes))
	if parseErr != nil {
		return fmt.Errorf("parsing stat file %q: %w", statPath, parseErr)
	}

	memPath := procPath(pid, "mem")
	memFile, openErr := openFile(memPath, os.O_RDWR, 0)
	if openErr != nil {
		if isIgnorableMemoryError(openErr) {
			return nil
		}
		return fmt.Errorf("opening mem file %q: %w", memPath, openErr)
	}
	defer func() {
		if closeErr := closeFile(memFile); closeErr != nil && err == nil && !isIgnorableMemoryError(closeErr) {
			err = fmt.Errorf("closing mem file %q: %w", memPath, closeErr)
		}
	}()

	zeros := make([]byte, envEnd-envStart)
	if _, writeErr := writeMemory(memFile, zeros, envStart); writeErr != nil {
		if isIgnorableMemoryError(writeErr) {
			return nil
		}
		return fmt.Errorf("zeroing memory in %q: %w", memPath, writeErr)
	}

	return nil
}

func defaultExec(ctx context.Context, args, env []string) error {
	cmd := createCommand(ctx, args)
	cmd.Env = env
	cmd.Stdin = os.Stdin
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr

	if err := cmd.Start(); err != nil {
		return fmt.Errorf("starting target binary %q: %w", defaultBinaryPath, err)
	}

	pid := cmd.Process.Pid
	scrubCtx, cancelScrub := context.WithCancel(ctx)
	defer cancelScrub()

	scrubDone := make(chan struct{})
	go func() {
		defer close(scrubDone)
		if scrubErr := scrubChildMemory(scrubCtx, pid); scrubErr != nil {
			fmt.Fprintf(os.Stderr, "memory scrubbing failed: %v\n", scrubErr)
		}
	}()

	cmd.Env = nil
	clear(env)
	runtime.GC()

	waitErr := cmd.Wait()
	cancelScrub()
	<-scrubDone

	if waitErr != nil {
		return fmt.Errorf("running target binary: %w", waitErr)
	}
	return nil
}

var (
	closeFile        = defaultCloseFile
	createCommand    = defaultCreateCommand
	createDialer     = defaultCreateDialer
	dialTCP          = defaultDialTCP
	execProcess      = defaultExec
	exitFunc         = os.Exit
	openFile         = defaultOpenFile
	procPath         = defaultProcPath
	readDir          = defaultReadDir
	readFile         = defaultReadFile
	scrubChildMemory = defaultScrubChildMemory
	writeMemory      = defaultWriteMemory
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
		clear(data)
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
