// Command supplychain audits the locked Go modules without downloading an
// external scanner: it runs `go mod verify`, confirms every dependency is in
// the local module cache, and looks for a license/notice file in each.
//
// It is not a legal conclusion and does not replace govulncheck/go-licenses.
// Exit codes: 0 verified, 1 module verification failed, 2 unverified (missing
// cache entries, license files or a root LICENSE).
//
//	go run ./tools/supplychain [-json]
package main

import (
	"bytes"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
)

var licenseName = regexp.MustCompile(`(?i)^(license|licence|copying|notice)(\.[^/]*)?$`)

type module struct {
	Path    string `json:"Path"`
	Version string `json:"Version"`
	Dir     string `json:"Dir"`
	Main    bool   `json:"Main"`
}

type dependency struct {
	Module       string   `json:"module"`
	Version      string   `json:"version"`
	CachePresent bool     `json:"cache_present"`
	LicenseFiles []string `json:"license_files"`
}

type report struct {
	Format               string       `json:"format"`
	ModuleVerify         string       `json:"module_verify"`
	ModuleVerifyOutput   string       `json:"module_verify_output"`
	CacheComplete        bool         `json:"cache_complete"`
	LicenseFilesComplete bool         `json:"license_files_complete"`
	RootLicenseDeclared  bool         `json:"root_license_declared"`
	Dependencies         []dependency `json:"dependencies"`
}

func main() {
	asJSON := flag.Bool("json", false, "emit machine-readable output")
	flag.Parse()

	verifyOutput, verifyErr := exec.Command("go", "mod", "verify").CombinedOutput()
	result := report{Format: "all2api-go-supply-chain-local-v1", ModuleVerify: "ok", ModuleVerifyOutput: strings.TrimSpace(string(verifyOutput))}
	if verifyErr != nil {
		result.ModuleVerify = "failed"
	}

	modules, err := listModules()
	if err != nil {
		fmt.Fprintln(os.Stderr, "FAIL list modules:", err)
		os.Exit(1)
	}
	result.CacheComplete, result.LicenseFilesComplete = len(modules) > 0, len(modules) > 0
	for _, item := range modules {
		dep := dependency{Module: item.Path, Version: item.Version, LicenseFiles: []string{}}
		if info, err := os.Stat(item.Dir); item.Dir != "" && err == nil && info.IsDir() {
			dep.CachePresent = true
			dep.LicenseFiles = licenseFiles(item.Dir)
		}
		result.CacheComplete = result.CacheComplete && dep.CachePresent
		result.LicenseFilesComplete = result.LicenseFilesComplete && len(dep.LicenseFiles) > 0
		result.Dependencies = append(result.Dependencies, dep)
	}
	for _, name := range []string{"LICENSE", "LICENSE.md", "COPYING"} {
		if _, err := os.Stat(name); err == nil {
			result.RootLicenseDeclared = true
		}
	}

	if *asJSON {
		encoder := json.NewEncoder(os.Stdout)
		encoder.SetIndent("", "  ")
		_ = encoder.Encode(result)
	} else {
		fmt.Printf("module_verify=%s\nmodule_cache_complete=%t\ndependency_license_files_complete=%t\nroot_license_declared=%t\n",
			result.ModuleVerify, result.CacheComplete, result.LicenseFilesComplete, result.RootLicenseDeclared)
		for _, dep := range result.Dependencies {
			state := "unverified"
			if dep.CachePresent && len(dep.LicenseFiles) > 0 {
				state = "ok"
			}
			fmt.Printf("%s %s@%s\n", state, dep.Module, dep.Version)
		}
	}

	switch {
	case result.ModuleVerify != "ok":
		os.Exit(1)
	case !result.CacheComplete || !result.LicenseFilesComplete || !result.RootLicenseDeclared:
		os.Exit(2)
	}
}

// listModules returns the third-party modules actually compiled into the
// shipped binaries (./cmd/...), with replace directives applied.
func listModules() ([]module, error) {
	output, err := exec.Command("go", "list", "-e", "-deps", "-json=Module", "./cmd/...").Output()
	if err != nil {
		return nil, err
	}
	seen := map[string]module{}
	decoder := json.NewDecoder(bytes.NewReader(output))
	for {
		var pkg struct {
			Module *struct {
				module
				Replace *module `json:"Replace"`
			} `json:"Module"`
		}
		if err := decoder.Decode(&pkg); err == io.EOF {
			break
		} else if err != nil {
			return nil, err
		}
		if pkg.Module == nil || pkg.Module.Main {
			continue // standard library or this repository
		}
		item := pkg.Module.module
		if replace := pkg.Module.Replace; replace != nil {
			item.Version, item.Dir = replace.Version, replace.Dir
		}
		seen[item.Path] = item
	}
	modules := make([]module, 0, len(seen))
	for _, item := range seen {
		modules = append(modules, item)
	}
	sort.Slice(modules, func(i, j int) bool { return modules[i].Path < modules[j].Path })
	return modules, nil
}

func licenseFiles(dir string) []string {
	var found []string
	_ = filepath.WalkDir(dir, func(path string, entry fs.DirEntry, err error) error {
		if err != nil || len(found) >= 20 {
			return filepath.SkipDir
		}
		if !entry.IsDir() && licenseName.MatchString(entry.Name()) {
			relative, _ := filepath.Rel(dir, path)
			found = append(found, filepath.ToSlash(relative))
		}
		return nil
	})
	sort.Strings(found)
	return found
}
