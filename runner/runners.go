/*
 * Copyright 2018-2020 the original author or authors.
 *
 * Licensed under the Apache License, Version 2.0 (the "License");
 * you may not use this file except in compliance with the License.
 * You may obtain a copy of the License at
 *
 *      https://www.apache.org/licenses/LICENSE-2.0
 *
 * Unless required by applicable law or agreed to in writing, software
 * distributed under the License is distributed on an "AS IS" BASIS,
 * WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
 * See the License for the specific language governing permissions and
 * limitations under the License.
 */

package runner

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"runtime"
	"strings"

	"github.com/buildpacks/libcnb"
	"github.com/mattn/go-shellwords"
	"github.com/paketo-buildpacks/libpak"
	"github.com/paketo-buildpacks/libpak/bard"
	"github.com/paketo-buildpacks/libpak/effect"
	"github.com/paketo-buildpacks/libpak/sherpa"
)

//go:generate mockery --name CargoService --case underscore

type CargoService interface {
	Install(srcDir string, destLayer libcnb.Layer) error
	InstallMember(memberPath string, srcDir string, destLayer libcnb.Layer) error
	InstallTool(name string, additionalArgs []string) error
	WorkspaceMembers(srcDir string, destLayer libcnb.Layer) ([]url.URL, error)
	ProjectTargets(srcDir string) ([]string, error)
	CleanCargoHomeCache() error
	CargoVersion() (string, error)
	RustVersion() (string, error)
}

const (
	StaticTypeMUSLC   = "muslc"
	StaticTypeGNULIBC = "gnulibc"
)

// Option is a function for configuring a CargoRunner
type Option func(runner CargoRunner) CargoRunner

// WithCargoHome sets CARGO_HOME
func WithCargoHome(cargoHome string) Option {
	return func(runner CargoRunner) CargoRunner {
		runner.CargoHome = cargoHome
		return runner
	}
}

// WithCargoWorkspaceMembers sets a comma separate list of workspace members
func WithCargoWorkspaceMembers(cargoWorkspaceMembers string) Option {
	return func(runner CargoRunner) CargoRunner {
		runner.CargoWorkspaceMembers = cargoWorkspaceMembers
		return runner
	}
}

// WithCargoInstallArgs sets addition args to pass to cargo install
func WithCargoInstallArgs(installArgs string) Option {
	return func(runner CargoRunner) CargoRunner {
		runner.CargoInstallArgs = installArgs
		return runner
	}
}

// WithExecutor sets the executor to use when running cargo
func WithExecutor(executor effect.Executor) Option {
	return func(runner CargoRunner) CargoRunner {
		runner.Executor = executor
		return runner
	}
}

// WithLogger sets additional args to pass to cargo install
func WithLogger(logger bard.Logger) Option {
	return func(runner CargoRunner) CargoRunner {
		runner.Logger = logger
		return runner
	}
}

// WithStack sets the stack on which we're running
func WithStack(stack string) Option {
	return func(runner CargoRunner) CargoRunner {
		runner.Stack = stack
		return runner
	}
}

// WithStaticType sets the static type to use
func WithStaticType(staticType string) Option {
	return func(runner CargoRunner) CargoRunner {
		runner.StaticType = staticType
		return runner
	}
}

// CargoRunner can execute cargo via CLI
type CargoRunner struct {
	CargoHome             string
	CargoWorkspaceMembers string
	CargoInstallArgs      string
	Executor              effect.Executor
	Logger                bard.Logger
	Stack                 string
	StaticType            string
}

type metadataTarget struct {
	Kind       []string `json:"kind"`
	CrateTypes []string `json:"crate_types"`
	Name       string   `json:"name"`
	SrcPath    string   `json:"src_path"`
	Edition    string   `json:"edition"`
	Doc        bool     `json:"doc"`
	Doctest    bool     `json:"doctest"`
	Test       bool     `json:"test"`
}

type metadataPackage struct {
	ID      string
	Targets []metadataTarget `json:"targets"`
}

type metadata struct {
	Packages         []metadataPackage `json:"packages"`
	WorkspaceMembers []string          `json:"workspace_members"`
}

// NewCargoRunner creates a new cargo runner with the given options
func NewCargoRunner(options ...Option) CargoRunner {
	runner := CargoRunner{}

	for _, option := range options {
		runner = option(runner)
	}

	return runner
}

// Install will build and install the project using `cargo install`
func (c CargoRunner) Install(srcDir string, destLayer libcnb.Layer) error {
	return c.InstallMember(".", srcDir, destLayer)
}

// InstallMember will build and install a specific workspace member using `cargo install`
func (c CargoRunner) InstallMember(memberPath string, srcDir string, destLayer libcnb.Layer) error {
	// makes warning from `cargo install` go away
	path := os.Getenv("PATH")
	if path != "" && !strings.Contains(path, destLayer.Path) {
		path = sherpa.AppendToEnvVar("PATH", ":", filepath.Join(destLayer.Path, "bin"))
		err := os.Setenv("PATH", path)
		if err != nil {
			return fmt.Errorf("unable to update PATH\n%w", err)
		}
	}

	args, err := c.BuildArgs(destLayer, memberPath)
	if err != nil {
		return fmt.Errorf("unable to build args\n%w", err)
	}

	c.Logger.Bodyf("cargo %s", strings.Join(args, " "))
	if err := c.Executor.Execute(effect.Execution{
		Command: "cargo",
		Args:    args,
		Dir:     srcDir,
		Stdout:  bard.NewWriter(c.Logger.Logger.InfoWriter(), bard.WithIndent(3)),
		Stderr:  bard.NewWriter(c.Logger.Logger.InfoWriter(), bard.WithIndent(3)),
	}); err != nil {
		return fmt.Errorf("unable to build\n%w", err)
	}

	err = c.CleanCargoHomeCache()
	if err != nil {
		return fmt.Errorf("unable to cleanup: %w", err)
	}
	return nil
}

func (c CargoRunner) InstallTool(name string, additionalArgs []string) error {
	args := []string{"install", name}
	args = append(args, additionalArgs...)

	c.Logger.Bodyf("cargo %s", strings.Join(args, " "))
	if err := c.Executor.Execute(effect.Execution{
		Command: "cargo",
		Args:    args,
		Stdout:  bard.NewWriter(c.Logger.Logger.InfoWriter(), bard.WithIndent(3)),
		Stderr:  bard.NewWriter(c.Logger.Logger.InfoWriter(), bard.WithIndent(3)),
	}); err != nil {
		return fmt.Errorf("unable to install tool\n%w", err)
	}

	return nil
}

// WorkspaceMembers loads the members from the project workspace
func (c CargoRunner) WorkspaceMembers(srcDir string, destLayer libcnb.Layer) ([]url.URL, error) {
	m, err := c.fetchCargoMetadata(srcDir)
	if err != nil {
		return []url.URL{}, fmt.Errorf("unable to load cargo metadata\n%w", err)
	}

	filterMap := c.makeFilterMap()

	var paths []url.URL
	for _, workspace := range m.WorkspaceMembers {
		pkgName, _, pathUrl, err := ParseWorkspaceMember(workspace)
		if err != nil {
			return nil, fmt.Errorf("unable to parse: %w", err)
		}

		if len(filterMap) > 0 && filterMap[strings.TrimSpace(pkgName)] || len(filterMap) == 0 {
			path, err := url.Parse(pathUrl)
			if err != nil {
				return nil, fmt.Errorf("unable to parse path URL %s: %w", workspace, err)
			}
			paths = append(paths, *path)
		}
	}

	return paths, nil
}

// parseWorkspaceMember parses a workspace member which can be in a couple of different formats
//
//		pre-1.77: `package-name package-version (url)`, like `function 0.1.0 (path+file:///Users/dmikusa/Downloads/fn-rs)`
//		1.77+:
//	     - `url#package-name@package-version` like `path+file:///Users/dmikusa/Downloads/fn-rs#function@0.1.0`
//	     - `url#version` for local packages where the workspace member name is equal to the directory name like `path+file:///Users/jondoe/.../services/example-transform#0.1.0`
//
// The final directory is assumed to be the package name with the local package format.
// returns the package name, version, URL, and optional error in that order
func ParseWorkspaceMember(workspaceMember string) (string, string, string, error) {
	if strings.HasPrefix(workspaceMember, "path+file://") {
		half := strings.SplitN(workspaceMember, "#", 2)
		if len(half) != 2 {
			return "", "", "", fmt.Errorf("unable to parse workspace member [%s], missing `#`", workspaceMember)
		}

		otherHalf := strings.SplitN(half[1], "@", 2)
		if len(otherHalf) == 2 {
			return strings.TrimSpace(otherHalf[0]), strings.TrimSpace(otherHalf[1]), strings.TrimSpace(half[0]), nil
		} else {
			splitIndex := strings.LastIndex(half[0], "/")
			path := half[0][:splitIndex]
			pkgName := half[0][splitIndex+1:]
			return strings.TrimSpace(pkgName), strings.TrimSpace(half[1]), strings.TrimSpace(path), nil
		}

	} else {
		// This is OK because the workspace member format is `package-name package-version (url)` and
		//   none of name, version or URL may contain a space & be valid
		parts := strings.SplitN(workspaceMember, " ", 3)
		if len(parts) != 3 {
			return "", "", "", fmt.Errorf("unable to parse workspace member [%s], unexpected format", workspaceMember)
		}
		return strings.TrimSpace(parts[0]), strings.TrimSpace(parts[1]), strings.TrimSuffix(strings.TrimPrefix(parts[2], "("), ")"), nil
	}
}

// ProjectTargets loads the members from the project workspace
func (c CargoRunner) ProjectTargets(srcDir string) ([]string, error) {
	m, err := c.fetchCargoMetadata(srcDir)
	if err != nil {
		return []string{}, fmt.Errorf("unable to load cargo metadata\n%w", err)
	}

	filterMap := c.makeFilterMap()

	workspaces := []string{}
	for _, workspace := range m.WorkspaceMembers {
		// This is OK because the workspace member format is `package-name package-version (url)` and
		//   none of name, version or URL may contain a space & be valid
		parts := strings.SplitN(workspace, " ", 3)
		if len(filterMap) > 0 && filterMap[strings.TrimSpace(parts[0])] || len(filterMap) == 0 {
			workspaces = append(workspaces, workspace)
		}
	}

	var names []string
	for _, pkg := range m.Packages {
		for _, workspace := range workspaces {
			if pkg.ID == workspace {
				for _, target := range pkg.Targets {
					for _, kind := range target.Kind {
						if kind == "bin" && strings.HasPrefix(target.SrcPath, srcDir) {
							names = append(names, target.Name)
						}
					}
				}
			}
		}
	}

	return names, nil
}

// CleanCargoHomeCache clears out unnecessary files from under $CARGO_HOME
func (c CargoRunner) CleanCargoHomeCache() error {
	files, err := os.ReadDir(c.CargoHome)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return fmt.Errorf("unable to read directory\n%w", err)
	}

	for _, file := range files {
		if file.IsDir() && file.Name() == "bin" ||
			file.IsDir() && file.Name() == "registry" ||
			file.IsDir() && file.Name() == "git" {
			continue
		}
		err := os.RemoveAll(filepath.Join(c.CargoHome, file.Name()))
		if err != nil {
			return fmt.Errorf("unable to remove files\n%w", err)
		}
	}

	registryDir := filepath.Join(c.CargoHome, "registry")
	files, err = os.ReadDir(registryDir)
	if err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("unable to read directory\n%w", err)
	}

	for _, file := range files {
		if file.IsDir() && file.Name() == "index" ||
			file.IsDir() && file.Name() == "cache" {
			continue
		}
		err := os.RemoveAll(filepath.Join(registryDir, file.Name()))
		if err != nil {
			return fmt.Errorf("unable to remove files\n%w", err)
		}
	}

	gitDir := filepath.Join(c.CargoHome, "git")
	files, err = os.ReadDir(gitDir)
	if err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("unable to read directory\n%w", err)
	}

	for _, file := range files {
		if file.IsDir() && file.Name() == "db" {
			continue
		}
		err := os.RemoveAll(filepath.Join(gitDir, file.Name()))
		if err != nil {
			return fmt.Errorf("unable to remove files\n%w", err)
		}
	}

	return nil
}

// CargoVersion returns the version of cargo installed
func (c CargoRunner) CargoVersion() (string, error) {
	buf := &bytes.Buffer{}

	if err := c.Executor.Execute(effect.Execution{
		Command: "cargo",
		Args:    []string{"version"},
		Stdout:  buf,
		Stderr:  buf,
	}); err != nil {
		return "", fmt.Errorf("error executing 'cargo version':\n Combined Output: %s: \n%w", buf.String(), err)
	}

	s := strings.SplitN(strings.TrimSpace(buf.String()), " ", 3)
	return s[1], nil
}

// RustVersion returns the version of rustc installed
func (c CargoRunner) RustVersion() (string, error) {
	buf := &bytes.Buffer{}

	if err := c.Executor.Execute(effect.Execution{
		Command: "rustc",
		Args:    []string{"--version"},
		Stdout:  buf,
		Stderr:  buf,
	}); err != nil {
		return "", fmt.Errorf("error executing 'rustc --version':\n Combined Output: %s: \n%w", buf.String(), err)
	}

	s := strings.Split(strings.TrimSpace(buf.String()), " ")
	return s[1], nil
}

// BuildArgs will build the list of arguments to pass `cargo install`
func (c CargoRunner) BuildArgs(destLayer libcnb.Layer, defaultMemberPath string) ([]string, error) {
	envArgs, err := FilterInstallArgs(c.CargoInstallArgs)
	if err != nil {
		return nil, fmt.Errorf("filter failed: %w", err)
	}

	args := []string{"install"}
	args = append(args, envArgs...)
	args = append(args, "--color=never", fmt.Sprintf("--root=%s", destLayer.Path))
	args = AddDefaultPath(args, defaultMemberPath)

	args, err = AddDefaultTargetForTinyOrStatic(args, c.Stack, c.StaticType)
	if err != nil {
		return []string{}, fmt.Errorf("unable to add default target\n%w", err)
	}

	return args, nil
}

// FilterInstallArgs provides a clean list of allowed arguments
func FilterInstallArgs(args string) ([]string, error) {
	argwords, err := shellwords.Parse(args)
	if err != nil {
		return nil, fmt.Errorf("parse args failed: %w", err)
	}

	var filteredArgs []string
	skipNext := false
	for _, arg := range argwords {
		if skipNext {
			skipNext = false
			continue
		}
		if arg == "--root" || arg == "--color" {
			skipNext = true
			continue
		}
		if strings.HasPrefix(arg, "--root=") || strings.HasPrefix(arg, "--color=") {
			continue
		}
		filteredArgs = append(filteredArgs, arg)
	}

	return filteredArgs, nil
}

// AddDefaultPath will add --path=. if --path is not set
func AddDefaultPath(args []string, defaultMemberPath string) []string {
	for _, arg := range args {
		if arg == "--path" || strings.HasPrefix(arg, "--path=") {
			return args
		}
	}
	return append(args, fmt.Sprintf("--path=%s", defaultMemberPath))
}

// AddDefaultTargetForTinyOrStatic will add the appropriate options if not already set
func AddDefaultTargetForTinyOrStatic(args []string, stack string, staticType string) ([]string, error) {
	if !libpak.IsTinyStack(stack) && !libpak.IsStaticStack(stack) {
		return args, nil
	}

	// user already picked a target, back off
	for _, arg := range args {
		if arg == "--target" || strings.HasPrefix(arg, "--target=") {
			return args, nil
		}
	}

	// user set flags to do a static build, back off
	rustFlags := os.Getenv("RUSTFLAGS")
	if strings.Contains(rustFlags, "target-feature=+crt-static") {
		return args, nil
	}

	arch := archFromSystem()

	target := fmt.Sprintf("--target=%s-unknown-linux-musl", arch)
	if staticType == StaticTypeGNULIBC {
		target = fmt.Sprintf("--target=%s-unknown-linux-gnu", arch)

		rustFlagsList := []string{}
		if len(rustFlags) > 0 {
			rustFlagsList = append(rustFlagsList, rustFlags)
		}
		rustFlagsList = append(rustFlagsList, "-C target-feature=+crt-static")
		newRustFlags := strings.Join(rustFlagsList, " ")

		err := os.Setenv("RUSTFLAGS", newRustFlags)
		if err != nil {
			return []string{}, fmt.Errorf("unable to set env RUSTFLAGS to [%s]\n%w", newRustFlags, err)
		}
	}

	return append(args, target), nil
}

func (c CargoRunner) fetchCargoMetadata(srcDir string) (metadata, error) {
	stdout := bytes.Buffer{}
	stderr := bytes.Buffer{}

	if err := c.Executor.Execute(effect.Execution{
		Command: "cargo",
		Args:    []string{"metadata", "--format-version=1", "--no-deps"},
		Dir:     srcDir,
		Stdout:  &stdout,
		Stderr:  &stderr,
	}); err != nil {
		return metadata{}, fmt.Errorf("unable to read metadata: \n%s\n%s\n%w", &stdout, &stderr, err)
	}

	var m metadata
	if err := json.Unmarshal(stdout.Bytes(), &m); err != nil {
		return metadata{}, fmt.Errorf("unable to parse Cargo metadata: %w", err)
	}

	return m, nil
}

func (c CargoRunner) makeFilterMap() map[string]bool {
	filter := c.CargoWorkspaceMembers != ""
	filterMap := make(map[string]bool)
	if filter {
		if !strings.Contains(c.CargoWorkspaceMembers, ",") {
			filterMap[c.CargoWorkspaceMembers] = true
		}
		for _, f := range strings.Split(c.CargoWorkspaceMembers, ",") {
			filterMap[strings.TrimSpace(f)] = true
		}
	}

	return filterMap
}

func archFromSystem() string {
	archFromEnv, ok := os.LookupEnv("BP_ARCH")
	if !ok {
		archFromEnv = runtime.GOARCH
	}

	if archFromEnv == "amd64" {
		return "x86_64"
	} else if archFromEnv == "arm64" {
		return "aarch64"
	} else {
		return archFromEnv
	}
}
