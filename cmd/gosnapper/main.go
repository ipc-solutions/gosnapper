package main

import (
	"flag"
	"fmt"
	"os"
	"path/filepath"

	"github.com/IPC-Solutions/gosnapper"
)

func main() {
	// Parse command line options
	var directory string
	var jobs int
	var verbose bool

	flag.StringVar(&directory, "d", "", "Extract files from this directory of the archive")
	flag.StringVar(&directory, "directory", "", "Extract files from this directory of the archive")
	flag.IntVar(&jobs, "j", 0, "Number of workers to use")
	flag.IntVar(&jobs, "jobs", 0, "Number of workers to use")
	flag.BoolVar(&verbose, "v", false, "Enable verbose output")
	flag.BoolVar(&verbose, "verbose", false, "Enable verbose output")

	// Custom usage message
	flag.Usage = func() {
		fmt.Fprintf(os.Stderr, "Usage: %s [-d DIR] archive [-- [TARSNAP OPTIONS]]\n", filepath.Base(os.Args[0]))
		flag.PrintDefaults()
	}

	// Parse flags
	flag.Parse()

	// Get archive name
	args := flag.Args()
	if len(args) == 0 {
		fmt.Fprintln(os.Stderr, "Missing 'archive' parameter")
		flag.Usage()
		os.Exit(1)
	}

	archive := args[0]

	// Check for -- separator for tarsnap options
	tarsnapOptions := []string{}
	for i := 1; i < len(args); i++ {
		if args[i] == "--" && i+1 < len(args) {
			tarsnapOptions = args[i+1:]
			break
		}
	}

	// Create and run GoSnapper
	options := gosnapper.Options{
		Directory:      directory,
		ThreadPoolSize: jobs,
		TarsnapOptions: tarsnapOptions,
		Verbose:        verbose,
	}

	if verbose {
		fmt.Println("Verbose mode enabled")
	}

	rs := gosnapper.NewGoSnapper(archive, options)
	rs.Run()
}
