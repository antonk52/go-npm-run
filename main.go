package main

import (
	"encoding/json"
	"fmt"
	"strings"

	"io"
	"os"
	"os/exec"
	"path/filepath"
	"sync"
	"time"

	"github.com/ktr0731/go-fuzzyfinder"
	"gopkg.in/yaml.v2"
)

type NpmScript struct {
	PackageName  string
	ScriptName   string
	Command      string
	AbsolutePath string
}

// Workspace represents the structure of the pnpm-workspace.yaml file.
type pnpmWorkspace struct {
	Packages []string `yaml:"packages"`
}

// Concurrent version of finding package.json files
func findProjectRootPackageJSONPathsConcurrent(rootPath string) []string {
	var wg sync.WaitGroup
	pathsChan := make(chan string, 100) // Buffered channel to prevent blocking

	// Create a goroutine to traverse the filesystem
	wg.Add(1)
	go findPackageJSON(rootPath, pathsChan, &wg)

	// Wait for all goroutines to finish in a separate goroutine
	go func() {
		wg.Wait()
		close(pathsChan)
	}()

	// Collect paths from the channel
	var filepaths []string
	for path := range pathsChan {
		filepaths = append(filepaths, path)
	}

	return filepaths
}

func findPackageJSON(path string, paths chan<- string, wg *sync.WaitGroup) {
	defer wg.Done()

	// Open the directory
	dir, err := os.Open(path)
	if err != nil {
		return
	}
	defer dir.Close()

	// Read the directory entries
	entries, err := dir.Readdir(-1)
	if err != nil {
		return
	}

	for _, entry := range entries {
		if entry.IsDir() && entry.Name() != "node_modules" && entry.Name() != ".git" {
			// If the entry is a directory, launch a new goroutine
			dirPath := filepath.Join(path, entry.Name())

			packageJsonPath := filepath.Join(dirPath, "package.json")
			// If package.json file is in the directory, we might be able to stop here
			if _, err := os.Stat(packageJsonPath); err == nil {
				paths <- packageJsonPath
			} else {
				wg.Add(1)
				go findPackageJSON(dirPath, paths, wg)
			}
		} else if entry.Name() == "package.json" {
			// If the entry is a package.json file, send its path to the channel
			paths <- filepath.Join(path, entry.Name())
		}
	}
}

func locatePnpmWorkspaces(pnpmWorkspaceRoot string) ([]string, error) {
	file, err := os.Open(pnpmWorkspaceRoot + "/pnpm-workspace.yaml")
	if err != nil {
		return nil, err
	}
	defer file.Close()

	data, err := io.ReadAll(file)
	if err != nil {
		return nil, fmt.Errorf("reading pnpm workspace file: %w", err)
	}

	// Unmarshal YAML into our Workspace struct.
	var ws pnpmWorkspace
	if err := yaml.Unmarshal(data, &ws); err != nil {
		return nil, fmt.Errorf("parsing YAML: %w", err)
	}

	var includePatterns []string
	var excludePatterns []string

	// Separate inclusion and exclusion patterns.
	for _, pattern := range ws.Packages {
		trimmed := strings.TrimSpace(pattern)
		if strings.HasPrefix(trimmed, "!") {
			// Exclusion pattern (remove the "!" prefix).
			name := filepath.Join(pnpmWorkspaceRoot, strings.TrimPrefix(trimmed, "!"))
			excludePatterns = append(excludePatterns, name)
		} else {
			includePatterns = append(includePatterns, filepath.Join(pnpmWorkspaceRoot, trimmed))
		}
	}

	// Build a set (map) to store matching workspaces.
	matchesSet := make(map[string]bool)

	// Process include patterns.
	for _, pattern := range includePatterns {
		// filepath.Glob returns only existing paths that match the given pattern.
		matches, err := filepath.Glob(pattern)
		if err != nil {
			return nil, fmt.Errorf("expanding include pattern %q: %w", pattern, err)
		}
		for _, match := range matches {
			// Optionally, you can check if the match is a directory.
			if info, err := os.Stat(match); err == nil && info.IsDir() {
				matchesSet[match] = true
			} else if err == nil {
				// The item exists, even if it is not a directory.
				matchesSet[match] = true
			}
		}
	}

	// Process exclusion patterns.
	for _, pattern := range excludePatterns {
		matches, err := filepath.Glob(pattern)
		if err != nil {
			return nil, fmt.Errorf("expanding exclude pattern %q: %w", pattern, err)
		}
		for _, match := range matches {
			delete(matchesSet, match)
		}
	}

	// Convert the set of matches to a slice.
	var result []string
	for match := range matchesSet {
		result = append(result, match)
	}

	return result, nil
}

func extractScriptsFromPackageJSON(filePath string) ([]NpmScript, error) {
	var scripts []NpmScript
	// Open the package.json file
	file, err := os.Open(filePath)
	if err != nil {
		return nil, err
	}
	defer file.Close()

	// Read the file content
	byteValue, err := io.ReadAll(file)
	if err != nil {
		return nil, err
	}

	// Unmarshal the JSON content
	var packageJSON map[string]any
	err = json.Unmarshal(byteValue, &packageJSON)
	if err != nil {
		return nil, fmt.Errorf("failed to parse %s: %w", filePath, err)
	}

	packageName := "unknown"

	if name, ok := packageJSON["name"].(string); ok {
		packageName = name
	}

	// Extract the scripts
	if scriptsMap, ok := packageJSON["scripts"].(map[string]any); ok {
		for name, command := range scriptsMap {
			scripts = append(scripts, NpmScript{PackageName: packageName, ScriptName: name, Command: command.(string), AbsolutePath: filePath})
		}
	}

	if workspaces, ok := packageJSON["workspaces"].([]any); ok {
		for _, workspace := range workspaces {
			isGlob := strings.ContainsAny(workspace.(string), "*?[")
			workspacePath := filepath.Join(filepath.Dir(filePath), workspace.(string))

			if isGlob {
				// If the workspace is a glob pattern, find all matching directories
				matches, err := filepath.Glob(workspacePath)
				if err != nil {
					continue
				}
				for _, match := range matches {
					workspacePackageJSONPath := filepath.Join(match, "package.json")
					if _, err := os.Stat(workspacePackageJSONPath); err == nil {
						workspaceScripts, err := extractScriptsFromPackageJSON(workspacePackageJSONPath)
						if err == nil {
							scripts = append(scripts, workspaceScripts...)
						}
					}
				}
			} else {
				// If the workspace is a directory, check if package.json exists
				workspacePackageJSONPath := filepath.Join(workspacePath, "package.json")
				if _, err := os.Stat(workspacePackageJSONPath); err == nil {
					workspaceScripts, err := extractScriptsFromPackageJSON(workspacePackageJSONPath)
					if err == nil {
						scripts = append(scripts, workspaceScripts...)
					}
				}
			}

			// if workspace path exists, extract scripts from it's package.json
			if _, err := os.Stat(workspacePath); err == nil {
				workspacePackageJSONPath := filepath.Join(workspacePath, "package.json")
				workspaceScripts, err := extractScriptsFromPackageJSON(workspacePackageJSONPath)
				if err == nil {
					scripts = append(scripts, workspaceScripts...)
				}
			}
		}
	}

	dirname := filepath.Dir(filePath)
	pnpmWorkspacePath := filepath.Join(dirname, "pnpm-workspace.yaml")

	if _, err := os.Stat(pnpmWorkspacePath); err == nil {

		if result, err := locatePnpmWorkspaces(dirname); err == nil {
			// Iterate over the matches and extract scripts from each package.json.
			for _, match := range result {
				workspacePackageJSONPath := filepath.Join(match, "package.json")
				// Skip the current package.json
				if workspacePackageJSONPath == filePath {
					continue
				}
				if _, err := os.Stat(workspacePackageJSONPath); err == nil {
					workspaceScripts, err := extractScriptsFromPackageJSON(workspacePackageJSONPath)
					if err == nil {
						scripts = append(scripts, workspaceScripts...)
					}
				}
			}
		}
	}

	return scripts, nil
}

func extractScriptsFromPackageJSONsConcurrent(filepaths []string) []NpmScript {
	var wg sync.WaitGroup
	scriptsChan := make(chan []NpmScript, len(filepaths))

	for _, path := range filepaths {
		wg.Add(1)
		// Launch a goroutine for each package.json file
		go func(filePath string) {
			defer wg.Done()
			scripts, err := extractScriptsFromPackageJSON(filePath)
			if err != nil {
				fmt.Println("Error:", err)
				return
			}
			scriptsChan <- scripts
		}(path)
	}

	// Wait for all goroutines to finish in a separate goroutine
	go func() {
		wg.Wait()
		close(scriptsChan)
	}()

	// Collect all scripts from the channel
	var allScripts []NpmScript
	for scripts := range scriptsChan {
		allScripts = append(allScripts, scripts...)
	}

	return allScripts
}

func inferPackageManager(filePath string) string {
	knownLockFiles := map[string]string{
		"package-lock.json": "npm",
		"yarn.lock":         "yarn",
		"pnpm-lock.yaml":    "pnpm",
		"bun.lock":          "bun",
		"bun.lockb":         "bun",
	}

	dir := filepath.Dir(filePath)
	for dir != "." {
		for lockFile, pkgManager := range knownLockFiles {
			if _, err := os.Stat(filepath.Join(dir, lockFile)); err == nil {
				return pkgManager
			}
		}
		dir = filepath.Dir(dir)
	}
	return "npm"

}

func runScript(script NpmScript) {
	packageManager := inferPackageManager(script.AbsolutePath)
	cmd := exec.Command(packageManager, "run", script.ScriptName)

	cmd.Dir = filepath.Dir(script.AbsolutePath)
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr

	if err := cmd.Run(); err != nil {
		if exitError, ok := err.(*exec.ExitError); ok {
			// exit with the same exit code as the command
			os.Exit(exitError.ExitCode())
		} else {
			fmt.Printf("Error: %v\n", err)
			os.Exit(1)
		}
	}
}

func main() {
	timeStart := time.Now()
	searchPath := "."

	if len(os.Args) > 1 {
		searchPath = os.Args[1]
	}

	// Use the concurrent version to find package.json files
	projectRootPackageJsons := findProjectRootPackageJSONPathsConcurrent(searchPath)

	if len(projectRootPackageJsons) == 0 {
		fmt.Println("No package.json files found.")
		os.Exit(1)
		return
	}

	// Use the concurrent version to extract scripts from package.json files
	allScripts := extractScriptsFromPackageJSONsConcurrent(projectRootPackageJsons)

	timeEnd := time.Now()

	idx, err := fuzzyfinder.Find(allScripts, func(i int) string {
		return fmt.Sprintf("%s > (%s)", allScripts[i].PackageName, allScripts[i].ScriptName)
	})

	fmt.Printf("Found %d projects in %s\n", len(projectRootPackageJsons), timeEnd.Sub(timeStart).String())

	if err != nil {
		if err != fuzzyfinder.ErrAbort {
			fmt.Println("Error:", err)
		}
		return
	}

	runScript(allScripts[idx])
}
