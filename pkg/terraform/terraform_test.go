package terraform

import (
	"archive/zip"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/hashicorp/terraform-exec/tfexec"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/mock/gomock"
)

func TestNew(t *testing.T) {
	tests := []struct {
		name           string
		setupMocks     func(ctrl *gomock.Controller, tempDir string) (*MockHTTPClient, *MockInstaller)
		packageContent []byte
		tfVersion      string
		wantErr        bool
		errContains    string
	}{
		{
			name: "successful initialization",
			setupMocks: func(ctrl *gomock.Controller, _ string) (*MockHTTPClient, *MockInstaller) {
				httpClient := NewMockHTTPClient(ctrl)
				installer := NewMockInstaller(ctrl)

				// Mock HTTP response with valid zip content
				httpClient.EXPECT().Do(gomock.Any()).DoAndReturn(func(_ *http.Request) (*http.Response, error) {
					return &http.Response{
						StatusCode: http.StatusOK,
						Body:       io.NopCloser(bytes.NewReader(createTestZip(t, "1.5.0"))),
					}, nil
				})

				installer.EXPECT().Install(gomock.Any(), gomock.Any(), "1.5.0", "").Return("/path/to/terraform", nil)

				return httpClient, installer
			},
			tfVersion: "1.5.0",
			wantErr:   false,
		},
		{
			name: "download failure",
			setupMocks: func(ctrl *gomock.Controller, _ string) (*MockHTTPClient, *MockInstaller) {
				httpClient := NewMockHTTPClient(ctrl)
				installer := NewMockInstaller(ctrl)

				httpClient.EXPECT().Do(gomock.Any()).Return(nil, errors.New("network error"))

				return httpClient, installer
			},
			wantErr:     true,
			errContains: "failed to download",
		},
		{
			name: "terraform install failure",
			setupMocks: func(ctrl *gomock.Controller, _ string) (*MockHTTPClient, *MockInstaller) {
				httpClient := NewMockHTTPClient(ctrl)
				installer := NewMockInstaller(ctrl)

				httpClient.EXPECT().Do(gomock.Any()).DoAndReturn(func(_ *http.Request) (*http.Response, error) {
					return &http.Response{
						StatusCode: http.StatusOK,
						Body:       io.NopCloser(bytes.NewReader(createTestZip(t, "1.5.0"))),
					}, nil
				})

				installer.EXPECT().Install(gomock.Any(), gomock.Any(), "1.5.0", "").Return("", errors.New("install failed"))

				return httpClient, installer
			},
			tfVersion:   "1.5.0",
			wantErr:     true,
			errContains: "failed to initialize Terraform",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ctrl := gomock.NewController(t)
			defer ctrl.Finish()

			tempDir := t.TempDir()
			httpClient, installer := tt.setupMocks(ctrl, tempDir)

			ctx := context.Background()
			options := NewTerraformOptions{
				RootPath:          tempDir,
				TFVersionFilePath: ".terraform-version",
				PackageURL:        "http://example.com/package.zip",
				PackageDestName:   "package.zip",
				HTTPClient:        httpClient,
				TFInstaller:       installer,
			}

			tf, err := New(ctx, options)
			if tt.wantErr {
				require.Error(t, err)
				if tt.errContains != "" {
					assert.Contains(t, err.Error(), tt.errContains)
				}
				return
			}
			require.NoError(t, err)
			assert.NotNil(t, tf)
		})
	}
}

func TestClean(t *testing.T) {
	t.Run("successful cleanup", func(t *testing.T) {
		tempDir := t.TempDir()
		subDir := filepath.Join(tempDir, "terraform-work")
		err := os.MkdirAll(subDir, 0750)
		require.NoError(t, err)

		// Create a test file
		testFile := filepath.Join(subDir, "test.tf")
		err = os.WriteFile(testFile, []byte("test content"), 0644)
		require.NoError(t, err)

		tf := &Terraform{rootPath: subDir}
		err = tf.Clean(context.Background())
		require.NoError(t, err)

		// Verify directory is removed
		_, err = os.Stat(subDir)
		assert.True(t, os.IsNotExist(err))
	})

	t.Run("cleanup non-existent directory", func(t *testing.T) {
		tf := &Terraform{rootPath: "/non/existent/path/that/does/not/exist"}
		err := tf.Clean(context.Background())
		// os.RemoveAll doesn't return error for non-existent paths
		require.NoError(t, err)
	})
}

func TestApplyModule(t *testing.T) {
	tests := []struct {
		name        string
		setupMocks  func(ctrl *gomock.Controller) (*MockExecutorFactory, *MockExecutor)
		applyOpts   ApplyOptions
		moduleOpts  ModuleOptions
		wantErr     bool
		errContains string
	}{
		{
			name: "successful apply",
			setupMocks: func(ctrl *gomock.Controller) (*MockExecutorFactory, *MockExecutor) {
				factory := NewMockExecutorFactory(ctrl)
				executor := NewMockExecutor(ctrl)

				factory.EXPECT().NewTerraform(gomock.Any(), gomock.Any()).Return(executor, nil)
				executor.EXPECT().SetStdout(gomock.Any())
				executor.EXPECT().SetStderr(gomock.Any())
				executor.EXPECT().SetWaitDelay(gomock.Any()).Return(nil)
				executor.EXPECT().Init(gomock.Any(), gomock.Any(), gomock.Any()).Return(nil)
				executor.EXPECT().Apply(gomock.Any(), gomock.Any()).Return(nil)

				return factory, executor
			},
			applyOpts: ApplyOptions{
				Retry:                  1,
				RetryInterval:          time.Millisecond,
				GracefulShutdownPeriod: time.Second,
			},
			moduleOpts: ModuleOptions{
				ModulePath:            "test-module",
				BackendConfigFileName: "backend.tf",
				BackendConfig:         []byte("backend config"),
				VariableFileName:      "vars.tfvars",
				VariablePayload:       []byte("variables"),
			},
			wantErr: false,
		},
		{
			name: "apply with sensitive variables",
			setupMocks: func(ctrl *gomock.Controller) (*MockExecutorFactory, *MockExecutor) {
				factory := NewMockExecutorFactory(ctrl)
				executor := NewMockExecutor(ctrl)

				factory.EXPECT().NewTerraform(gomock.Any(), gomock.Any()).Return(executor, nil)
				executor.EXPECT().SetStdout(gomock.Any())
				executor.EXPECT().SetStderr(gomock.Any())
				executor.EXPECT().SetWaitDelay(gomock.Any()).Return(nil)
				executor.EXPECT().Init(gomock.Any(), gomock.Any(), gomock.Any()).Return(nil)
				// With variadic args, gomock.Any() matches any number of additional arguments
				executor.EXPECT().Apply(gomock.Any(), gomock.Any()).Return(nil)

				return factory, executor
			},
			applyOpts: ApplyOptions{
				Retry:                  1,
				RetryInterval:          time.Millisecond,
				GracefulShutdownPeriod: time.Second,
			},
			moduleOpts: ModuleOptions{
				ModulePath:            "test-module",
				BackendConfigFileName: "backend.tf",
				BackendConfig:         []byte("backend config"),
				VariableFileName:      "vars.tfvars",
				VariablePayload:       []byte("variables"),
				SensitiveVariables:    map[string]string{"secret": "value"},
			},
			wantErr: false,
		},
		{
			name: "terraform executor creation failure",
			setupMocks: func(ctrl *gomock.Controller) (*MockExecutorFactory, *MockExecutor) {
				factory := NewMockExecutorFactory(ctrl)
				executor := NewMockExecutor(ctrl)

				factory.EXPECT().NewTerraform(gomock.Any(), gomock.Any()).Return(nil, errors.New("executor creation failed"))

				return factory, executor
			},
			applyOpts: ApplyOptions{},
			moduleOpts: ModuleOptions{
				ModulePath:            "test-module",
				BackendConfigFileName: "backend.tf",
				BackendConfig:         []byte("backend config"),
				VariableFileName:      "vars.tfvars",
				VariablePayload:       []byte("variables"),
			},
			wantErr:     true,
			errContains: "failed to create Terraform exec",
		},
		{
			name: "init failure",
			setupMocks: func(ctrl *gomock.Controller) (*MockExecutorFactory, *MockExecutor) {
				factory := NewMockExecutorFactory(ctrl)
				executor := NewMockExecutor(ctrl)

				factory.EXPECT().NewTerraform(gomock.Any(), gomock.Any()).Return(executor, nil)
				executor.EXPECT().SetStdout(gomock.Any())
				executor.EXPECT().SetStderr(gomock.Any())
				executor.EXPECT().SetWaitDelay(gomock.Any()).Return(nil)
				executor.EXPECT().Init(gomock.Any(), gomock.Any(), gomock.Any()).Return(errors.New("init failed"))

				return factory, executor
			},
			applyOpts: ApplyOptions{
				GracefulShutdownPeriod: time.Second,
			},
			moduleOpts: ModuleOptions{
				ModulePath:            "test-module",
				BackendConfigFileName: "backend.tf",
				BackendConfig:         []byte("backend config"),
				VariableFileName:      "vars.tfvars",
				VariablePayload:       []byte("variables"),
			},
			wantErr:     true,
			errContains: "failed to init terraform",
		},
		{
			name: "apply failure",
			setupMocks: func(ctrl *gomock.Controller) (*MockExecutorFactory, *MockExecutor) {
				factory := NewMockExecutorFactory(ctrl)
				executor := NewMockExecutor(ctrl)

				factory.EXPECT().NewTerraform(gomock.Any(), gomock.Any()).Return(executor, nil)
				executor.EXPECT().SetStdout(gomock.Any())
				executor.EXPECT().SetStderr(gomock.Any())
				executor.EXPECT().SetWaitDelay(gomock.Any()).Return(nil)
				executor.EXPECT().Init(gomock.Any(), gomock.Any(), gomock.Any()).Return(nil)
				executor.EXPECT().Apply(gomock.Any()).Return(errors.New("apply failed"))

				return factory, executor
			},
			applyOpts: ApplyOptions{
				GracefulShutdownPeriod: time.Second,
			},
			moduleOpts: ModuleOptions{
				ModulePath:            "test-module",
				BackendConfigFileName: "backend.tf",
				BackendConfig:         []byte("backend config"),
				VariableFileName:      "vars.tfvars",
				VariablePayload:       []byte("variables"),
			},
			wantErr:     true,
			errContains: "failed to apply terraform config",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ctrl := gomock.NewController(t)
			defer ctrl.Finish()

			tempDir := t.TempDir()
			modulePath := filepath.Join(tempDir, tt.moduleOpts.ModulePath)
			err := os.MkdirAll(modulePath, 0750)
			require.NoError(t, err)

			factory, _ := tt.setupMocks(ctrl)

			tf := &Terraform{
				rootPath:          tempDir,
				tfExecutorFactory: factory,
			}

			ctx := context.Background()
			err = tf.ApplyModule(ctx, tt.moduleOpts, tt.applyOpts)
			if tt.wantErr {
				require.Error(t, err)
				if tt.errContains != "" {
					assert.Contains(t, err.Error(), tt.errContains)
				}
				return
			}
			require.NoError(t, err)
		})
	}
}

func TestDestroyModule(t *testing.T) {
	tests := []struct {
		name        string
		setupMocks  func(ctrl *gomock.Controller) (*MockExecutorFactory, *MockExecutor)
		destroyOpts DestroyOptions
		moduleOpts  ModuleOptions
		wantErr     bool
		errContains string
	}{
		{
			name: "successful destroy",
			setupMocks: func(ctrl *gomock.Controller) (*MockExecutorFactory, *MockExecutor) {
				factory := NewMockExecutorFactory(ctrl)
				executor := NewMockExecutor(ctrl)

				factory.EXPECT().NewTerraform(gomock.Any(), gomock.Any()).Return(executor, nil)
				executor.EXPECT().SetStdout(gomock.Any())
				executor.EXPECT().SetStderr(gomock.Any())
				executor.EXPECT().SetWaitDelay(gomock.Any()).Return(nil)
				executor.EXPECT().Init(gomock.Any(), gomock.Any(), gomock.Any()).Return(nil)
				executor.EXPECT().Destroy(gomock.Any(), gomock.Any()).Return(nil)

				return factory, executor
			},
			destroyOpts: DestroyOptions{
				Retry:                  1,
				RetryInterval:          time.Millisecond,
				GracefulShutdownPeriod: time.Second,
			},
			moduleOpts: ModuleOptions{
				ModulePath:            "test-module",
				BackendConfigFileName: "backend.tf",
				BackendConfig:         []byte("backend config"),
				VariableFileName:      "vars.tfvars",
				VariablePayload:       []byte("variables"),
				SensitiveVariables:    map[string]string{"var1": "value1"},
			},
			wantErr: false,
		},
		{
			name: "destroy failure",
			setupMocks: func(ctrl *gomock.Controller) (*MockExecutorFactory, *MockExecutor) {
				factory := NewMockExecutorFactory(ctrl)
				executor := NewMockExecutor(ctrl)

				factory.EXPECT().NewTerraform(gomock.Any(), gomock.Any()).Return(executor, nil)
				executor.EXPECT().SetStdout(gomock.Any())
				executor.EXPECT().SetStderr(gomock.Any())
				executor.EXPECT().SetWaitDelay(gomock.Any()).Return(nil)
				executor.EXPECT().Init(gomock.Any(), gomock.Any(), gomock.Any()).Return(nil)
				executor.EXPECT().Destroy(gomock.Any(), gomock.Any()).Return(errors.New("destroy failed"))

				return factory, executor
			},
			destroyOpts: DestroyOptions{
				GracefulShutdownPeriod: time.Second,
			},
			moduleOpts: ModuleOptions{
				ModulePath:            "test-module",
				BackendConfigFileName: "backend.tf",
				BackendConfig:         []byte("backend config"),
				VariableFileName:      "vars.tfvars",
				VariablePayload:       []byte("variables"),
				SensitiveVariables:    map[string]string{"var1": "value1"},
			},
			wantErr:     true,
			errContains: "failed to apply terraform config",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ctrl := gomock.NewController(t)
			defer ctrl.Finish()

			tempDir := t.TempDir()
			modulePath := filepath.Join(tempDir, tt.moduleOpts.ModulePath)
			err := os.MkdirAll(modulePath, 0750)
			require.NoError(t, err)

			factory, _ := tt.setupMocks(ctrl)

			tf := &Terraform{
				rootPath:          tempDir,
				tfExecutorFactory: factory,
			}

			ctx := context.Background()
			err = tf.DestroyModule(ctx, tt.moduleOpts, tt.destroyOpts)
			if tt.wantErr {
				require.Error(t, err)
				if tt.errContains != "" {
					assert.Contains(t, err.Error(), tt.errContains)
				}
				return
			}
			require.NoError(t, err)
		})
	}
}

func TestRetrieveModuleOutput(t *testing.T) {
	tests := []struct {
		name        string
		outputKey   string
		setupMocks  func(ctrl *gomock.Controller) (*MockExecutorFactory, *MockExecutor)
		outputOpts  OutputOptions
		moduleOpts  ModuleOptions
		wantOutput  json.RawMessage
		wantErr     bool
		errContains string
	}{
		{
			name:      "successful output retrieval",
			outputKey: "instance_ip",
			setupMocks: func(ctrl *gomock.Controller) (*MockExecutorFactory, *MockExecutor) {
				factory := NewMockExecutorFactory(ctrl)
				executor := NewMockExecutor(ctrl)

				factory.EXPECT().NewTerraform(gomock.Any(), gomock.Any()).Return(executor, nil)
				executor.EXPECT().Init(gomock.Any(), gomock.Any(), gomock.Any()).Return(nil)
				executor.EXPECT().Output(gomock.Any()).Return(map[string]tfexec.OutputMeta{
					"instance_ip": {
						Value: json.RawMessage(`"192.168.1.1"`),
					},
				}, nil)

				return factory, executor
			},
			outputOpts: OutputOptions{
				Retry:         1,
				RetryInterval: time.Millisecond,
			},
			moduleOpts: ModuleOptions{
				ModulePath:            "test-module",
				BackendConfigFileName: "backend.tf",
				BackendConfig:         []byte("backend config"),
				VariableFileName:      "vars.tfvars",
				VariablePayload:       []byte("variables"),
			},
			wantOutput: json.RawMessage(`"192.168.1.1"`),
			wantErr:    false,
		},
		{
			name:      "missing output key",
			outputKey: "non_existent_key",
			setupMocks: func(ctrl *gomock.Controller) (*MockExecutorFactory, *MockExecutor) {
				factory := NewMockExecutorFactory(ctrl)
				executor := NewMockExecutor(ctrl)

				factory.EXPECT().NewTerraform(gomock.Any(), gomock.Any()).Return(executor, nil)
				executor.EXPECT().Init(gomock.Any(), gomock.Any(), gomock.Any()).Return(nil)
				executor.EXPECT().Output(gomock.Any()).Return(map[string]tfexec.OutputMeta{
					"other_key": {
						Value: json.RawMessage(`"value"`),
					},
				}, nil)

				return factory, executor
			},
			outputOpts: OutputOptions{},
			moduleOpts: ModuleOptions{
				ModulePath:            "test-module",
				BackendConfigFileName: "backend.tf",
				BackendConfig:         []byte("backend config"),
				VariableFileName:      "vars.tfvars",
				VariablePayload:       []byte("variables"),
			},
			wantErr:     true,
			errContains: "missing key non_existent_key",
		},
		{
			name:      "output failure",
			outputKey: "instance_ip",
			setupMocks: func(ctrl *gomock.Controller) (*MockExecutorFactory, *MockExecutor) {
				factory := NewMockExecutorFactory(ctrl)
				executor := NewMockExecutor(ctrl)

				factory.EXPECT().NewTerraform(gomock.Any(), gomock.Any()).Return(executor, nil)
				executor.EXPECT().Init(gomock.Any(), gomock.Any(), gomock.Any()).Return(nil)
				executor.EXPECT().Output(gomock.Any()).Return(nil, errors.New("output failed"))

				return factory, executor
			},
			outputOpts: OutputOptions{},
			moduleOpts: ModuleOptions{
				ModulePath:            "test-module",
				BackendConfigFileName: "backend.tf",
				BackendConfig:         []byte("backend config"),
				VariableFileName:      "vars.tfvars",
				VariablePayload:       []byte("variables"),
			},
			wantErr:     true,
			errContains: "failed to get terraform output",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ctrl := gomock.NewController(t)
			defer ctrl.Finish()

			tempDir := t.TempDir()
			modulePath := filepath.Join(tempDir, tt.moduleOpts.ModulePath)
			err := os.MkdirAll(modulePath, 0750)
			require.NoError(t, err)

			factory, _ := tt.setupMocks(ctrl)

			tf := &Terraform{
				rootPath:          tempDir,
				tfExecutorFactory: factory,
			}

			ctx := context.Background()
			output, err := tf.RetrieveModuleOutput(ctx, tt.outputKey, tt.moduleOpts, tt.outputOpts)
			if tt.wantErr {
				require.Error(t, err)
				if tt.errContains != "" {
					assert.Contains(t, err.Error(), tt.errContains)
				}
				return
			}
			require.NoError(t, err)
			assert.Equal(t, tt.wantOutput, output)
		})
	}
}

func TestRetrieveModuleOutputOrNil(t *testing.T) {
	tests := []struct {
		name        string
		outputKey   string
		setupMocks  func(ctrl *gomock.Controller) (*MockExecutorFactory, *MockExecutor)
		outputOpts  OutputOptions
		moduleOpts  ModuleOptions
		wantOutput  json.RawMessage
		wantNil     bool
		wantErr     bool
		errContains string
	}{
		{
			name:      "successful output retrieval",
			outputKey: "instance_ip",
			setupMocks: func(ctrl *gomock.Controller) (*MockExecutorFactory, *MockExecutor) {
				factory := NewMockExecutorFactory(ctrl)
				executor := NewMockExecutor(ctrl)

				factory.EXPECT().NewTerraform(gomock.Any(), gomock.Any()).Return(executor, nil)
				executor.EXPECT().Init(gomock.Any(), gomock.Any(), gomock.Any()).Return(nil)
				executor.EXPECT().Output(gomock.Any()).Return(map[string]tfexec.OutputMeta{
					"instance_ip": {
						Value: json.RawMessage(`"192.168.1.1"`),
					},
				}, nil)

				return factory, executor
			},
			outputOpts: OutputOptions{},
			moduleOpts: ModuleOptions{
				ModulePath:            "test-module",
				BackendConfigFileName: "backend.tf",
				BackendConfig:         []byte("backend config"),
				VariableFileName:      "vars.tfvars",
				VariablePayload:       []byte("variables"),
			},
			wantOutput: json.RawMessage(`"192.168.1.1"`),
			wantErr:    false,
		},
		{
			name:      "empty output returns nil",
			outputKey: "instance_ip",
			setupMocks: func(ctrl *gomock.Controller) (*MockExecutorFactory, *MockExecutor) {
				factory := NewMockExecutorFactory(ctrl)
				executor := NewMockExecutor(ctrl)

				factory.EXPECT().NewTerraform(gomock.Any(), gomock.Any()).Return(executor, nil)
				executor.EXPECT().Init(gomock.Any(), gomock.Any(), gomock.Any()).Return(nil)
				executor.EXPECT().Output(gomock.Any()).Return(map[string]tfexec.OutputMeta{}, nil)

				return factory, executor
			},
			outputOpts: OutputOptions{},
			moduleOpts: ModuleOptions{
				ModulePath:            "test-module",
				BackendConfigFileName: "backend.tf",
				BackendConfig:         []byte("backend config"),
				VariableFileName:      "vars.tfvars",
				VariablePayload:       []byte("variables"),
			},
			wantNil: true,
			wantErr: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ctrl := gomock.NewController(t)
			defer ctrl.Finish()

			tempDir := t.TempDir()
			modulePath := filepath.Join(tempDir, tt.moduleOpts.ModulePath)
			err := os.MkdirAll(modulePath, 0750)
			require.NoError(t, err)

			factory, _ := tt.setupMocks(ctrl)

			tf := &Terraform{
				rootPath:          tempDir,
				tfExecutorFactory: factory,
			}

			ctx := context.Background()
			output, err := tf.RetrieveModuleOutputOrNil(ctx, tt.outputKey, tt.moduleOpts, tt.outputOpts)
			if tt.wantErr {
				require.Error(t, err)
				if tt.errContains != "" {
					assert.Contains(t, err.Error(), tt.errContains)
				}
				return
			}
			require.NoError(t, err)
			if tt.wantNil {
				assert.Nil(t, output)
			} else {
				assert.Equal(t, tt.wantOutput, output)
			}
		})
	}
}

func TestExtractStateLockedError(t *testing.T) {
	tests := []struct {
		name         string
		err          error
		wantLockInfo LockErrInfo
		wantOk       bool
	}{
		{
			name:   "nil error",
			err:    nil,
			wantOk: false,
		},
		{
			name:   "non-lock error",
			err:    errors.New("some other error"),
			wantOk: false,
		},
		{
			name: "valid state lock error",
			// nolint:revive // This error message simulates actual Terraform output which uses capital letters
			err: errors.New(`Error acquiring the state lock

Error message: ConditionalCheckFailedException: The conditional request failed
Lock Info:
  ID:        12345678-1234-1234-1234-123456789012
  Path:      terraform.tfstate
  Operation: OperationTypeApply
  Who:       user@host
  Version:   1.5.0
  Created:   2023-06-15 10:30:45.123456789 +0000 UTC
`),
			wantLockInfo: LockErrInfo{
				ID:        "12345678-1234-1234-1234-123456789012",
				Path:      "terraform.tfstate",
				Operation: "OperationTypeApply",
				Who:       "user@host",
				Version:   "1.5.0",
				Created:   time.Date(2023, 6, 15, 10, 30, 45, 123456789, time.FixedZone("", 0)),
			},
			wantOk: true,
		},
		{
			name: "lock error without lock info",
			err:  errors.New("Error acquiring the state lock\nSome other message without lock info"),
			// This will match the first regex but not the second
			wantOk: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			lockInfo, ok := extractStateLockedError(tt.err)
			assert.Equal(t, tt.wantOk, ok)
			if tt.wantOk {
				assert.Equal(t, tt.wantLockInfo.ID, lockInfo.ID)
				assert.Equal(t, tt.wantLockInfo.Path, lockInfo.Path)
				assert.Equal(t, tt.wantLockInfo.Operation, lockInfo.Operation)
				assert.Equal(t, tt.wantLockInfo.Who, lockInfo.Who)
				assert.Equal(t, tt.wantLockInfo.Version, lockInfo.Version)
				assert.True(t, tt.wantLockInfo.Created.Equal(lockInfo.Created))
			}
		})
	}
}

func TestToVariableAssignments(t *testing.T) {
	tests := []struct {
		name      string
		variables map[string]string
		want      []string
	}{
		{
			name:      "empty map",
			variables: map[string]string{},
			want:      nil,
		},
		{
			name: "single variable",
			variables: map[string]string{
				"key1": "value1",
			},
			want: []string{"key1=value1"},
		},
		{
			name: "multiple variables",
			variables: map[string]string{
				"key1": "value1",
				"key2": "value2",
			},
			want: []string{"key1=value1", "key2=value2"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := toVariableAssignments(tt.variables)
			if tt.want == nil {
				assert.Nil(t, got)
				return
			}
			// Since map iteration order is not guaranteed, we need to check that all expected values are present
			assert.Len(t, got, len(tt.want))
			for _, expected := range tt.want {
				assert.Contains(t, got, expected)
			}
		})
	}
}

func TestSetUpModule(t *testing.T) {
	t.Run("successful setup", func(t *testing.T) {
		tempDir := t.TempDir()
		modulePath := filepath.Join(tempDir, "test-module")
		err := os.MkdirAll(modulePath, 0750)
		require.NoError(t, err)

		tf := &Terraform{rootPath: tempDir}

		moduleOpts := ModuleOptions{
			ModulePath:            "test-module",
			BackendConfigFileName: "backend.tf",
			BackendConfig:         []byte("backend config content"),
			VariableFileName:      "vars.tfvars",
			VariablePayload:       []byte("variable content"),
			CLIConfigFileName:     "cli.tfrc",
			CLIConfigPayload:      []byte("cli config content"),
		}

		absModulePath, backendCfgPath, variablePath, cliConfigPath, err := tf.setUpModule(moduleOpts)
		require.NoError(t, err)

		assert.Equal(t, filepath.Join(tempDir, "test-module"), absModulePath)
		assert.Equal(t, filepath.Join(modulePath, "backend.tf"), backendCfgPath)
		assert.Equal(t, filepath.Join(modulePath, "vars.tfvars"), variablePath)
		assert.Equal(t, filepath.Join(modulePath, "cli.tfrc"), cliConfigPath)

		// Verify files are created with correct content
		backendContent, err := os.ReadFile(backendCfgPath)
		require.NoError(t, err)
		assert.Equal(t, "backend config content", string(backendContent))

		variableContent, err := os.ReadFile(variablePath)
		require.NoError(t, err)
		assert.Equal(t, "variable content", string(variableContent))

		cliContent, err := os.ReadFile(cliConfigPath)
		require.NoError(t, err)
		assert.Equal(t, "cli config content", string(cliContent))
	})

	t.Run("setup without cli config", func(t *testing.T) {
		tempDir := t.TempDir()
		modulePath := filepath.Join(tempDir, "test-module")
		err := os.MkdirAll(modulePath, 0750)
		require.NoError(t, err)

		tf := &Terraform{rootPath: tempDir}

		moduleOpts := ModuleOptions{
			ModulePath:            "test-module",
			BackendConfigFileName: "backend.tf",
			BackendConfig:         []byte("backend config"),
			VariableFileName:      "vars.tfvars",
			VariablePayload:       []byte("variables"),
		}

		_, _, _, cliConfigPath, err := tf.setUpModule(moduleOpts)
		require.NoError(t, err)
		assert.Empty(t, cliConfigPath)
	})
}

func TestReadTerraformVersion(t *testing.T) {
	t.Run("valid version file", func(t *testing.T) {
		tempDir := t.TempDir()
		versionFile := filepath.Join(tempDir, ".terraform-version")
		err := os.WriteFile(versionFile, []byte("1.5.0\n"), 0644)
		require.NoError(t, err)

		version, err := readTerraformVersion(versionFile)
		require.NoError(t, err)
		assert.Equal(t, "1.5.0", version)
	})

	t.Run("version file with extra whitespace", func(t *testing.T) {
		tempDir := t.TempDir()
		versionFile := filepath.Join(tempDir, ".terraform-version")
		err := os.WriteFile(versionFile, []byte("  1.6.0  \n\n"), 0644)
		require.NoError(t, err)

		version, err := readTerraformVersion(versionFile)
		require.NoError(t, err)
		assert.Equal(t, "1.6.0", version)
	})

	t.Run("non-existent file", func(t *testing.T) {
		_, err := readTerraformVersion("/non/existent/path")
		require.Error(t, err)
	})
}

// createTestZip creates a minimal valid zip file for testing.
func createTestZip(t *testing.T, tfVersion string) []byte {
	t.Helper()

	buf := new(bytes.Buffer)
	w := zip.NewWriter(buf)

	// Add .terraform-version file
	versionFile, err := w.Create(".terraform-version")
	require.NoError(t, err)
	_, err = versionFile.Write([]byte(tfVersion))
	require.NoError(t, err)

	// Add a dummy main.tf
	mainFile, err := w.Create("main.tf")
	require.NoError(t, err)
	_, err = mainFile.Write([]byte(`resource "null_resource" "test" {}`))
	require.NoError(t, err)

	err = w.Close()
	require.NoError(t, err)

	return buf.Bytes()
}
