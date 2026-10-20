package terraform

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// Exercise the public apply and destroy paths with the real terraform-exec adapter. Only the
// external Terraform executable is replaced; no cloud resources are touched.
func TestModuleJSON(t *testing.T) {
	diagnostic := func(severity, summary, detail string) string {
		b, err := json.Marshal(map[string]any{
			"type": "diagnostic", "@message": summary,
			"diagnostic": map[string]string{"severity": severity, "summary": summary, "detail": detail},
		})
		require.NoError(t, err)
		return string(b) + "\n"
	}
	lock := func(created string) string {
		return diagnostic("error", "Error acquiring the state lock",
			"Lock Info:\n  ID: stale-lock\n  Path: bucket/state\n  Operation: OperationTypeApply\n"+
				"  Who: previous-runner\n  Version: 1.5.7\n  Created: "+created+"\n")
	}
	progress := "{\"type\":\"apply_complete\",\"@message\":\"Apply complete\"}\n"
	expired := lock("2000-01-01 00:00:00 +0000 UTC")
	tests := []struct {
		name        string
		output      string
		stderr      string
		exit        int
		parallelism int
		verbose     bool
		expiration  time.Duration
		retries     int
		wantUnlock  bool
		wantErr     string
		wantOutput  string
		retryOutput string
		retryExit   int
	}{
		{name: "default concurrency and quiet output", output: progress},
		{name: "verbose CLI progress", output: progress, verbose: true, wantOutput: "{\"terraform_version\":\"1.5.7\"}\ninitialized\n" + progress},
		{name: "explicit concurrency", output: progress, parallelism: 2},
		{name: "warning remains visible", output: diagnostic("warning", "Deprecated", "Use the new setting"),
			verbose: true, wantOutput: "{\"terraform_version\":\"1.5.7\"}\ninitialized\n" + diagnostic("warning", "Deprecated", "Use the new setting")},
		{name: "JSON diagnostics do not rewrite process errors", output: diagnostic("error", "Apply failed", "permission denied"), exit: 1, wantErr: "exit status 1"},
		{name: "stderr failure", stderr: "provider crashed", exit: 1, wantErr: "provider crashed"},
		{name: "expired lock unlocks and retries", output: expired, exit: 1, expiration: time.Minute, retries: 1, wantUnlock: true},
		{name: "successful command never unlocks", output: expired, expiration: time.Minute},
		{name: "lock metadata is isolated between attempts", output: expired, exit: 1, expiration: time.Minute, retries: 1, wantUnlock: true,
			retryOutput: diagnostic("error", "Apply failed", "permission denied"), retryExit: 1, wantErr: "exit status 1"},
		{name: "unterminated lock diagnostic", output: strings.TrimSuffix(expired, "\n"), exit: 1, expiration: time.Minute, retries: 1, wantUnlock: true},
		{name: "unrelated diagnostics cannot hide lock metadata", output: strings.Repeat(diagnostic("error", "Apply failed", strings.Repeat("detail ", 100)), 200) + expired,
			exit: 1, expiration: time.Minute, retries: 1, wantUnlock: true},
		{name: "large output cannot hide lock metadata", output: "{\"type\":\"outputs\",\"value\":\"" + strings.Repeat("x", 2<<20) + "\"}\n" + expired,
			exit: 1, expiration: time.Minute, retries: 1, wantUnlock: true},
		{name: "non-diagnostic event cannot unlock", output: strings.Replace(expired, "\"type\":\"diagnostic\"", "\"type\":\"outputs\"", 1),
			exit: 1, expiration: time.Minute, wantErr: "exit status 1"},
		{name: "warning cannot unlock", output: strings.Replace(expired, "\"severity\":\"error\"", "\"severity\":\"warning\"", 1),
			exit: 1, expiration: time.Minute, wantErr: "exit status 1"},
		{name: "active lock is retained", output: lock(time.Now().UTC().Format(lockCreatedLayout)), exit: 1, expiration: time.Hour, wantErr: "exit status 1"},
		{name: "disabled lock recovery", output: expired, exit: 1, wantErr: "exit status 1"},
		{name: "malformed lock is retained", output: lock("invalid timestamp"), exit: 1, expiration: time.Minute, wantErr: "exit status 1"},
		{name: "unrelated error never unlocks", output: diagnostic("error", "Apply failed", "permission denied"), exit: 1, expiration: time.Minute, wantErr: "exit status 1"},
		{name: "unterminated final diagnostic", output: strings.TrimSuffix(diagnostic("error", "Apply failed", "final detail"), "\n"), exit: 1, wantErr: "exit status 1"},
		{name: "large output does not hide later diagnostic", output: "{\"type\":\"outputs\",\"value\":\"" + strings.Repeat("x", 2<<20) + "\"}\n" +
			diagnostic("error", "Apply failed", "after large output"), exit: 1, wantErr: "exit status 1"},
		{name: "large diagnostic snippet preserves error", output: `{"type":"diagnostic","diagnostic":{"severity":"error","summary":"Invalid manifest","detail":"Schema validation failed","snippet":{"code":"` +
			strings.Repeat("x", 2<<20) + "\"}}}\n", exit: 1, wantErr: "exit status 1"},
		{name: "malformed output does not hide later diagnostic", output: "not JSON\n" + diagnostic("error", "Apply failed", "after malformed output"), exit: 1, wantErr: "exit status 1"},
		{name: "unrelated diagnostics are not retained", output: strings.Repeat(diagnostic("error", "Apply failed", strings.Repeat("detail ", 100)), 200), exit: 1, wantErr: "exit status 1"},
	}
	for _, operation := range []string{"apply", "destroy"} {
		for _, tt := range tests {
			t.Run(operation+"/"+tt.name, func(t *testing.T) {
				root := t.TempDir()
				require.NoError(t, os.Mkdir(filepath.Join(root, "module"), 0o750))
				script := `#!/bin/sh
case "$1" in
version) printf '{"terraform_version":"1.5.7"}\n';;
init) echo initialized;;
apply|destroy)
  printf '%s\n' "$@" >> arguments
  if [ -f unlocked ]; then cat retry.json; exit ` + fmt.Sprint(tt.retryExit) + `; fi
  cat output.json
  cat stderr.txt >&2
  exit ` + fmt.Sprint(tt.exit) + `
  ;;
force-unlock) printf '%s\n' "$@" >> unlocked;;
*) exit 2;;
esac
`
				binary := filepath.Join(root, "terraform")
				require.NoError(t, os.WriteFile(binary, []byte(script), 0o750))
				require.NoError(t, os.WriteFile(filepath.Join(root, "module", "output.json"), []byte(tt.output), 0o600))
				retryOutput := tt.retryOutput
				if retryOutput == "" {
					retryOutput = progress
				}
				require.NoError(t, os.WriteFile(filepath.Join(root, "module", "retry.json"), []byte(retryOutput), 0o600))
				require.NoError(t, os.WriteFile(filepath.Join(root, "module", "stderr.txt"), []byte(tt.stderr), 0o600))
				tf := &Terraform{rootPath: root, tfExecPath: binary, tfExecutorFactory: &defaultExecutorFactory{}}
				var stdout, stderr bytes.Buffer
				options := ApplyOptions{JSONOutput: true, Parallelism: tt.parallelism, Retry: tt.retries, RetryInterval: time.Millisecond, LockExpirationDuration: tt.expiration}
				if tt.verbose {
					options.StdOut, options.StdErr = &stdout, &stderr
				}
				moduleOptions := ModuleOptions{
					ModulePath: "module", BackendConfigFileName: "backend.tf", VariableFileName: "vars.tfvars",
				}
				var err error
				if operation == "apply" {
					err = tf.ApplyModule(context.Background(), moduleOptions, options)
				} else {
					err = tf.DestroyModule(context.Background(), moduleOptions, DestroyOptions{
						JSONOutput: true, Retry: options.Retry, RetryInterval: options.RetryInterval,
						LockExpirationDuration: options.LockExpirationDuration, StdOut: options.StdOut, StdErr: options.StdErr,
					})
				}
				if tt.wantErr == "" {
					require.NoError(t, err)
				} else {
					require.ErrorContains(t, err, tt.wantErr)
					var exitErr *exec.ExitError
					require.True(t, errors.As(err, &exitErr), "retain original process error")
					if tt.stderr == "" {
						assert.Equal(t, "code(internal) error applying terraform config: code(internal) failed to apply terraform config\nexit status 1\n", err.Error())
					}
				}
				assert.Equal(t, tt.wantOutput, stdout.String())
				args, err := os.ReadFile(filepath.Join(root, "module", "arguments"))
				require.NoError(t, err)
				assert.Contains(t, string(args), "\n-json\n")
				concurrency := tt.parallelism
				if concurrency == 0 || operation == "destroy" {
					concurrency = 10
				}
				assert.Contains(t, string(args), fmt.Sprintf("\n-parallelism=%d\n", concurrency))
				unlocked, err := os.ReadFile(filepath.Join(root, "module", "unlocked"))
				if tt.wantUnlock {
					require.NoError(t, err)
					assert.Equal(t, "force-unlock\n-no-color\n-force\nstale-lock\n", string(unlocked))
					assert.Equal(t, 2, strings.Count(string(args), operation+"\n"))
				} else {
					require.ErrorIs(t, err, os.ErrNotExist)
				}
			})
		}
	}
}

func TestModuleJSONCancellation(t *testing.T) {
	for _, operation := range []string{"apply", "destroy"} {
		t.Run(operation, func(t *testing.T) {
			root := t.TempDir()
			require.NoError(t, os.Mkdir(filepath.Join(root, "module"), 0o750))
			binary := filepath.Join(root, "terraform")
			require.NoError(t, os.WriteFile(binary, []byte(`#!/bin/sh
case "$1" in
version) printf '{"terraform_version":"1.5.7"}\n';;
init) exit 0;;
apply|destroy) touch started; exec sleep 30;;
esac
`), 0o750))
			tf := &Terraform{rootPath: root, tfExecPath: binary, tfExecutorFactory: &defaultExecutorFactory{}}
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			done := make(chan error, 1)
			go func() {
				moduleOptions := ModuleOptions{ModulePath: "module", BackendConfigFileName: "backend.tf", VariableFileName: "vars.tfvars"}
				if operation == "apply" {
					done <- tf.ApplyModule(ctx, moduleOptions, ApplyOptions{JSONOutput: true, GracefulShutdownPeriod: 100 * time.Millisecond})
				} else {
					done <- tf.DestroyModule(ctx, moduleOptions, DestroyOptions{JSONOutput: true, GracefulShutdownPeriod: 100 * time.Millisecond})
				}
			}()
			require.Eventually(t, func() bool {
				_, err := os.Stat(filepath.Join(root, "module", "started"))
				return err == nil
			}, 5*time.Second, 10*time.Millisecond)
			cancel()
			select {
			case err := <-done:
				require.ErrorIs(t, err, context.Canceled)
			case <-time.After(5 * time.Second):
				t.Fatal("apply did not stop after cancellation")
			}
		})
	}
}

func TestModuleOutputMode(t *testing.T) {
	for _, operation := range []string{"apply", "destroy"} {
		for _, jsonOutput := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/json=%t", operation, jsonOutput), func(t *testing.T) {
				root := t.TempDir()
				require.NoError(t, os.Mkdir(filepath.Join(root, "module"), 0o750))
				binary := filepath.Join(root, "terraform")
				require.NoError(t, os.WriteFile(binary, []byte(`#!/bin/sh
case "$1" in
version) printf '{"terraform_version":"1.5.7"}\n';;
init) exit 0;;
apply|destroy)
  printf '%s\n' "$@" > arguments
  case " $* " in
    *" -json "*) printf '{"type":"complete","@message":"done","extra":{"preserved":true}}\n';;
    *) printf 'Human-readable output\n';;
  esac;;
esac
`), 0o750))
				tf := &Terraform{rootPath: root, tfExecPath: binary, tfExecutorFactory: &defaultExecutorFactory{}}
				moduleOptions := ModuleOptions{ModulePath: "module", BackendConfigFileName: "backend.tf", VariableFileName: "vars.tfvars"}
				var stdout bytes.Buffer
				var err error
				if operation == "apply" {
					err = tf.ApplyModule(context.Background(), moduleOptions, ApplyOptions{JSONOutput: jsonOutput, StdOut: &stdout})
				} else {
					err = tf.DestroyModule(context.Background(), moduleOptions, DestroyOptions{JSONOutput: jsonOutput, StdOut: &stdout})
				}
				require.NoError(t, err)
				args, err := os.ReadFile(filepath.Join(root, "module", "arguments"))
				require.NoError(t, err)
				assert.Equal(t, jsonOutput, strings.Contains(string(args), "\n-json\n"))
				want := "Human-readable output\n"
				if jsonOutput {
					want = "{\"type\":\"complete\",\"@message\":\"done\",\"extra\":{\"preserved\":true}}\n"
				}
				assert.Equal(t, "{\"terraform_version\":\"1.5.7\"}\n"+want, stdout.String())
			})
		}
	}
}

// Fail only on command JSON, allowing terraform-exec's version/init setup to run.
type failingJSONWriter struct{ err error }

func (w failingJSONWriter) Write(p []byte) (int, error) {
	if bytes.Contains(p, []byte(`"type"`)) {
		return 0, w.err
	}
	return len(p), nil
}

func TestModuleJSONOutputFailure(t *testing.T) {
	writeErr := errors.New("output unavailable")
	for _, operation := range []string{"apply", "destroy"} {
		for _, tt := range []struct {
			name     string
			writeErr error
			wantErr  error
			exit     int
		}{
			{name: "writer error", writeErr: writeErr, wantErr: writeErr},
			{name: "short write", wantErr: io.ErrShortWrite},
			{name: "original process error", writeErr: writeErr, exit: 7},
		} {
			t.Run(operation+"/"+tt.name, func(t *testing.T) {
				root := t.TempDir()
				require.NoError(t, os.Mkdir(filepath.Join(root, "module"), 0o750))
				binary := filepath.Join(root, "terraform")
				require.NoError(t, os.WriteFile(binary, []byte(`#!/bin/sh
case "$1" in
version) printf '{"terraform_version":"1.5.7"}\n';;
init) exit 0;;
apply|destroy) exec awk '1; END {exit `+fmt.Sprint(tt.exit)+`}' large.json;;
esac
`), 0o750))
				require.NoError(t, os.WriteFile(filepath.Join(root, "module", "large.json"),
					[]byte(strings.Repeat("{\"type\":\"log\",\"@message\":\"progress\"}\n", 100000)), 0o600))
				tf := &Terraform{rootPath: root, tfExecPath: binary, tfExecutorFactory: &defaultExecutorFactory{}}
				ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
				defer cancel()
				moduleOptions := ModuleOptions{ModulePath: "module", BackendConfigFileName: "backend.tf", VariableFileName: "vars.tfvars"}
				var err error
				if operation == "apply" {
					err = tf.ApplyModule(ctx, moduleOptions, ApplyOptions{JSONOutput: true, StdOut: failingJSONWriter{tt.writeErr}, GracefulShutdownPeriod: 100 * time.Millisecond})
				} else {
					err = tf.DestroyModule(ctx, moduleOptions, DestroyOptions{JSONOutput: true, StdOut: failingJSONWriter{tt.writeErr}, GracefulShutdownPeriod: 100 * time.Millisecond})
				}
				require.NoError(t, ctx.Err(), "output failure must not block Terraform until cancellation")
				if tt.exit != 0 {
					var exitErr *exec.ExitError
					require.ErrorAs(t, err, &exitErr)
					assert.Equal(t, tt.exit, exitErr.ExitCode())
					assert.NotErrorIs(t, err, writeErr)
				} else {
					require.ErrorIs(t, err, tt.wantErr)
				}
			})
		}
	}
}
