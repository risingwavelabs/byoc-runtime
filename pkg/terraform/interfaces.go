// Package terraform defines logic of how RisingWave manages BYOC terraform modules.
package terraform

import (
	"context"
	"io"
	"net/http"
	"time"

	"github.com/hashicorp/terraform-exec/tfexec"
)

// Executor defines the interface for terraform execution operations.
// This interface wraps tfexec.Terraform to allow for mocking in tests.
type Executor interface {
	Init(ctx context.Context, opts ...tfexec.InitOption) error
	Apply(ctx context.Context, opts ...tfexec.ApplyOption) error
	Destroy(ctx context.Context, opts ...tfexec.DestroyOption) error
	Output(ctx context.Context, opts ...tfexec.OutputOption) (map[string]tfexec.OutputMeta, error)
	ForceUnlock(ctx context.Context, lockID string, opts ...tfexec.ForceUnlockOption) error
	SetStdout(w io.Writer)
	SetStderr(w io.Writer)
	SetWaitDelay(d time.Duration) error
}

// ExecutorFactory creates Executor instances.
type ExecutorFactory interface {
	NewTerraform(workingDir, execPath string) (Executor, error)
}

// Installer defines the interface for installing terraform binaries.
type Installer interface {
	Install(ctx context.Context, dir, version, apiBaseURL string) (string, error)
}

// HTTPClient defines the interface for HTTP operations.
type HTTPClient interface {
	Do(req *http.Request) (*http.Response, error)
}

// defaultExecutorFactory implements ExecutorFactory using tfexec.
type defaultExecutorFactory struct{}

func (f *defaultExecutorFactory) NewTerraform(workingDir, execPath string) (Executor, error) {
	return tfexec.NewTerraform(workingDir, execPath)
}

// defaultInstaller implements Installer using hc-install.
type defaultInstaller struct{}

func (i *defaultInstaller) Install(ctx context.Context, dir, tfVersion, apiBaseURL string) (string, error) {
	return defaultInstallTerraform(ctx, dir, tfVersion, apiBaseURL)
}

// defaultHTTPClient wraps http.DefaultClient to implement HTTPClient.
type defaultHTTPClient struct{}

func (c *defaultHTTPClient) Do(req *http.Request) (*http.Response, error) {
	return http.DefaultClient.Do(req)
}
