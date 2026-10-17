// Command doclinks checks that relative links in the repository's Markdown
// files point at files that exist. It never touches the network.
//
//	go run ./tools/doclinks                     # README.md + docs/ (skips docs/archive)
//	go run ./tools/doclinks -include-archive     # also check docs/archive
//	go run ./tools/doclinks docs/API契约.md      # specific files or directories
package main

import (
	"flag"
	"fmt"
	"io/fs"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
)

// linkPattern matches [text](target) but not image links ![alt](src).
var linkPattern = regexp.MustCompile(`(^|[^!])\[[^\]]*\]\((<[^>]+>|[^)\s]+)(?:\s+[^)]*)?\)`)

var skipSchemes = []string{"http://", "https://", "mailto:", "tel:", "data:"}

func main() {
	includeArchive := flag.Bool("include-archive", false, "also check links inside docs/archive")
	flag.Parse()
	root, err := repositoryRoot()
	if err != nil {
		fail(err)
	}
	inputs := flag.Args()
	if len(inputs) == 0 {
		inputs = []string{"README.md", "docs", "scripts/README.md"}
	}
	files, err := markdownFiles(root, inputs, *includeArchive)
	if err != nil {
		fail(err)
	}

	var broken []string
	checked := 0
	for _, file := range files {
		data, err := os.ReadFile(file)
		if err != nil {
			fail(err)
		}
		text := string(data)
		for _, match := range linkPattern.FindAllStringSubmatchIndex(text, -1) {
			target := text[match[4]:match[5]]
			resolved, ok := resolveTarget(root, file, target)
			if !ok {
				continue
			}
			checked++
			if _, err := os.Stat(resolved); err != nil {
				line := strings.Count(text[:match[4]], "\n") + 1
				relative, _ := filepath.Rel(root, file)
				broken = append(broken, fmt.Sprintf("%s:%d -> %s", filepath.ToSlash(relative), line, target))
			}
		}
	}
	if len(broken) > 0 {
		fmt.Fprintln(os.Stderr, "Broken local Markdown links:")
		for _, item := range broken {
			fmt.Fprintln(os.Stderr, "- "+item)
		}
		os.Exit(1)
	}
	fmt.Printf("Checked %d Markdown files and %d local links: PASS\n", len(files), checked)
}

func markdownFiles(root string, inputs []string, includeArchive bool) ([]string, error) {
	archive := filepath.Join(root, "docs", "archive")
	var files []string
	for _, input := range inputs {
		path := filepath.Join(root, filepath.FromSlash(input))
		info, err := os.Stat(path)
		if err != nil {
			return nil, fmt.Errorf("markdown path does not exist: %s", input)
		}
		if !info.IsDir() {
			files = append(files, path)
			continue
		}
		err = filepath.WalkDir(path, func(current string, entry fs.DirEntry, err error) error {
			if err != nil {
				return err
			}
			if entry.IsDir() && !includeArchive && current == archive {
				return filepath.SkipDir
			}
			if !entry.IsDir() && strings.EqualFold(filepath.Ext(current), ".md") {
				files = append(files, current)
			}
			return nil
		})
		if err != nil {
			return nil, err
		}
	}
	sort.Strings(files)
	return files, nil
}

// resolveTarget maps a link target to a filesystem path. It returns false for
// anchors, external URLs and other links that are not local files.
func resolveTarget(root, source, target string) (string, bool) {
	target = strings.TrimSpace(strings.TrimSuffix(strings.TrimPrefix(target, "<"), ">"))
	lower := strings.ToLower(target)
	if target == "" || strings.HasPrefix(target, "#") {
		return "", false
	}
	for _, scheme := range skipSchemes {
		if strings.HasPrefix(lower, scheme) {
			return "", false
		}
	}
	target, _, _ = strings.Cut(target, "#")
	if unescaped, err := url.PathUnescape(target); err == nil {
		target = unescaped
	}
	if target == "" {
		return "", false
	}
	if strings.HasPrefix(target, "/") {
		return filepath.Join(root, filepath.FromSlash(strings.TrimPrefix(target, "/"))), true
	}
	return filepath.Join(filepath.Dir(source), filepath.FromSlash(target)), true
}

// repositoryRoot walks up from the working directory to the directory that
// holds go.mod, so the tool works from any subdirectory.
func repositoryRoot() (string, error) {
	dir, err := os.Getwd()
	if err != nil {
		return "", err
	}
	for {
		if _, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil {
			return dir, nil
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			return "", fmt.Errorf("go.mod not found above the working directory")
		}
		dir = parent
	}
}

func fail(err error) {
	fmt.Fprintln(os.Stderr, "FAIL", err)
	os.Exit(1)
}
