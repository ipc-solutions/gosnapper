package gosnapper

import (
	"bufio"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

const (
	// Tarsnap executable name
	Tarsnap = "tarsnap"

	// Default thread pool size
	ThreadPoolDefaultSize = 10

	// Error messages from tarsnap
	ExitError     = "tarsnap: Error exit delayed from previous errors.\n"
	NotOlderError = "File on disk is not older; skipping.\n"
	AlreadyExists = ": Already exists\n"

	// Characters that need escaping in tarsnap
	GlobChars = "*?[]{}"
)

// Group represents a group of files to be processed together
type Group struct {
	Files []string
	Size  int64
}

// Add adds a file to the group
func (g *Group) Add(name string, size int64) {
	g.Files = append(g.Files, name)
	g.Size += size
}

// FileInfo stores information about a file in the archive
type FileInfo struct {
	Size int64
	Date time.Time
}

// GoSnapper is the main struct for the gosnapper tool
type GoSnapper struct {
	archive      string
	options      Options
	tpsize       int
	files        map[string]FileInfo
	errorOccured bool
	outputMutex  sync.Mutex
}

// Options contains configuration options for GoSnapper
type Options struct {
	Directory      string
	ThreadPoolSize int
	TarsnapOptions []string
	Previous       map[string]FileInfo
	Verbose        bool
}

// NewGoSnapper creates a new GoSnapper instance
func NewGoSnapper(archive string, options Options) *GoSnapper {
	if options.ThreadPoolSize == 0 {
		options.ThreadPoolSize = ThreadPoolDefaultSize
	}

	return &GoSnapper{
		archive:      archive,
		options:      options,
		tpsize:       options.ThreadPoolSize,
		errorOccured: false,
		outputMutex:  sync.Mutex{},
	}
}

// GetFiles returns the list of files in the archive
func (rs *GoSnapper) GetFiles() map[string]FileInfo {
	if rs.files != nil {
		return rs.files
	}

	args := []string{"-tvf", rs.archive}
	args = append(args, rs.options.TarsnapOptions...)
	if rs.options.Directory != "" {
		args = append(args, rs.options.Directory)
	}

	cmd := exec.Command(Tarsnap, args...)

	stdout, err := cmd.StdoutPipe()
	if err != nil {
		fmt.Fprintf(os.Stderr, "Error creating stdout pipe: %v\n", err)
		return nil
	}

	if err := cmd.Start(); err != nil {
		fmt.Fprintf(os.Stderr, "Error starting tarsnap: %v\n", err)
		return nil
	}

	rs.files = make(map[string]FileInfo)
	scanner := bufio.NewScanner(stdout)

	for scanner.Scan() {
		entry := scanner.Text()
		fields := strings.Fields(entry)
		if len(fields) < 9 {
			continue
		}

		size, err := strconv.ParseInt(fields[4], 10, 64)
		if err != nil {
			continue
		}

		month := fields[5]
		day := fields[6]
		yearOrTime := fields[7]
		name := strings.Join(fields[8:], " ")

		// Parse date
		var dateStr string
		if strings.Contains(yearOrTime, ":") {
			// If it's a time, assume it's this year
			currentYear := time.Now().Year()
			dateStr = fmt.Sprintf("%s %s %d %s", month, day, currentYear, yearOrTime)
		} else {
			dateStr = fmt.Sprintf("%s %s %s", month, day, yearOrTime)
		}

		date, err := time.Parse("Jan 2 2006 15:04", dateStr)
		if err != nil {
			date, err = time.Parse("Jan 2 2006", dateStr)
			if err != nil {
				continue
			}
		}

		// If the date is in the future, assume it's from last year
		if date.After(time.Now()) {
			date = date.AddDate(-1, 0, 0)
		}

		rs.files[name] = FileInfo{
			Size: size,
			Date: date,
		}
	}

	if err := cmd.Wait(); err != nil {
		fmt.Fprintf(os.Stderr, "Error waiting for tarsnap: %v\n", err)
	}

	rs.outputMutex.Lock()
	fmt.Fprintf(os.Stderr, "File scanning complete: %d files found in archive\n", len(rs.files))
	rs.outputMutex.Unlock()

	return rs.files
}

// EmptyDirs returns a list of empty directories
func (rs *GoSnapper) EmptyDirs(files []string, dirs []string) []string {
	emptyDirs := make(map[string]bool)
	for _, dir := range dirs {
		emptyDirs[dir] = true
	}

	// Remove directories that contain files
	for _, file := range files {
		delete(emptyDirs, filepath.Dir(file)+"/")
	}

	// Remove parent directories of other directories
	for _, dir := range dirs {
		components := strings.Split(strings.TrimSuffix(dir, "/"), "/")
		for i := range components {
			if i < len(components)-1 {
				parentDir := strings.Join(components[:i+1], "/") + "/"
				delete(emptyDirs, parentDir)
			}
		}
	}

	result := make([]string, 0, len(emptyDirs))
	for dir := range emptyDirs {
		result = append(result, dir)
	}

	return result
}

// FilesToExtract returns a map of files to extract
func (rs *GoSnapper) FilesToExtract() map[string]FileInfo {
	allFiles := rs.GetFiles()
	filesToExtract := make(map[string]FileInfo)
	dirs := make([]string, 0)

	// Separate files and directories
	for name, info := range allFiles {
		if strings.HasSuffix(name, "/") {
			dirs = append(dirs, name)
		} else {
			filesToExtract[name] = info
		}
	}

	// Add empty directories
	fileNames := make([]string, 0, len(filesToExtract))
	for name := range filesToExtract {
		fileNames = append(fileNames, name)
	}

	emptyDirs := rs.EmptyDirs(fileNames, dirs)
	for _, dir := range emptyDirs {
		filesToExtract[dir] = FileInfo{Size: 0}
	}

	return filesToExtract
}

// FileGroups divides files into groups for parallel processing
func (rs *GoSnapper) FileGroups() [][]string {
	groups := make([]*Group, rs.tpsize)
	for i := range groups {
		groups[i] = &Group{
			Files: make([]string, 0),
			Size:  0,
		}
	}

	// Sort files by size (largest first)
	filesToExtract := rs.FilesToExtract()
	type fileEntry struct {
		Name string
		Info FileInfo
	}

	entries := make([]fileEntry, 0, len(filesToExtract))
	for name, info := range filesToExtract {
		entries = append(entries, fileEntry{Name: name, Info: info})
	}

	sort.Slice(entries, func(i, j int) bool {
		return entries[i].Info.Size > entries[j].Info.Size
	})

	// Distribute files to groups
	for _, entry := range entries {
		size := entry.Info.Size

		// If the file exists in previous with the same size and date, assign zero weight
		if rs.options.Previous != nil {
			if prevInfo, ok := rs.options.Previous[entry.Name]; ok {
				if prevInfo.Size == entry.Info.Size && prevInfo.Date.Equal(entry.Info.Date) {
					size = 0
				}
			}
		}

		// Find the group with the smallest total size
		smallestGroup := groups[0]
		for _, group := range groups {
			if group.Size < smallestGroup.Size {
				smallestGroup = group
			}
		}

		smallestGroup.Add(entry.Name, size)
	}

	// Convert to slice of string slices
	result := make([][]string, 0, rs.tpsize)
	for _, group := range groups {
		if len(group.Files) > 0 {
			result = append(result, group.Files)
		}
	}

	return result
}

// Run executes the extraction process
func (rs *GoSnapper) Run() {
	var wg sync.WaitGroup
	fileGroups := rs.FileGroups()

	rs.outputMutex.Lock()
	if rs.options.Verbose {
		fmt.Fprintf(os.Stderr, "Creating %d worker threads for file extraction with %d total files\n", len(fileGroups), len(rs.files))
		fmt.Fprintf(os.Stderr, "File groups distribution: %d groups with average of %d files per group\n", len(fileGroups), len(rs.files)/max(1, len(fileGroups)))
	}
	rs.outputMutex.Unlock()

	for i, files := range fileGroups {
		wg.Add(1)
		go func(idx int, chunk []string) {
			defer wg.Done()

			startTime := time.Now()

			rs.outputMutex.Lock()
			if rs.options.Verbose {
				fmt.Fprintf(os.Stderr, "Thread %d started with %d files to process. Files: %v\n", idx, len(chunk), chunk[:min(5, len(chunk))])
				if len(chunk) > 5 {
					fmt.Fprintf(os.Stderr, "... and %d more files\n", len(chunk)-5)
				}
				fmt.Fprintf(os.Stderr, "Thread %d working directory: %s\n", idx, rs.options.Directory)
			}
			rs.outputMutex.Unlock()

			// Escape glob characters in filenames
			for i, file := range chunk {
				for _, c := range GlobChars {
					file = strings.ReplaceAll(file, string(c), "\\"+string(c))
				}
				chunk[i] = file
			}

			// Create command with files appended directly to arguments, matching Ruby implementation
			args := []string{"-xvf", rs.archive}
			args = append(args, rs.options.TarsnapOptions...)
			args = append(args, chunk...)

			cmd := exec.Command(Tarsnap, args...)

			stderr, err := cmd.StderrPipe()
			if err != nil {
				rs.outputMutex.Lock()
				fmt.Fprintf(os.Stderr, "Error creating stderr pipe: %v\n", err)
				rs.outputMutex.Unlock()
				return
			}

			if err := cmd.Start(); err != nil {
				rs.outputMutex.Lock()
				fmt.Fprintf(os.Stderr, "Error starting tarsnap: %v\n", err)
				rs.outputMutex.Unlock()
				return
			}

			// Process stderr
			scanner := bufio.NewScanner(stderr)
			for scanner.Scan() {
				line := scanner.Text() + "\n"
				if strings.HasSuffix(line, NotOlderError) || strings.HasSuffix(line, AlreadyExists) {
					continue
				}
				if line == ExitError {
					rs.errorOccured = true
					continue
				}
				rs.outputMutex.Lock()
				if rs.options.Verbose {
					fmt.Fprintf(os.Stderr, "Thread %d output: %s", idx, line)
				} else {
					fmt.Fprintf(os.Stderr, "%s", line)
				}
				rs.outputMutex.Unlock()
			}

			if err := cmd.Wait(); err != nil {
				// Command errors are already handled via stderr
				if rs.options.Verbose {
					rs.outputMutex.Lock()
					fmt.Fprintf(os.Stderr, "Thread %d command execution completed with error: %v\n", idx, err)
					rs.outputMutex.Unlock()
				}
			} else if rs.options.Verbose {
				rs.outputMutex.Lock()
				fmt.Fprintf(os.Stderr, "Thread %d command execution completed successfully\n", idx)
				rs.outputMutex.Unlock()
			}

			duration := time.Since(startTime)
			rs.outputMutex.Lock()
			if rs.options.Verbose {
				fmt.Fprintf(os.Stderr, "Thread %d completed in %v with %d files processed. Exit status: %v\n", idx, duration, len(chunk), cmd.ProcessState)
				fmt.Fprintf(os.Stderr, "Thread %d command executed: %s %s\n", idx, Tarsnap, strings.Join(args, " "))
			}
			rs.outputMutex.Unlock()
		}(i, files)
	}

	wg.Wait()

	if rs.errorOccured {
		rs.outputMutex.Lock()
		fmt.Fprintf(os.Stderr, ExitError)
		rs.outputMutex.Unlock()
	}
}

func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}

func max(a, b int) int {
	if a > b {
		return a
	}
	return b
}
