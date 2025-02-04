// Copyright 2020 Matthew Holt
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package xk6

import (
	"bytes"
	"context"
	"fmt"
	"html/template"
	"io/ioutil"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
)

func (b Builder) newEnvironment(ctx context.Context) (*environment, error) {
	k6ModulePath, err := versionedModulePath(defaultK6ModulePath, b.K6Version)
	if err != nil {
		return nil, err
	}

	// clean up any SIV-incompatible module paths real quick
	for i, p := range b.Extensions {
		b.Extensions[i].PackagePath, err = versionedModulePath(p.PackagePath, p.Version)
		if err != nil {
			return nil, err
		}
	}

	// create the context for the main module template
	tplCtx := goModTemplateContext{
		K6Module: k6ModulePath,
	}
	// evaluate the template for the main module
	var buf bytes.Buffer
	tpl, err := template.New("main").Parse(mainModuleTemplate)
	if err != nil {
		return nil, err
	}
	err = tpl.Execute(&buf, tplCtx)
	if err != nil {
		return nil, err
	}

	// create the folder in which the build environment will operate
	tempFolder, err := newTempFolder()
	if err != nil {
		return nil, err
	}
	env := &environment{
		k6Version:    b.K6Version,
		extensions:   b.Extensions,
		k6ModulePath: k6ModulePath,
		tempFolder:   tempFolder,
		timeoutGoGet: b.TimeoutGet,
		skipCleanup:  b.SkipCleanup,
	}
	defer func() {
		if err != nil {
			err2 := env.Close()
			if err2 != nil {
				err = fmt.Errorf("%w; additionally, cleaning up folder: %v", err, err2)
			}
		}
	}()
	log.Printf("[INFO] Temporary folder: %s", tempFolder)

	// initialize the go module
	log.Println("[INFO] Initializing Go module")
	cmd := env.newCommand("go", "mod", "init", "k6")
	err = env.runCommand(ctx, cmd, 10*time.Second)
	if err != nil {
		return nil, err
	}

	// specify module replacements before pinning versions
	replaced := make(map[string]string)
	for _, r := range b.Replacements {
		log.Printf("[INFO] Replace %s => %s", r.Old.String(), r.New.String())
		err = env.execGoModReplace(ctx, r.Old.Param(), r.New.Param())
		if err != nil {
			return nil, err
		}
		replaced[r.Old.String()] = r.New.String()
	}

	// check for early abort
	select {
	case <-ctx.Done():
		return nil, ctx.Err()
	default:
	}

	// pin versions by populating go.mod, first for k6 itself and then extensions
	log.Println("[INFO] Pinning versions")
	if b.K6Repo != "" {
		// building with a forked repo, so get the main one and replace it with
		// the fork
		err = env.execGoModRequire(ctx, k6ModulePath, "")
		if err != nil {
			return nil, err
		}
		replace := b.K6Repo
		if b.K6Version != "" {
			replace = fmt.Sprintf("%s@%s", b.K6Repo, b.K6Version)
		}
		err = env.execGoModReplace(ctx, k6ModulePath, replace)
		if err != nil {
			return nil, err
		}
	}
nextExt:
	for _, p := range b.Extensions {
		err = env.writeExtensionImportFile(p.PackagePath)
		if err != nil {
			return nil, err
		}
		// if module is locally available, do not "go get" it;
		// also note that we iterate and check prefixes, because
		// an extension package may be a subfolder of a module, i.e.
		// foo/a/extension is within module foo/a.
		for repl := range replaced {
			if strings.HasPrefix(p.PackagePath, repl) {
				continue nextExt
			}
		}
		err = env.execGoModRequire(ctx, p.PackagePath, p.Version)
		if err != nil {
			return nil, err
		}
		// check for early abort
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		default:
		}
	}
	// This is here as we could've not run go mod tidy due to a replace being the only extension
	err = env.execGoModTidy(ctx)
	if err != nil {
		return nil, err
	}

	// write the main module file to temporary folder
	// we do this last so we get the needed versions from all the replacements and extensions instead of k6 if possible
	mainPath := filepath.Join(tempFolder, "main.go")
	log.Printf("[INFO] Writing main module: %s", mainPath)
	err = ioutil.WriteFile(mainPath, buf.Bytes(), 0o600)
	if err != nil {
		return nil, err
	}

	// building with the default main repo
	if b.K6Repo == "" && env.k6Version != "" {
		// Only require a specific k6 version if provided. Otherwise extensions
		// will require a version they depend on, and Go's module resolution
		// algorithm will choose the highest one among all extensions.
		err = env.execGoModRequire(ctx, k6ModulePath, env.k6Version)
		if err != nil {
			return nil, err
		}
	}
	err = env.execGoModTidy(ctx)
	if err != nil {
		return nil, err
	}

	log.Println("[INFO] Build environment ready")

	return env, nil
}

type environment struct {
	k6Version    string
	extensions   []Dependency
	k6ModulePath string
	tempFolder   string
	timeoutGoGet time.Duration
	skipCleanup  bool
}

// Close cleans up the build environment, including deleting
// the temporary folder from the disk.
func (env environment) Close() error {
	if env.skipCleanup {
		log.Printf("[INFO] Skipping cleanup as requested; leaving folder intact: %s", env.tempFolder)
		return nil
	}
	log.Printf("[INFO] Cleaning up temporary folder: %s", env.tempFolder)
	return os.RemoveAll(env.tempFolder)
}

func (env environment) writeExtensionImportFile(packagePath string) error {
	fileContents := fmt.Sprintf(`package main
import _ %q
`, packagePath)
	filePath := filepath.Join(env.tempFolder, strings.ReplaceAll(packagePath, "/", "_")+".go")
	return ioutil.WriteFile(filePath, []byte(fileContents), 0o600)
}

func (env environment) newCommand(command string, args ...string) *exec.Cmd {
	cmd := exec.Command(command, args...)
	cmd.Dir = env.tempFolder
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	return cmd
}

func (env environment) runCommand(ctx context.Context, cmd *exec.Cmd, timeout time.Duration) error {
	log.Printf("[INFO] exec (timeout=%s): %+v ", timeout, cmd)

	if timeout > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, timeout)
		defer cancel()
	}

	// start the command; if it fails to start, report error immediately
	err := cmd.Start()
	if err != nil {
		return err
	}

	// wait for the command in a goroutine; the reason for this is
	// very subtle: if, in our select, we do `case cmdErr := <-cmd.Wait()`,
	// then that case would be chosen immediately, because cmd.Wait() is
	// immediately available (even though it blocks for potentially a long
	// time, it can be evaluated immediately). So we have to remove that
	// evaluation from the `case` statement.
	cmdErrChan := make(chan error)
	go func() {
		cmdErrChan <- cmd.Wait()
	}()

	// unblock either when the command finishes, or when the done
	// channel is closed -- whichever comes first
	select {
	case cmdErr := <-cmdErrChan:
		// process ended; report any error immediately
		return cmdErr
	case <-ctx.Done():
		// context was canceled, either due to timeout or
		// maybe a signal from higher up canceled the parent
		// context; presumably, the OS also sent the signal
		// to the child process, so wait for it to die
		select {
		case <-time.After(15 * time.Second):
			cmd.Process.Kill()
		case <-cmdErrChan:
		}
		return ctx.Err()
	}
}

// tidy the module to ensure go.mod will not have versions such as `latest`
func (env environment) execGoModTidy(ctx context.Context) error {
	tidyCmd := env.newCommand("go", "mod", "tidy", "-compat=1.17")
	return env.runCommand(ctx, tidyCmd, env.timeoutGoGet)
}

func (env environment) execGoModRequire(ctx context.Context, modulePath, moduleVersion string) error {
	mod := modulePath
	if moduleVersion != "" {
		mod += "@" + moduleVersion
	} else {
		mod += "@latest"
	}
	cmd := env.newCommand("go", "mod", "edit", "-require", mod)
	err := env.runCommand(ctx, cmd, env.timeoutGoGet)
	if err != nil {
		return err
	}
	return env.execGoModTidy(ctx)
}

func (env environment) execGoModReplace(ctx context.Context, modulePath, replaceRepo string) error {
	replace := fmt.Sprintf("%s=%s", modulePath, replaceRepo)
	cmd := env.newCommand("go", "mod", "edit", "-replace", replace)
	err := env.runCommand(ctx, cmd, env.timeoutGoGet)
	if err != nil {
		return err
	}
	return env.execGoModTidy(ctx)
}

type goModTemplateContext struct {
	K6Module string
}

const mainModuleTemplate = `package main

import (
	k6cmd "{{.K6Module}}/cmd"

	// plug in k6 modules here
	// TODO: Create /modules/standard dir structure?
	// _ "{{.K6Module}}/modules/standard"
)

func main() {
	k6cmd.Execute()
}
`
