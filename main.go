package main

import (
	"encoding/json"
	"fmt"

	"github.com/ktr0731/go-fuzzyfinder"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"sync"
	"time"
)

type NpmScript struct {
	PackageName  string
	ScriptName   string
	Command      string
	AbsolutePath string
}

// Concurrent version of finding package.json files
func findPackageJSONPathsConcurrent(rootPath string) []string {
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
			entryPath := path + "/" + entry.Name()
			wg.Add(1)
			go findPackageJSON(entryPath, paths, wg)
		} else if entry.Name() == "package.json" {
			// If the entry is a package.json file, send its path to the channel
			paths <- path + "/" + entry.Name()
		}
	}
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
	filepaths := findPackageJSONPathsConcurrent(searchPath)

	if len(filepaths) == 0 {
		fmt.Println("No package.json files found.")
		os.Exit(1)
		return
	}

	// Use the concurrent version to extract scripts from package.json files
	allScripts := extractScriptsFromPackageJSONsConcurrent(filepaths)

	timeEnd := time.Now()

	fmt.Printf("Found %d package.json files in %s\n", len(filepaths), timeEnd.Sub(timeStart).String())

	idx, err := fuzzyfinder.Find(allScripts, func(i int) string {
		return fmt.Sprintf("%s > (%s)", allScripts[i].PackageName, allScripts[i].ScriptName)
	})

	if err != nil {
		fmt.Println("Error:", err)
		return
	}

	runScript(allScripts[idx])
}
