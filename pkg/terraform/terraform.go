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

	"github.com/risingwavelabs/byoc-runtime/pkg/utils/wait"
)

var (
	stateLockErrRegexp  = regexp.MustCompile(`Error acquiring the state lock`)
	stateLockInfoRegexp = regexp.MustCompile(`Lock Info:\n\s*ID:\s*([^\n]+)\n\s*Path:\s*([^\n]+)\n\s*Operation:\s*([^\n]+)\n\s*Who:\s*([^\n]+)\n\s*Version:\s*([^\n]+)\n\s*Created:\s*([^\n]+)\n`)
)

const (
	lockCreatedLayout = "2006-01-02 15:04:05.999999999 -0700 MST"
)

// Terraform wraps BYOC terraform management logics.
type Terraform struct {
	// ModulePath is the relative path of the file storing TF version to the workspace root path
	tfVersionFilePath      string
	tfExecPath             string
	rootPath               string
	packageURL             string
	packageName            string
	privateTFBinaryBaseURL string // If empty, use the default public endpoint to download TF binary.
}

// NewTerraformOptions contains all options for Terraform initialization.
type NewTerraformOptions struct {
	RootPath               string
	TFVersionFilePath      string
	PackageURL             string
	PackageDestName        string
	PrivateTFBinaryBaseURL string
}

// New initilizes a new Terraform.
func New(ctx context.Context, options NewTerraformOptions) (*Terraform, error) {
	t := &Terraform{
		tfVersionFilePath:      options.TFVersionFilePath,
		rootPath:               options.RootPath,
		packageURL:             options.PackageURL,
		packageName:            options.PackageDestName,
		privateTFBinaryBaseURL: options.PrivateTFBinaryBaseURL,
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

// ApplyModule applies a Terraform module.
func (t *Terraform) ApplyModule(ctx context.Context, moduleOptions ModuleOptions, applyOptions ApplyOptions) error {
	absModulePath := fmt.Sprintf("%s/%s", t.rootPath, moduleOptions.ModulePath)
	backendCfgPath := fmt.Sprintf("%s/%s", absModulePath, moduleOptions.BackendConfigFileName)
	err := os.WriteFile(backendCfgPath, []byte(moduleOptions.BackendConfig), 0666)
	if err != nil {
		return eris.Wrapf(err, "failed to write tf backend config to %v", backendCfgPath)
	}

	variablePath := fmt.Sprintf("%s/%s", absModulePath, moduleOptions.VariableFileName)
	err = os.WriteFile(variablePath, []byte(moduleOptions.VariablePayload), 0666)
	if err != nil {
		return eris.Wrapf(err, "failed to write tf variable payloads to %v", variablePath)
	}

	err = t.terraformInitAndApply(
		ctx,
		absModulePath,
		backendCfgPath,
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
	absModulePath := fmt.Sprintf("%s/%s", t.rootPath, moduleOptions.ModulePath)
	backendCfgPath := fmt.Sprintf("%s/%s", absModulePath, moduleOptions.BackendConfigFileName)
	err := os.WriteFile(backendCfgPath, []byte(moduleOptions.BackendConfig), 0666)
	if err != nil {
		return eris.Wrapf(err, "failed to write tf backend config to %v", backendCfgPath)
	}

	variablePath := fmt.Sprintf("%s/%s", absModulePath, moduleOptions.VariableFileName)
	err = os.WriteFile(variablePath, []byte(moduleOptions.VariablePayload), 0666)
	if err != nil {
		return eris.Wrapf(err, "failed to write tf variable payloads to %v", variablePath)
	}

	err = t.terraformInitAndDestroy(
		ctx,
		absModulePath,
		backendCfgPath,
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
	absModulePath := fmt.Sprintf("%s/%s", t.rootPath, moduleOptions.ModulePath)
	backendCfgPath := fmt.Sprintf("%s/%s", absModulePath, moduleOptions.BackendConfigFileName)
	err := os.WriteFile(backendCfgPath, []byte(moduleOptions.BackendConfig), 0666)
	if err != nil {
		return nil, eris.Wrapf(err, "failed to write tf backend config to %v", backendCfgPath)
	}

	variablePath := fmt.Sprintf("%s/%s", absModulePath, moduleOptions.VariableFileName)
	err = os.WriteFile(variablePath, []byte(moduleOptions.VariablePayload), 0666)
	if err != nil {
		return nil, eris.Wrapf(err, "failed to write tf variable payloads to %v", variablePath)
	}
	rawOutput, err := t.terraformInitAndOutput(
		ctx,
		absModulePath,
		backendCfgPath,
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
func (t *Terraform) RetrieveModuleOutputOrNil(ctx context.Context, outputKey string, options ModuleOptions, outputOptions OutputOptions) (json.RawMessage, error) {
	absModulePath := fmt.Sprintf("%s/%s", t.rootPath, options.ModulePath)
	backendCfgPath := fmt.Sprintf("%s/%s", absModulePath, options.BackendConfigFileName)
	err := os.WriteFile(backendCfgPath, []byte(options.BackendConfig), 0666)
	if err != nil {
		return nil, eris.Wrapf(err, "failed to write tf backend config to %v", backendCfgPath)
	}

	variablePath := fmt.Sprintf("%s/%s", absModulePath, options.VariableFileName)
	err = os.WriteFile(variablePath, []byte(options.VariablePayload), 0666)
	if err != nil {
		return nil, eris.Wrapf(err, "failed to write tf variable payloads to %v", variablePath)
	}
	rawOutput, err := t.terraformInitAndOutput(
		ctx,
		absModulePath,
		backendCfgPath,
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
	tfExecPath, err := installTerraform(ctx, t.rootPath, tfVersion, t.privateTFBinaryBaseURL)
	if err != nil {
		return eris.Wrapf(err, "failed to initialize Terraform, version: %v", tfVersion)
	}
	t.tfExecPath = tfExecPath
	return nil
}

func (t *Terraform) prepareTerraformPackage(ctx context.Context) error {
	// will download the file from the remote.
	packagePath := fmt.Sprintf("%s/%s", t.rootPath, t.packageName)
	if err := downloadFile(ctx, t.packageURL, packagePath); err != nil {
		return eris.Wrap(err, "failed to download Terraform modules")
	}
	if err := unzipFile(packagePath, t.rootPath); err != nil {
		return eris.Wrap(err, "failed to decompress Terraform modules")
	}
	if err := os.Remove(packagePath); err != nil {
		return eris.Wrap(err, "failed to clean up Terraform modules zip")
	}
	return nil
}

func downloadFile(ctx context.Context, url, destination string) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return err
	}
	response, err := http.DefaultClient.Do(req)
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

func installTerraform(ctx context.Context, dir, tfVersion, privateTFBinaryBaseURL string) (string, error) {
	version, err := version.NewVersion(tfVersion)
	if err != nil {
		return "", eris.Wrapf(err, "failed to get terraform version %v", tfVersion)
	}
	installer := &releases.ExactVersion{
		Product:    product.Terraform,
		Version:    version,
		InstallDir: dir,
		ApiBaseURL: privateTFBinaryBaseURL,
	}

	execPath, err := installer.Install(ctx)
	if err != nil {
		return "", eris.Wrap(err, "error installing Terraform")
	}
	return execPath, nil
}

func (t *Terraform) getTerraformExec(workingDir string) (*tfexec.Terraform, error) {
	tf, err := tfexec.NewTerraform(workingDir, t.tfExecPath)
	if err != nil {
		return nil, err
	}
	return tf, nil
}

func (t *Terraform) terraformInitAndApply(ctx context.Context, workingDir, backendPath string, sensitiveVariables map[string]string, options ApplyOptions) error {
	tf, err := t.getTerraformExec(workingDir)
	if err != nil {
		return eris.Wrap(err, "failed to create Terraform exec")
	}

	tf.SetStdout(options.StdOut)
	tf.SetStderr(options.StdErr)

	err = tfInit(ctx, tf, backendPath, options.InitOptions)
	if err != nil {
		return eris.Wrap(err, "failed to init terraform")
	}

	tfexecApplyOptions := []tfexec.ApplyOption{
		tfexec.GracefulShutdown(tfexec.GracefulShutdownConfig{
			Enable: true,
			Period: options.GracefulShutdownPeriod,
		}),
	}
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

func (t *Terraform) terraformInitAndDestroy(ctx context.Context, workingDir, backendPath string, sensitiveVariables map[string]string, options DestroyOptions) error {
	tf, err := t.getTerraformExec(workingDir)
	if err != nil {
		return eris.Wrap(err, "failed to create Terraform exec")
	}

	tf.SetStdout(options.StdOut)
	tf.SetStderr(options.StdErr)

	err = tfInit(ctx, tf, backendPath, options.InitOptions)
	if err != nil {
		return eris.Wrap(err, "failed to init terraform")
	}

	tfexecDestroyOptions := []tfexec.DestroyOption{
		tfexec.GracefulShutdown(tfexec.GracefulShutdownConfig{
			Enable: true,
			Period: options.GracefulShutdownPeriod,
		}),
	}
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

func (t *Terraform) terraformInitAndOutput(ctx context.Context, workingDir, backendPath, outputKey string, ignoreEmptyOutput bool, options OutputOptions) (*tfexec.OutputMeta, error) {
	tf, err := t.getTerraformExec(workingDir)
	if err != nil {
		return nil, eris.Wrap(err, "failed to create Terraform exec")
	}

	err = tfInit(ctx, tf, backendPath, options.InitOptions)
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

func tfInit(ctx context.Context, tf *tfexec.Terraform, backendPath string, options TFInitOptions) error {
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
