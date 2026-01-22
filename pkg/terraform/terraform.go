// Package terraform defines logic of how RisingWave manages BYOC terraform modules.
package terraform

import (
	"archive/zip"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"regexp"
	"strings"
	"time"

	version "github.com/hashicorp/go-version"
	"github.com/hashicorp/hc-install/product"
	"github.com/hashicorp/hc-install/releases"
	"github.com/hashicorp/terraform-exec/tfexec"
	"github.com/risingwavelabs/eris"

	"github.com/risingwavelabs/byoc-runtime/pkg/module"
	"github.com/risingwavelabs/byoc-runtime/pkg/utils/wait"
)

var (
	stateLockErrRegexp  = regexp.MustCompile(`Error acquiring the state lock`)
	stateLockInfoRegexp = regexp.MustCompile(`Lock Info:\n\s*ID:\s*([^\n]+)\n\s*Path:\s*([^\n]+)\n\s*Operation:\s*([^\n]+)\n\s*Who:\s*([^\n]+)\n\s*Version:\s*([^\n]+)\n\s*Created:\s*([^\n]+)\n`)
)

const (
	lockCreatedLayout = "2006-01-02 15:04:05.999999999 -0700 MST"

	tfCLIConfigFileEnvKey = "TF_CLI_CONFIG_FILE"
)

// Terraform wraps BYOC terraform management logics.
type Terraform struct {
	// ModulePath is the relative path of the file storing TF version to the workspace root path
	tfVersionFilePath            string
	tfExecPath                   string
	rootPath                     string
	packageURL                   string
	packageName                  string
	privateTFBinaryBaseURL       string // If empty, use the default public endpoint to download TF binary.
	customModuleRegistryEndpoint string // If empty, use the default hashicorp endpoint to download 3rd party modules.

	// Injected dependencies for testing
	tfExecutorFactory ExecutorFactory
	tfInstaller       Installer
	httpClient        HTTPClient
}

// NewTerraformOptions contains all options for Terraform initialization.
type NewTerraformOptions struct {
	RootPath                     string
	TFVersionFilePath            string
	PackageURL                   string
	PackageDestName              string
	PrivateTFBinaryBaseURL       string
	CustomModuleRegistryEndpoint string

	// Optional dependencies for testing (if nil, defaults are used)
	TFExecutorFactory ExecutorFactory
	TFInstaller       Installer
	HTTPClient        HTTPClient
}

// New initilizes a new Terraform.
func New(ctx context.Context, options NewTerraformOptions) (*Terraform, error) {
	// Set default implementations if not provided
	tfExecutorFactory := options.TFExecutorFactory
	if tfExecutorFactory == nil {
		tfExecutorFactory = &defaultExecutorFactory{}
	}
	tfInstaller := options.TFInstaller
	if tfInstaller == nil {
		tfInstaller = &defaultInstaller{}
	}
	httpClient := options.HTTPClient
	if httpClient == nil {
		httpClient = &defaultHTTPClient{}
	}

	t := &Terraform{
		tfVersionFilePath:            options.TFVersionFilePath,
		rootPath:                     options.RootPath,
		packageURL:                   options.PackageURL,
		packageName:                  options.PackageDestName,
		privateTFBinaryBaseURL:       options.PrivateTFBinaryBaseURL,
		customModuleRegistryEndpoint: options.CustomModuleRegistryEndpoint,
		tfExecutorFactory:            tfExecutorFactory,
		tfInstaller:                  tfInstaller,
		httpClient:                   httpClient,
	}
	if err := t.initialize(ctx); err != nil {
		return nil, eris.Wrapf(err, "failed to initialize terraform")
	}
	return t, nil
}

// Clean cleans up all Terraform temp artifacts.
func (t *Terraform) Clean(_ context.Context) error {
	err := os.RemoveAll(t.rootPath)
	if err != nil {
		return eris.Wrap(err, "failed to clean up byoc directory")
	}
	return nil
}

// ModuleOptions contains all options for Terraform module operations.
type ModuleOptions struct {
	// ModulePath is the relative path of the module to the workspace root path
	ModulePath            string
	BackendConfigFileName string
	BackendConfig         []byte
	// A list of sensitive variable assignments. This will be passed to tfexec
	// through `-var` arg.
	SensitiveVariables map[string]string
	VariableFileName   string
	VariablePayload    []byte

	CLIConfigFileName string
	CLIConfigPayload  []byte
}

// TFInitOptions lists all options for `terraform init`.
type TFInitOptions struct {
	Retry         int
	RetryInterval time.Duration
}

// ApplyOptions lists all options for `terraform apply`.
type ApplyOptions struct {
	Retry                  int
	RetryInterval          time.Duration
	GracefulShutdownPeriod time.Duration
	LockExpirationDuration time.Duration
	InitOptions            TFInitOptions

	StdOut io.Writer
	StdErr io.Writer
}

func (t *Terraform) setUpModule(moduleOptions ModuleOptions) (absModulePath, backendCfgPath, variablePath, cliConfigPath string, err error) {
	absModulePath = fmt.Sprintf("%s/%s", t.rootPath, moduleOptions.ModulePath)
	backendCfgPath = fmt.Sprintf("%s/%s", absModulePath, moduleOptions.BackendConfigFileName)
	err = os.WriteFile(backendCfgPath, []byte(moduleOptions.BackendConfig), 0666)
	if err != nil {
		return "", "", "", "", eris.Wrapf(err, "failed to write tf backend config to %v", backendCfgPath)
	}

	variablePath = fmt.Sprintf("%s/%s", absModulePath, moduleOptions.VariableFileName)
	err = os.WriteFile(variablePath, []byte(moduleOptions.VariablePayload), 0666)
	if err != nil {
		return "", "", "", "", eris.Wrapf(err, "failed to write tf variable payloads to %v", variablePath)
	}

	cliConfigPath = ""
	if moduleOptions.CLIConfigPayload != nil && moduleOptions.CLIConfigFileName != "" {
		cliConfigPath = fmt.Sprintf("%s/%s", absModulePath, moduleOptions.CLIConfigFileName)
		err = os.WriteFile(cliConfigPath, []byte(moduleOptions.CLIConfigPayload), 0666)
		if err != nil {
			return "", "", "", "", eris.Wrapf(err, "failed to write tf cli config payloads to %v", cliConfigPath)
		}
	}
	return absModulePath, backendCfgPath, variablePath, cliConfigPath, nil
}

// ApplyModule applies a Terraform module.
func (t *Terraform) ApplyModule(ctx context.Context, moduleOptions ModuleOptions, applyOptions ApplyOptions) error {
	absModulePath, backendCfgPath, _, cliConfigPath, err := t.setUpModule(moduleOptions)
	if err != nil {
		return eris.Wrap(err, "error setting up the module")
	}

	err = t.terraformInitAndApply(
		ctx,
		absModulePath,
		backendCfgPath,
		cliConfigPath,
		moduleOptions.SensitiveVariables,
		applyOptions,
	)
	if err != nil {
		return eris.Wrap(err, "error applying terraform config")
	}
	return nil
}

// DestroyOptions lists all options for `terraform destroy`.
type DestroyOptions struct {
	Retry                  int
	RetryInterval          time.Duration
	GracefulShutdownPeriod time.Duration
	LockExpirationDuration time.Duration
	InitOptions            TFInitOptions

	StdOut io.Writer
	StdErr io.Writer
}

// DestroyModule destroys a Terraform module.
func (t *Terraform) DestroyModule(ctx context.Context, moduleOptions ModuleOptions, destroyOptions DestroyOptions) error {
	absModulePath, backendCfgPath, _, cliConfigPath, err := t.setUpModule(moduleOptions)
	if err != nil {
		return eris.Wrap(err, "error setting up the module")
	}

	err = t.terraformInitAndDestroy(
		ctx,
		absModulePath,
		backendCfgPath,
		cliConfigPath,
		moduleOptions.SensitiveVariables,
		destroyOptions,
	)
	if err != nil {
		return eris.Wrap(err, "error applying terraform config")
	}
	return nil
}

// OutputOptions defines options for `terraform output` command.
type OutputOptions struct {
	Retry         int
	RetryInterval time.Duration
	InitOptions   TFInitOptions
}

// RetrieveModuleOutput reads the output from a Terraform module.
func (t *Terraform) RetrieveModuleOutput(ctx context.Context, outputKey string, moduleOptions ModuleOptions, outputOptions OutputOptions) (json.RawMessage, error) {
	absModulePath, backendCfgPath, _, cliConfigPath, err := t.setUpModule(moduleOptions)
	if err != nil {
		return nil, eris.Wrap(err, "error setting up the module")
	}
	rawOutput, err := t.terraformInitAndOutput(
		ctx,
		absModulePath,
		backendCfgPath,
		cliConfigPath,
		outputKey,
		false,
		outputOptions,
	)
	if err != nil {
		return nil, eris.Wrap(err, "error retrieving module output")
	}
	return rawOutput.Value, nil
}

// RetrieveModuleOutputOrNil reads the output from a Terraform module if it has one.
func (t *Terraform) RetrieveModuleOutputOrNil(ctx context.Context, outputKey string, moduleOptions ModuleOptions, outputOptions OutputOptions) (json.RawMessage, error) {
	absModulePath, backendCfgPath, _, cliConfigPath, err := t.setUpModule(moduleOptions)
	if err != nil {
		return nil, eris.Wrap(err, "error setting up the module")
	}
	rawOutput, err := t.terraformInitAndOutput(
		ctx,
		absModulePath,
		backendCfgPath,
		cliConfigPath,
		outputKey,
		true,
		outputOptions,
	)
	if err != nil {
		return nil, eris.Wrap(err, "error retrieving module output")
	}
	if rawOutput == nil {
		return nil, nil
	}
	return rawOutput.Value, nil
}

func (t *Terraform) initialize(ctx context.Context) error {
	err := os.RemoveAll(t.rootPath)
	if err != nil {
		return eris.Wrap(err, "failed to clean up byoc directory")
	}
	err = os.MkdirAll(t.rootPath, 0750)
	if err != nil {
		return eris.Wrap(err, "failed to create byoc directory")
	}
	err = t.prepareTerraformPackage(ctx)
	if err != nil {
		return eris.Wrap(err, "failed to prepare the tf files")
	}
	tfVersionPath := fmt.Sprintf("%s/%s", t.rootPath, t.tfVersionFilePath)
	tfVersion, err := readTerraformVersion(tfVersionPath)
	if err != nil {
		return eris.Wrap(err, "invalid terraform version in module package")
	}
	tfExecPath, err := t.tfInstaller.Install(ctx, t.rootPath, tfVersion, t.privateTFBinaryBaseURL)
	if err != nil {
		return eris.Wrapf(err, "failed to initialize Terraform, version: %v", tfVersion)
	}
	t.tfExecPath = tfExecPath
	return nil
}

func (t *Terraform) prepareTerraformPackage(ctx context.Context) error {
	// will download the file from the remote.
	packagePath := fmt.Sprintf("%s/%s", t.rootPath, t.packageName)
	if err := t.downloadFile(ctx, t.packageURL, packagePath); err != nil {
		return eris.Wrap(err, "failed to download Terraform modules")
	}
	if err := unzipFile(packagePath, t.rootPath); err != nil {
		return eris.Wrap(err, "failed to decompress Terraform modules")
	}
	if err := os.Remove(packagePath); err != nil {
		return eris.Wrap(err, "failed to clean up Terraform modules zip")
	}

	if t.customModuleRegistryEndpoint != "" {
		if err := module.InjectCustomModuleRegistry(t.rootPath, t.customModuleRegistryEndpoint); err != nil {
			return eris.Wrapf(err, "failed to inject custom module registry %s to TF package files", t.customModuleRegistryEndpoint)
		}
	}
	return nil
}

func (t *Terraform) downloadFile(ctx context.Context, url, destination string) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return err
	}
	response, err := t.httpClient.Do(req)
	if err != nil {
		return err
	}
	defer func() { _ = response.Body.Close() }()

	outFile, err := os.Create(destination)
	if err != nil {
		return err
	}
	defer func() { _ = outFile.Close() }()

	_, err = io.Copy(outFile, response.Body)
	return err
}

func unzipFile(zipFile, destination string) error {
	reader, err := zip.OpenReader(zipFile)
	if err != nil {
		return err
	}
	defer func() { _ = reader.Close() }()

	for _, file := range reader.File {
		filePath := destination + "/" + file.Name

		if file.FileInfo().IsDir() {
			err := os.MkdirAll(filePath, os.ModePerm)
			if err != nil {
				return err
			}
		} else {
			inFile, err := file.Open()
			if err != nil {
				return err
			}
			defer func() { _ = inFile.Close() }()

			outFile, err := os.Create(filePath)
			if err != nil {
				return err
			}
			defer func() { _ = outFile.Close() }()

			_, err = io.Copy(outFile, inFile)
			if err != nil {
				return err
			}
		}
	}

	return nil
}

func readTerraformVersion(path string) (string, error) {
	versionRaw, err := os.ReadFile(path)
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(string(versionRaw)), nil
}

func defaultInstallTerraform(ctx context.Context, dir, tfVersion, privateTFBinaryBaseURL string) (string, error) {
	ver, err := version.NewVersion(tfVersion)
	if err != nil {
		return "", eris.Wrapf(err, "failed to get terraform version %v", tfVersion)
	}
	installer := &releases.ExactVersion{
		Product:    product.Terraform,
		Version:    ver,
		InstallDir: dir,
		ApiBaseURL: privateTFBinaryBaseURL,
	}

	execPath, err := installer.Install(ctx)
	if err != nil {
		return "", eris.Wrap(err, "error installing Terraform")
	}
	return execPath, nil
}

func (t *Terraform) getTerraformExec(workingDir string) (Executor, error) {
	return t.tfExecutorFactory.NewTerraform(workingDir, t.tfExecPath)
}

func (t *Terraform) terraformInitAndApply(ctx context.Context, workingDir, backendPath, cliConfigPath string, sensitiveVariables map[string]string, options ApplyOptions) error {
	tf, err := t.getTerraformExec(workingDir)
	if err != nil {
		return eris.Wrap(err, "failed to create Terraform exec")
	}

	tf.SetStdout(options.StdOut)
	tf.SetStderr(options.StdErr)
	if err := tf.SetWaitDelay(options.GracefulShutdownPeriod); err != nil {
		return eris.Wrap(err, "failed to set graceful shutdown period")
	}

	err = tfInit(ctx, tf, backendPath, cliConfigPath, options.InitOptions)
	if err != nil {
		return eris.Wrap(err, "failed to init terraform")
	}

	var tfexecApplyOptions []tfexec.ApplyOption
	for _, assignment := range toVariableAssignments(sensitiveVariables) {
		tfexecApplyOptions = append(tfexecApplyOptions, tfexec.Var(assignment))
	}

	apply := func(ctx context.Context) error {
		applyErr := tf.Apply(ctx, tfexecApplyOptions...)
		lockErrInfo, ok := extractStateLockedError(applyErr)
		if options.LockExpirationDuration == 0 || !ok || time.Since(lockErrInfo.Created) < options.LockExpirationDuration {
			return applyErr
		}
		unlockErr := tf.ForceUnlock(ctx, lockErrInfo.ID)
		return eris.Join(applyErr, unlockErr)
	}
	err = wait.RetryWithInterval(ctx, options.Retry, options.RetryInterval, apply)
	if err != nil {
		return eris.Wrap(err, "failed to apply terraform config")
	}
	return nil
}

func (t *Terraform) terraformInitAndDestroy(ctx context.Context, workingDir, backendPath, cliConfigPath string, sensitiveVariables map[string]string, options DestroyOptions) error {
	tf, err := t.getTerraformExec(workingDir)
	if err != nil {
		return eris.Wrap(err, "failed to create Terraform exec")
	}

	tf.SetStdout(options.StdOut)
	tf.SetStderr(options.StdErr)
	if err := tf.SetWaitDelay(options.GracefulShutdownPeriod); err != nil {
		return eris.Wrap(err, "failed to set graceful shutdown period")
	}

	err = tfInit(ctx, tf, backendPath, cliConfigPath, options.InitOptions)
	if err != nil {
		return eris.Wrap(err, "failed to init terraform")
	}

	var tfexecDestroyOptions []tfexec.DestroyOption
	for _, assignment := range toVariableAssignments(sensitiveVariables) {
		tfexecDestroyOptions = append(tfexecDestroyOptions, tfexec.Var(assignment))
	}

	destroy := func(ctx context.Context) error {
		destroyErr := tf.Destroy(ctx, tfexecDestroyOptions...)
		lockErrInfo, ok := extractStateLockedError(destroyErr)
		if options.LockExpirationDuration == 0 || !ok || time.Since(lockErrInfo.Created) < options.LockExpirationDuration {
			return destroyErr
		}
		unlockErr := tf.ForceUnlock(ctx, lockErrInfo.ID)
		return eris.Join(destroyErr, unlockErr)
	}
	err = wait.RetryWithInterval(ctx, options.Retry, options.RetryInterval, destroy)
	if err != nil {
		return eris.Wrap(err, "failed to apply terraform config")
	}
	return nil
}

func (t *Terraform) terraformInitAndOutput(ctx context.Context, workingDir, backendPath, cliConfigPath, outputKey string, ignoreEmptyOutput bool, options OutputOptions) (*tfexec.OutputMeta, error) {
	tf, err := t.getTerraformExec(workingDir)
	if err != nil {
		return nil, eris.Wrap(err, "failed to create Terraform exec")
	}

	err = tfInit(ctx, tf, backendPath, cliConfigPath, options.InitOptions)
	if err != nil {
		return nil, eris.Wrap(err, "failed to init terraform")
	}

	var output map[string]tfexec.OutputMeta
	doOutput := func(ctx context.Context) error {
		output, err = tf.Output(ctx)
		return err
	}
	err = wait.RetryWithInterval(ctx, options.Retry, options.RetryInterval, doOutput)
	if err != nil {
		return nil, eris.Wrap(err, "failed to get terraform output")
	}
	if ignoreEmptyOutput && len(output) == 0 {
		return nil, nil
	}
	outputMeta, ok := output[outputKey]
	if !ok {
		return nil, eris.Errorf("missing key %v from terraform output", outputKey)
	}
	return &outputMeta, nil
}

func tfInit(ctx context.Context, tf Executor, backendPath, cliConfigPath string, options TFInitOptions) error {
	if cliConfigPath != "" {
		err := os.Setenv(tfCLIConfigFileEnvKey, cliConfigPath)
		if err != nil {
			return eris.Wrap(err, "failed to set up CLI config file env var")
		}
	}
	init := func(ctx context.Context) error {
		return tf.Init(ctx, tfexec.Upgrade(true), tfexec.BackendConfig(backendPath))
	}
	return wait.RetryWithInterval(ctx, options.Retry, options.RetryInterval, init)
}

// LockErrInfo represents the metadata of a Terraform lock.
type LockErrInfo struct {
	ID        string
	Path      string
	Operation string
	Who       string
	Version   string
	Created   time.Time
}

func extractStateLockedError(err error) (LockErrInfo, bool) {
	if err == nil {
		return LockErrInfo{}, false
	}
	if !stateLockErrRegexp.MatchString(err.Error()) {
		return LockErrInfo{}, false
	}
	submatches := stateLockInfoRegexp.FindStringSubmatch(err.Error())
	if len(submatches) == 7 {
		created, err := time.Parse(lockCreatedLayout, submatches[6])
		if err != nil {
			return LockErrInfo{}, false
		}
		return LockErrInfo{
			ID:        submatches[1],
			Path:      submatches[2],
			Operation: submatches[3],
			Who:       submatches[4],
			Version:   submatches[5],
			Created:   created,
		}, true
	}
	return LockErrInfo{}, false
}

func toVariableAssignments(variables map[string]string) []string {
	var assignments []string
	for k, v := range variables {
		assignments = append(assignments, fmt.Sprintf("%s=%s", k, v))
	}
	return assignments
}
