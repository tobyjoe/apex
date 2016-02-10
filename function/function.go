// Package function implements function-level operations.
package function

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/ioutil"
	"os"
	"path/filepath"
	"strings"

	"github.com/apex/log"
	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/aws/awserr"
	"github.com/aws/aws-sdk-go/service/lambda"
	"github.com/aws/aws-sdk-go/service/lambda/lambdaiface"
	"github.com/dustin/go-humanize"
	"github.com/jpillora/archive"
	"gopkg.in/validator.v2"

	"github.com/apex/apex/hooks"
	"github.com/apex/apex/utils"
)

// defaultPlugins are the default plugins which are required by Apex. Note that
// the order here is important for some plugins such as inference before the
// runtimes.
var defaultPlugins = []string{
	"inference",
	"golang",
	"python",
	"nodejs",
	"hooks",
	"env",
	"shim",
}

// InvocationType determines how an invocation request is made.
type InvocationType string

// Invocation types.
const (
	RequestResponse InvocationType = "RequestResponse"
	Event                          = "Event"
	DryRun                         = "DryRun"
)

// CurrentAlias name.
const CurrentAlias = "current"

// InvokeError records an error from an invocation.
type InvokeError struct {
	Message string   `json:"errorMessage"`
	Type    string   `json:"errorType"`
	Stack   []string `json:"stackTrace"`
	Handled bool
}

// Error message.
func (e *InvokeError) Error() string {
	return e.Message
}

// Config for a Lambda function.
type Config struct {
	Description      string            `json:"description"`
	Runtime          string            `json:"runtime" validate:"nonzero"`
	Memory           int64             `json:"memory" validate:"nonzero"`
	Timeout          int64             `json:"timeout" validate:"nonzero"`
	Role             string            `json:"role" validate:"nonzero"`
	Handler          string            `json:"handler" validate:"nonzero"`
	Shim             bool              `json:"shim"`
	Environment      map[string]string `json:"environment"`
	Hooks            hooks.Hooks       `json:"hooks"`
	RetainedVersions int               `json:"retainedVersions"`
}

// Function represents a Lambda function, with configuration loaded
// from the "function.json" file on disk.
type Function struct {
	Config
	Name            string
	FunctionName    string
	Path            string
	Service         lambdaiface.LambdaAPI
	Log             log.Interface
	IgnoredPatterns []string
	Plugins         []string
}

// Open the function.json file and prime the config.
func (f *Function) Open() error {
	f.Log = f.Log.WithField("function", f.Name)

	if f.Plugins == nil {
		f.Plugins = defaultPlugins
	}

	if f.Environment == nil {
		f.Environment = make(map[string]string)
	}

	p, err := os.Open(filepath.Join(f.Path, "function.json"))
	if err == nil {
		if err := json.NewDecoder(p).Decode(&f.Config); err != nil {
			return err
		}
	}

	if err := f.hookOpen(); err != nil {
		return err
	}

	if err := validator.Validate(&f.Config); err != nil {
		return fmt.Errorf("error opening function %s: %s", f.Name, err.Error())
	}

	patterns, err := utils.ReadIgnoreFile(f.Path)
	if err != nil {
		return err
	}
	f.IgnoredPatterns = append(f.IgnoredPatterns, patterns...)

	return nil
}

// Setenv sets environment variable `name` to `value`.
func (f *Function) Setenv(name, value string) {
	f.Environment[name] = value
}

// Deploy generates a zip and creates or deploy the function.
// If the configuration hasn't been changed it will deploy only code,
// otherwise it will deploy both configuration and code.
func (f *Function) Deploy() error {
	f.Log.Info("deploying")

	zip, err := f.BuildBytes()
	if err != nil {
		return err
	}

	if err := f.hookDeploy(); err != nil {
		return err
	}

	config, err := f.GetConfig()
	if e, ok := err.(awserr.Error); ok {
		if e.Code() == "ResourceNotFoundException" {
			return f.Create(zip)
		}
	}
	if err != nil {
		return err
	}

	if f.configChanged(config) {
		f.Log.Info("config changed")
		return f.DeployConfigAndCode(zip)
	}

	f.Log.Info("config unchanged")
	return f.DeployCode(zip, config)
}

// DeployCode deploys function code when changed.
func (f *Function) DeployCode(zip []byte, config *lambda.GetFunctionOutput) error {
	remoteHash := *config.Configuration.CodeSha256
	localHash := utils.Sha256(zip)

	if localHash == remoteHash {
		f.Log.Info("code unchanged")
		return nil
	}

	f.Log.WithFields(log.Fields{
		"local":  localHash,
		"remote": remoteHash,
	}).Debug("code changed")

	return f.Update(zip)
}

// DeployConfigAndCode updates config and updates function code.
func (f *Function) DeployConfigAndCode(zip []byte) error {
	f.Log.Info("updating config")

	_, err := f.Service.UpdateFunctionConfiguration(&lambda.UpdateFunctionConfigurationInput{
		FunctionName: &f.FunctionName,
		MemorySize:   &f.Memory,
		Timeout:      &f.Timeout,
		Description:  &f.Description,
		Role:         &f.Role,
		Handler:      &f.Handler,
	})
	if err != nil {
		return err
	}

	return f.Update(zip)
}

// Delete the function including all its versions
func (f *Function) Delete() error {
	f.Log.Info("deleting")
	_, err := f.Service.DeleteFunction(&lambda.DeleteFunctionInput{
		FunctionName: &f.FunctionName,
	})
	return err
}

// GetConfig returns the function configuration.
func (f *Function) GetConfig() (*lambda.GetFunctionOutput, error) {
	f.Log.Debug("fetching config")
	return f.Service.GetFunction(&lambda.GetFunctionInput{
		FunctionName: &f.FunctionName,
	})
}

// GetConfigQualifier returns the function configuration for the given qualifier.
func (f *Function) GetConfigQualifier(s string) (*lambda.GetFunctionOutput, error) {
	f.Log.Debug("fetching config")
	return f.Service.GetFunction(&lambda.GetFunctionInput{
		FunctionName: &f.FunctionName,
		Qualifier:    &s,
	})
}

// GetConfigCurrent returns the function configuration for the current version.
func (f *Function) GetConfigCurrent() (*lambda.GetFunctionOutput, error) {
	return f.GetConfigQualifier(CurrentAlias)
}

// Update the function with the given `zip`.
func (f *Function) Update(zip []byte) error {
	f.Log.Info("updating function")

	updated, err := f.Service.UpdateFunctionCode(&lambda.UpdateFunctionCodeInput{
		FunctionName: &f.FunctionName,
		Publish:      aws.Bool(true),
		ZipFile:      zip,
	})

	if err != nil {
		return err
	}

	f.Log.Info("updating alias")

	_, err = f.Service.UpdateAlias(&lambda.UpdateAliasInput{
		FunctionName:    &f.FunctionName,
		Name:            aws.String(CurrentAlias),
		FunctionVersion: updated.Version,
	})
	if err != nil {
		return nil
	}

	f.Log.WithFields(log.Fields{
		"version": *updated.Version,
		"name":    f.FunctionName,
	}).Info("function updated")

	err = f.CleanupVersions()
	if err != nil {
		return nil
	}

	return nil
}

// Create the function with the given `zip`.
func (f *Function) Create(zip []byte) error {
	f.Log.Info("creating function")

	created, err := f.Service.CreateFunction(&lambda.CreateFunctionInput{
		FunctionName: &f.FunctionName,
		Description:  &f.Description,
		MemorySize:   &f.Memory,
		Timeout:      &f.Timeout,
		Runtime:      &f.Runtime,
		Handler:      &f.Handler,
		Role:         &f.Role,
		Publish:      aws.Bool(true),
		Code: &lambda.FunctionCode{
			ZipFile: zip,
		},
	})

	if err != nil {
		return err
	}

	f.Log.Info("creating alias")

	_, err = f.Service.CreateAlias(&lambda.CreateAliasInput{
		FunctionName:    &f.FunctionName,
		FunctionVersion: created.Version,
		Name:            aws.String(CurrentAlias),
	})

	if err != nil {
		return nil
	}

	f.Log.WithFields(log.Fields{
		"version": *created.Version,
		"name":    f.FunctionName,
	}).Info("function created")

	return nil
}

// Invoke the remote Lambda function, returning the response and logs, if any.
func (f *Function) Invoke(event, context interface{}) (reply, logs io.Reader, err error) {
	eventBytes, err := json.Marshal(event)
	if err != nil {
		return nil, nil, err
	}

	contextBytes, err := json.Marshal(context)
	if err != nil {
		return nil, nil, err
	}

	res, err := f.Service.Invoke(&lambda.InvokeInput{
		ClientContext:  aws.String(base64.StdEncoding.EncodeToString(contextBytes)),
		FunctionName:   &f.FunctionName,
		InvocationType: aws.String(string(RequestResponse)),
		LogType:        aws.String("Tail"),
		Qualifier:      aws.String(CurrentAlias),
		Payload:        eventBytes,
	})

	if err != nil {
		return nil, nil, err
	}

	logs = base64.NewDecoder(base64.StdEncoding, strings.NewReader(*res.LogResult))

	if res.FunctionError != nil {
		e := &InvokeError{
			Handled: *res.FunctionError == "Handled",
		}

		if err := json.Unmarshal(res.Payload, e); err != nil {
			return nil, logs, err
		}

		return nil, logs, e
	}

	reply = bytes.NewReader(res.Payload)
	return reply, logs, nil
}

// Rollback the function to the previous.
func (f *Function) Rollback() error {
	f.Log.Info("rolling back")

	alias, err := f.currentVersionAlias()
	if err != nil {
		return err
	}

	f.Log.Infof("current version: %s", *alias.FunctionVersion)

	versions, err := f.versions()
	if err != nil {
		return err
	}

	if len(versions) < 2 {
		return errors.New("Can't rollback. Only one version deployed.")
	}

	latest := *versions[len(versions)-1].Version
	prev := *versions[len(versions)-2].Version
	rollback := latest

	if *alias.FunctionVersion == latest {
		rollback = prev
	}

	f.Log.Infof("rollback to version: %s", rollback)

	_, err = f.Service.UpdateAlias(&lambda.UpdateAliasInput{
		FunctionName:    &f.FunctionName,
		Name:            aws.String(CurrentAlias),
		FunctionVersion: &rollback,
	})

	return err
}

// RollbackVersion the function to the specified version.
func (f *Function) RollbackVersion(version string) error {
	f.Log.Info("rolling back")

	alias, err := f.currentVersionAlias()
	if err != nil {
		return err
	}

	f.Log.Infof("current version: %s", *alias.FunctionVersion)

	if version == *alias.FunctionVersion {
		return errors.New("Specified version currently deployed.")
	}

	_, err = f.Service.UpdateAlias(&lambda.UpdateAliasInput{
		FunctionName:    &f.FunctionName,
		Name:            aws.String(CurrentAlias),
		FunctionVersion: &version,
	})

	return err
}

// BuildBytes returns the generated zip as bytes.
func (f *Function) BuildBytes() ([]byte, error) {
	r, err := f.Build()
	if err != nil {
		return nil, err
	}

	b, err := ioutil.ReadAll(r)
	if err != nil {
		return nil, err
	}

	f.Log.Infof("created build (%s)", humanize.Bytes(uint64(len(b))))
	return b, nil
}

// Build returns the zipped contents of the function.
func (f *Function) Build() (io.Reader, error) {
	f.Log.Debugf("creating build")

	buf := new(bytes.Buffer)
	zip := archive.NewZipWriter(buf)

	if err := f.hookBuild(zip); err != nil {
		return nil, err
	}

	files, err := utils.LoadFiles(f.Path, f.IgnoredPatterns)
	if err != nil {
		return nil, err
	}

	for _, path := range files {
		f.Log.WithField("file", path).Debug("add file to zip")

		file, err := os.Open(filepath.Join(f.Path, path))
		if err != nil {
			return nil, err
		}

		if err := zip.AddFile(path, file); err != nil {
			return nil, err
		}

		if err := file.Close(); err != nil {
			return nil, err
		}
	}

	if err := zip.Close(); err != nil {
		return nil, err
	}

	return buf, nil
}

// Clean invokes the CleanHook, useful for removing build artifacts and so on.
func (f *Function) Clean() error {
	return f.hookClean()
}

//CleanupVersions removes old function's versions retaing only f.RetainedVersions number
func (f *Function) CleanupVersions() error {
	versions, err := f.versions()
	if err != nil {
		return err
	}

	skip := f.RetainedVersions + 1 // skip current and retained

	if len(versions) > skip {
		versions = versions[:len(versions)-skip]
		for _, v := range versions {
			f.Log.Infof("cleaning up version: %s", *v.Version)

			_, err := f.Service.DeleteFunction(&lambda.DeleteFunctionInput{
				FunctionName: &f.FunctionName,
				Qualifier:    v.Version,
			})
			if err != nil {
				return err
			}
		}
	}

	return nil
}

// GroupName returns the CloudWatchLogs group name.
func (f *Function) GroupName() string {
	return fmt.Sprintf("/aws/lambda/%s", f.FunctionName)
}

// versions returns list of all versions deployed to AWS Lambda
func (f *Function) versions() ([]*lambda.FunctionConfiguration, error) {
	list, err := f.Service.ListVersionsByFunction(&lambda.ListVersionsByFunctionInput{
		FunctionName: &f.FunctionName,
	})

	if err != nil {
		return nil, err
	}

	versions := list.Versions[1:] // remove $LATEST

	return versions, nil
}

// currentVersionAlias returns alias configuration for currently deployed function
func (f *Function) currentVersionAlias() (*lambda.AliasConfiguration, error) {
	return f.Service.GetAlias(&lambda.GetAliasInput{
		FunctionName: &f.FunctionName,
		Name:         aws.String(CurrentAlias),
	})
}

// configChanged checks if function configuration differs from configuration stored in AWS Lambda
func (f *Function) configChanged(config *lambda.GetFunctionOutput) bool {
	if f.Description != *config.Configuration.Description {
		return true
	}

	if f.Memory != *config.Configuration.MemorySize {
		return true
	}

	if f.Timeout != *config.Configuration.Timeout {
		return true
	}

	if f.Role != *config.Configuration.Role {
		return true
	}

	if f.Handler != *config.Configuration.Handler {
		return true
	}

	return false
}

// hookOpen calls Openers.
func (f *Function) hookOpen() error {
	for _, name := range f.Plugins {
		if p, ok := plugins[name].(Opener); ok {
			if err := p.Open(f); err != nil {
				return err
			}
		}
	}
	return nil
}

// hookBuild calls Builders.
func (f *Function) hookBuild(zip *archive.Archive) error {
	for _, name := range f.Plugins {
		if p, ok := plugins[name].(Builder); ok {
			if err := p.Build(f, zip); err != nil {
				return err
			}
		}
	}
	return nil
}

// hookClean calls Cleaners.
func (f *Function) hookClean() error {
	for _, name := range f.Plugins {
		if p, ok := plugins[name].(Cleaner); ok {
			if err := p.Clean(f); err != nil {
				return err
			}
		}
	}
	return nil
}

// hookDeploy calls Deployers.
func (f *Function) hookDeploy() error {
	for _, name := range f.Plugins {
		if p, ok := plugins[name].(Deployer); ok {
			if err := p.Deploy(f); err != nil {
				return err
			}
		}
	}
	return nil
}
