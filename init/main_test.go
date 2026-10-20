package main

import (
	"bytes"
	"context"
	"errors"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"slices"
	"testing"
)

func mockFailingReadDir(_ string) ([]fs.DirEntry, error) {
	return nil, errMockReadDir
}

func mockFailingReadFile(_ string) ([]byte, error) {
	return nil, errMockReadFile
}

func mockSuccessfulExec(_ context.Context, _, _ []string) error {
	return nil
}

func mockFailingExec(_ context.Context, _, _ []string) error {
	return errMockExec
}

func mockSuccessfulCommand(ctx context.Context, _ []string) *exec.Cmd {
	return exec.CommandContext(ctx, "true")
}

func mockTerminatedExec(ctx context.Context, _, _ []string) error {
	return exec.CommandContext(ctx, "sh", "-c", "kill -TERM $$").Run()
}

var recordedExitCode = -1

func mockExitRecorder(code int) {
	recordedExitCode = code
}

var (
	capturedArgs []string
	capturedEnv  []string
)

func mockRecordingExec(_ context.Context, args, env []string) error {
	capturedArgs = args
	capturedEnv = env
	return nil
}

func mockNotExistReadDir(_ string) ([]fs.DirEntry, error) {
	return nil, fs.ErrNotExist
}

func TestDefaultReadFile(t *testing.T) {
	tempDir := t.TempDir()
	filePath := filepath.Join(tempDir, "sample.txt")
	expectedContent := []byte("secret_value")

	if err := os.WriteFile(filePath, expectedContent, 0o600); err != nil {
		t.Fatalf("writing test file: %v", err)
	}

	data, err := defaultReadFile(filePath)
	if err != nil {
		t.Fatalf("defaultReadFile() unexpected error: %v", err)
	}

	if !bytes.Equal(data, expectedContent) {
		t.Errorf("defaultReadFile() = %q, want %q", data, expectedContent)
	}

	if _, err := defaultReadFile(filepath.Join(tempDir, "nonexistent.txt")); err == nil {
		t.Error("defaultReadFile() expected error on nonexistent file, got nil")
	}
}

func TestDefaultReadDir(t *testing.T) {
	tempDir := t.TempDir()
	filePath := filepath.Join(tempDir, "entry.txt")
	if err := os.WriteFile(filePath, []byte("ok"), 0o600); err != nil {
		t.Fatalf("writing entry file: %v", err)
	}

	entries, err := defaultReadDir(tempDir)
	if err != nil {
		t.Fatalf("defaultReadDir() unexpected error: %v", err)
	}

	if len(entries) != 1 || entries[0].Name() != "entry.txt" {
		t.Errorf("defaultReadDir() unexpected entries: %v", entries)
	}

	if _, err := defaultReadDir(filepath.Join(tempDir, "missing_subdir")); err == nil {
		t.Error("defaultReadDir() expected error on nonexistent directory, got nil")
	}
}

func TestDefaultCreateCommand(t *testing.T) {
	ctx := context.Background()

	tests := []struct {
		name     string
		args     []string
		wantCmd  string
		wantArgs []string
	}{
		{
			name:     "empty args invokes default binary without extra arguments",
			args:     nil,
			wantCmd:  defaultBinaryPath,
			wantArgs: []string{defaultBinaryPath},
		},
		{
			name:     "custom flag args are passed directly",
			args:     []string{"--debug"},
			wantCmd:  defaultBinaryPath,
			wantArgs: []string{defaultBinaryPath, "--debug"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cmd := defaultCreateCommand(ctx, tt.args)
			if cmd.Path != tt.wantCmd && !slices.Contains(cmd.Args, tt.wantCmd) {
				t.Errorf("unexpected command path: %s", cmd.Path)
			}
			if !slices.Equal(cmd.Args, tt.wantArgs) {
				t.Errorf("defaultCreateCommand() args = %v, want %v", cmd.Args, tt.wantArgs)
			}
		})
	}

	t.Run("cancel command returns nil when process not started", func(t *testing.T) {
		cmd := defaultCreateCommand(ctx, nil)
		if cmd.Cancel == nil {
			t.Fatal("expected Cancel function to be configured")
		}
		if err := cmd.Cancel(); err != nil {
			t.Errorf("cmd.Cancel() unexpected error: %v", err)
		}
	})

	t.Run("cancel command signals running process", func(t *testing.T) {
		cmd := exec.CommandContext(ctx, "sleep", "10")
		cancelFn := cancelCommand(cmd)
		if err := cmd.Start(); err != nil {
			t.Fatalf("cmd.Start() unexpected error: %v", err)
		}
		defer func() {
			if waitErr := cmd.Wait(); waitErr != nil && !IsTermination(ctx, waitErr) {
				t.Errorf("cmd.Wait() unexpected error: %v", waitErr)
			}
		}()
		if err := cancelFn(); err != nil {
			t.Errorf("cancelFn() unexpected error: %v", err)
		}
	})
}

func TestDefaultExec(t *testing.T) {
	origCreateCommand := createCommand
	defer func() { createCommand = origCreateCommand }()

	ctx := context.Background()

	tests := []struct {
		cmdFunc     func(context.Context, []string) *exec.Cmd
		name        string
		expectError bool
	}{
		{
			name:        "successful execution",
			cmdFunc:     mockSuccessfulCommand,
			expectError: false,
		},
		{
			name: "missing binary execution failure",
			cmdFunc: func(ctx context.Context, _ []string) *exec.Cmd {
				return exec.CommandContext(ctx, "/path/that/does/not/exist")
			},
			expectError: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			createCommand = tt.cmdFunc
			err := defaultExec(ctx, []string{"test"}, []string{"FOO=BAR"})
			if tt.expectError && err == nil {
				t.Errorf("defaultExec() expected error, got nil")
			}
			if !tt.expectError && err != nil {
				t.Errorf("defaultExec() unexpected error: %v", err)
			}
		})
	}
}

func TestPrepareEnvironment_Errors(t *testing.T) {
	origReadDir := readDir
	origReadFile := readFile
	defer func() {
		readDir = origReadDir
		readFile = origReadFile
	}()

	tempDir := t.TempDir()

	tests := []struct {
		setup       func()
		expectedErr error
		name        string
		targetDir   string
	}{
		{
			expectedErr: errMockReadDir,
			name:        "readDir error propagates",
			setup: func() {
				readDir = mockFailingReadDir
			},
			targetDir: tempDir,
		},
		{
			expectedErr: nil,
			name:        "missing directory returns base environment",
			setup: func() {
				readDir = origReadDir
			},
			targetDir: filepath.Join(tempDir, "absent_dir"),
		},
		{
			expectedErr: errMockReadFile,
			name:        "readFile error propagates",
			setup: func() {
				readDir = origReadDir
				readFile = mockFailingReadFile
				if err := os.WriteFile(filepath.Join(tempDir, "gatus_token"), []byte("value"), 0o600); err != nil {
					t.Fatalf("writing test secret file: %v", err)
				}
			},
			targetDir: tempDir,
		},
		{
			expectedErr: errInvalidSecretKey,
			name:        "invalid secret key returns error",
			setup: func() {
				readDir = origReadDir
				readFile = origReadFile
				if err := os.WriteFile(filepath.Join(tempDir, "gatus_bad=key"), []byte("val"), 0o600); err != nil {
					t.Fatalf("writing test secret file: %v", err)
				}
			},
			targetDir: tempDir,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			tt.setup()
			res, err := PrepareEnvironment([]string{"BASE=1"}, tt.targetDir)
			if tt.expectedErr != nil {
				if !errors.Is(err, tt.expectedErr) {
					t.Fatalf("expected error %v, got %v", tt.expectedErr, err)
				}
			} else {
				if err != nil {
					t.Fatalf("unexpected error: %v", err)
				}
				if !slices.Contains(res, "BASE=1") {
					t.Errorf("expected base env preserved, got %v", res)
				}
			}
		})
	}
}

func TestPrepareEnvironment_Scanning(t *testing.T) {
	origReadDir := readDir
	origReadFile := readFile
	defer func() {
		readDir = origReadDir
		readFile = origReadFile
	}()

	readDir = defaultReadDir
	readFile = defaultReadFile

	tempDir := t.TempDir()

	if err := os.Mkdir(filepath.Join(tempDir, "gatus_directory"), 0o700); err != nil {
		t.Fatalf("creating test subdir: %v", err)
	}

	files := []struct {
		filename string
		content  string
	}{
		{filename: "ignored_file.txt", content: "ignore"},
		{filename: "gatus_", content: "empty_key"},
		{filename: "gatus_empty_val", content: "   \n\t  "},
		{filename: "gatus_config_path", content: "/config/config.yaml\n"},
		{filename: "gatus_log_level", content: "info"},
	}

	for _, f := range files {
		if err := os.WriteFile(filepath.Join(tempDir, f.filename), []byte(f.content), 0o600); err != nil {
			t.Fatalf("creating test file %s: %v", f.filename, err)
		}
	}

	baseEnv := []string{"INITIAL=VAL"}
	res, err := PrepareEnvironment(baseEnv, tempDir)
	if err != nil {
		t.Fatalf("PrepareEnvironment() unexpected error: %v", err)
	}

	expectedMatches := []string{
		"INITIAL=VAL",
		"GATUS_CONFIG_PATH=/config/config.yaml",
		"GATUS_LOG_LEVEL=info",
	}

	for _, match := range expectedMatches {
		if !slices.Contains(res, match) {
			t.Errorf("missing expected environment entry %q in %v", match, res)
		}
	}

	unexpectedPrefixes := []string{
		"GATUS_DIRECTORY",
		"IGNORED_FILE",
		"GATUS_EMPTY_VAL",
		"GATUS_=",
	}

	for _, prefix := range unexpectedPrefixes {
		for _, envEntry := range res {
			if slices.Contains([]string{envEntry}, prefix) {
				t.Errorf("unexpected environment entry found: %s", envEntry)
			}
		}
	}
}

func TestBuildCommandArguments(t *testing.T) {
	tests := []struct {
		name       string
		defaultCmd string
		args       []string
		want       []string
	}{
		{
			name:       "empty input produces empty args when default command is empty",
			defaultCmd: "",
			args:       nil,
			want:       []string{},
		},
		{
			name:       "empty input produces default command when set",
			defaultCmd: "daemon",
			args:       nil,
			want:       []string{"daemon"},
		},
		{
			name:       "custom command arguments are returned directly",
			defaultCmd: "",
			args:       []string{"--config", "/config/config.yaml"},
			want:       []string{"--config", "/config/config.yaml"},
		},
		{
			name:       "explicit binary argument is stripped and returns subcommands",
			defaultCmd: "",
			args:       []string{defaultBinaryPath, "--port", "8080"},
			want:       []string{"--port", "8080"},
		},
		{
			name:       "explicit binary argument alone falls back to empty when default command is empty",
			defaultCmd: "",
			args:       []string{defaultBinaryPath},
			want:       []string{},
		},
		{
			name:       "explicit binary argument alone falls back to default command when set",
			defaultCmd: "start",
			args:       []string{defaultBinaryPath},
			want:       []string{"start"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := BuildCommandArguments(tt.defaultCmd, tt.args)
			if !slices.Equal(got, tt.want) {
				t.Errorf("BuildCommandArguments() = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestExitCode(t *testing.T) {
	tests := []struct {
		err      error
		name     string
		expected int
	}{
		{
			err:      nil,
			expected: 0,
			name:     "nil error returns zero exit code",
		},
		{
			err:      errors.New("generic failure"),
			expected: 1,
			name:     "generic error returns exit code one",
		},
		{
			err: func() error {
				cmd := exec.CommandContext(t.Context(), "sh", "-c", "exit 42")
				return cmd.Run()
			}(),
			expected: 42,
			name:     "exit error extracts subprocess status code",
		},
		{
			err:      exec.CommandContext(t.Context(), "sh", "-c", "kill -TERM $$").Run(),
			expected: 0,
			name:     "termination signal returns zero exit code",
		},
		{
			err:      exec.CommandContext(t.Context(), "sh", "-c", "kill -INT $$").Run(),
			expected: 0,
			name:     "interrupt signal returns zero exit code",
		},
		{
			err:      exec.CommandContext(t.Context(), "sh", "-c", "kill -ABRT $$").Run(),
			expected: 134,
			name:     "other signal returns 128 plus signal code",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := ExitCode(tt.err); got != tt.expected {
				t.Errorf("ExitCode(%v) = %d, want %d", tt.err, got, tt.expected)
			}
		})
	}
}

func TestIsTermination(t *testing.T) {
	canceledCtx, cancel := context.WithCancel(t.Context())
	cancel()

	tests := []struct {
		ctx  context.Context
		err  error
		name string
		want bool
	}{
		{
			ctx:  t.Context(),
			err:  nil,
			name: "nil error with active context",
			want: false,
		},
		{
			ctx:  t.Context(),
			err:  errors.New("standard error"),
			name: "standard error with active context",
			want: false,
		},
		{
			ctx:  canceledCtx,
			err:  context.Canceled,
			name: "canceled context error",
			want: true,
		},
		{
			ctx:  t.Context(),
			err:  exec.CommandContext(t.Context(), "sh", "-c", "kill -TERM $$").Run(),
			name: "subprocess killed by sigterm",
			want: true,
		},
		{
			ctx:  t.Context(),
			err:  exec.CommandContext(t.Context(), "sh", "-c", "kill -INT $$").Run(),
			name: "subprocess killed by sigint",
			want: true,
		},
		{
			ctx:  t.Context(),
			err:  exec.CommandContext(t.Context(), "sh", "-c", "kill -KILL $$").Run(),
			name: "subprocess killed by sigkill",
			want: true,
		},
		{
			ctx:  t.Context(),
			err:  exec.CommandContext(t.Context(), "false").Run(),
			name: "subprocess non-signal failure",
			want: false,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := IsTermination(tc.ctx, tc.err); got != tc.want {
				t.Errorf("IsTermination() = %v, want %v", got, tc.want)
			}
		})
	}
}

func TestRun(t *testing.T) {
	origReadDir := readDir
	origReadFile := readFile
	origExecProcess := execProcess
	defer func() {
		readDir = origReadDir
		readFile = origReadFile
		execProcess = origExecProcess
	}()

	tempDir := t.TempDir()
	readDir = defaultReadDir
	readFile = defaultReadFile

	capturedArgs = nil
	capturedEnv = nil

	tests := []struct {
		execFunc    func(context.Context, []string, []string) error
		readDirFunc func(string) ([]fs.DirEntry, error)
		name        string
		expectError bool
	}{
		{
			name:        "successful execution passes arguments and environment",
			execFunc:    mockRecordingExec,
			readDirFunc: defaultReadDir,
			expectError: false,
		},
		{
			name:        "environment preparation error propagates failure",
			execFunc:    mockRecordingExec,
			readDirFunc: mockFailingReadDir,
			expectError: true,
		},
		{
			name:        "process execution failure propagates error",
			execFunc:    mockFailingExec,
			readDirFunc: defaultReadDir,
			expectError: true,
		},
	}

	runArgs := []string{"--dry-run"}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			execProcess = tt.execFunc
			readDir = tt.readDirFunc

			err := Run(context.Background(), runArgs, tempDir)
			if tt.expectError && err == nil {
				t.Fatalf("Run() expected error, got nil")
			}
			if !tt.expectError && err != nil {
				t.Fatalf("Run() unexpected error: %v", err)
			}
			if !tt.expectError {
				if !reflect.DeepEqual(capturedArgs, runArgs) {
					t.Errorf("Run() args = %v, want %v", capturedArgs, runArgs)
				}
				if len(capturedEnv) == 0 {
					t.Errorf("Run() expected non-empty environment")
				}
			}
		})
	}
}

func TestMain_Success(t *testing.T) {
	origExecProcess := execProcess
	origReadDir := readDir
	origReadFile := readFile
	origExitFunc := exitFunc
	origArgs := os.Args
	defer func() {
		execProcess = origExecProcess
		readDir = origReadDir
		readFile = origReadFile
		exitFunc = origExitFunc
		os.Args = origArgs
	}()

	execProcess = mockSuccessfulExec
	readDir = mockNotExistReadDir
	recordedExitCode = -1
	exitFunc = mockExitRecorder
	os.Args = []string{"init", "--version"}

	main()

	if recordedExitCode != -1 {
		t.Errorf("main() unexpected exit code: %d", recordedExitCode)
	}
}

func TestMain_Failure(t *testing.T) {
	origExecProcess := execProcess
	origReadDir := readDir
	origReadFile := readFile
	origExitFunc := exitFunc
	defer func() {
		execProcess = origExecProcess
		readDir = origReadDir
		readFile = origReadFile
		exitFunc = origExitFunc
	}()

	readDir = mockFailingReadDir
	recordedExitCode = -1
	exitFunc = mockExitRecorder

	main()

	if recordedExitCode != 1 {
		t.Errorf("main() expected exit code 1 on failure, got %d", recordedExitCode)
	}
}

func TestMain_Termination(t *testing.T) {
	origExecProcess := execProcess
	origReadDir := readDir
	origReadFile := readFile
	origExitFunc := exitFunc
	defer func() {
		execProcess = origExecProcess
		readDir = origReadDir
		readFile = origReadFile
		exitFunc = origExitFunc
	}()

	execProcess = mockTerminatedExec
	readDir = defaultReadDir
	recordedExitCode = -1
	exitFunc = mockExitRecorder

	main()

	if recordedExitCode != 0 {
		t.Errorf("main() on termination expected exit code 0, got %d", recordedExitCode)
	}
}
