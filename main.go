package main

import (
	"bytes"
	"encoding/json"
	"flag"
	"fmt"
	"log"
	"net/url"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/briandowns/spinner"
)

type Group struct {
	ID       int    `json:"id"`
	FullPath string `json:"full_path"`
}

type Project struct {
	HTTPURLToRepo     string `json:"http_url_to_repo"`
	PathWithNamespace string `json:"path_with_namespace"`
	Archived          bool   `json:"archived"`
}

type Subgroup struct {
	ID       int    `json:"id"`
	FullPath string `json:"full_path"`
}

// Job = one project to clone/update.
type Job struct {
	URL    string
	RelDir string
}

func main() {
	log.SetFlags(0)

	// Ensure cursor is always restored, even on panic or early exit
	defer fmt.Print("\033[?25h")

	// Handle Ctrl+C gracefully to restore cursor
	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, os.Interrupt, syscall.SIGTERM)
	go func() {
		<-sigCh
		fmt.Print("\033[?25h") // Restore cursor
		os.Exit(130)           // Standard exit code for SIGINT
	}()

	var includeInactive bool
	flag.BoolVar(&includeInactive, "inactive", false, "Include inactive (archived) projects")
	flag.Parse()

	if flag.NArg() < 1 || flag.NArg() > 2 {
		log.Fatalf("Usage: %s [--inactive] https://gitlab.com/group/subgroup[/...] [directory]", os.Args[0])
	}

	rawURL := flag.Arg(0)
	groupPath := extractGroupPath(rawURL)
	if groupPath == "" {
		log.Fatalf("Could not extract group path from URL: %s", rawURL)
	}

	// Determine target directory
	var directory string
	if flag.NArg() == 2 {
		// User provided a directory
		directory = flag.Arg(1)
	} else {
		// Extract last component from URL path
		parts := strings.Split(groupPath, "/")
		directory = parts[len(parts)-1]
	}

	// Create directory if it doesn't exist and change to it
	if err := os.MkdirAll(directory, 0o755); err != nil {
		log.Fatalf("Failed to create directory %s: %v", directory, err)
	}
	if err := os.Chdir(directory); err != nil {
		log.Fatalf("Failed to change to directory %s: %v", directory, err)
	}

	// Use glab for auth / host / token.
	group, err := getGroupByPathViaGlab(groupPath)
	if err != nil {
		log.Fatalf("Failed to get group: %v", err)
	}

	// 1. Collect all jobs (projects) recursively.
	s := spinner.New(spinner.CharSets[9], 100*time.Millisecond)
	s.Suffix = " Discovering projects and subgroups..."
	s.Start()
	defer s.Stop() // Ensure spinner is always stopped and cursor restored

	var jobs []Job
	var jobsMu sync.Mutex
	if err := collectJobsRecursive(group.ID, group.FullPath, &jobs, &jobsMu, includeInactive); err != nil {
		log.Fatalf("Error collecting projects: %v", err)
	}

	s.FinalMSG = fmt.Sprintf("Found %d projects.\n", len(jobs))
	s.Stop()

	if len(jobs) == 0 {
		log.Println("No projects found.")
		return
	}

	// 2. Run jobs concurrently with spinners.
	runJobsConcurrently(jobs)

	log.Println("Done.")
}

// extractGroupPath turns https://gitlab.com/gitlab-org/orbit/experiments into gitlab-org/orbit/experiments.
func extractGroupPath(raw string) string {
	u, err := url.Parse(raw)
	if err != nil {
		return ""
	}
	p := strings.TrimPrefix(u.Path, "/")
	p = strings.TrimSuffix(p, "/")
	return p
}

func getGroupByPathViaGlab(path string) (*Group, error) {
	escaped := url.PathEscape(path) // encodes "/" as "%2F"
	out, err := runGlabJSON("api", "groups/"+escaped)
	if err != nil {
		return nil, err
	}
	var g Group
	if err := json.Unmarshal(out, &g); err != nil {
		return nil, fmt.Errorf("unmarshal group: %w", err)
	}
	return &g, nil
}

// collectJobsRecursive walks a group + all subgroups and appends jobs to the slice.
func collectJobsRecursive(groupID int, rootFullPath string, jobs *[]Job, jobsMu *sync.Mutex, includeInactive bool) error {
	projects, err := listGroupProjectsViaGlab(groupID)
	if err != nil {
		return fmt.Errorf("list projects for group %d: %w", groupID, err)
	}

	jobsMu.Lock()
	for _, p := range projects {
		// Skip archived projects unless includeInactive is true
		if p.Archived && !includeInactive {
			continue
		}

		rel := relativePath(p.PathWithNamespace, rootFullPath)
		if rel == "" {
			rel = p.PathWithNamespace
		}
		*jobs = append(*jobs, Job{
			URL:    p.HTTPURLToRepo,
			RelDir: rel,
		})
	}
	jobsMu.Unlock()

	subs, err := listSubgroupsViaGlab(groupID)
	if err != nil {
		return fmt.Errorf("list subgroups for group %d: %w", groupID, err)
	}

	// Process subgroups concurrently
	var wg sync.WaitGroup
	errCh := make(chan error, len(subs))

	for _, sg := range subs {
		wg.Add(1)
		go func(subgroupID int) {
			defer wg.Done()
			if err := collectJobsRecursive(subgroupID, rootFullPath, jobs, jobsMu, includeInactive); err != nil {
				errCh <- err
			}
		}(sg.ID)
	}

	wg.Wait()
	close(errCh)

	// Check if any goroutine returned an error
	if err := <-errCh; err != nil {
		return err
	}

	return nil
}

func relativePath(pathWithNamespace, rootFullPath string) string {
	prefix := rootFullPath + "/"
	return strings.TrimPrefix(pathWithNamespace, prefix)
}

func listGroupProjectsViaGlab(groupID int) ([]Project, error) {
	var all []Project
	page := 1
	for {
		endpoint := fmt.Sprintf("groups/%d/projects?per_page=100&page=%d", groupID, page)
		out, err := runGlabJSON("api", endpoint)
		if err != nil {
			return nil, err
		}
		var batch []Project
		if err := json.Unmarshal(out, &batch); err != nil {
			return nil, fmt.Errorf("unmarshal projects: %w", err)
		}
		if len(batch) == 0 {
			break
		}
		all = append(all, batch...)
		page++
	}
	return all, nil
}

func listSubgroupsViaGlab(groupID int) ([]Subgroup, error) {
	var all []Subgroup
	page := 1
	for {
		endpoint := fmt.Sprintf("groups/%d/subgroups?per_page=100&page=%d", groupID, page)
		out, err := runGlabJSON("api", endpoint)
		if err != nil {
			return nil, err
		}
		var batch []Subgroup
		if err := json.Unmarshal(out, &batch); err != nil {
			return nil, fmt.Errorf("unmarshal subgroups: %w", err)
		}
		if len(batch) == 0 {
			break
		}
		all = append(all, batch...)
		page++
	}
	return all, nil
}

// runJobsConcurrently runs clone/pull jobs with a worker pool and a spinner per job.
func runJobsConcurrently(jobs []Job) {
	concurrency := runtime.NumCPU()
	if concurrency < 2 {
		concurrency = 2
	}

	jobCh := make(chan Job)
	var wg sync.WaitGroup
	var outputMu sync.Mutex

	for i := 0; i < concurrency; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for job := range jobCh {
				runJob(job, &outputMu)
			}
		}()
	}

	for _, j := range jobs {
		jobCh <- j
	}
	close(jobCh)

	wg.Wait()
}

func runJob(job Job, outputMu *sync.Mutex) {
	err := syncProjectJob(job)

	// Synchronize output to prevent messages appearing on the same line
	outputMu.Lock()
	defer outputMu.Unlock()

	if err != nil {
		fmt.Printf("❌ %s (%v)\n", job.RelDir, err)
	} else {
		fmt.Printf("\033[32m✔︎\033[0m %s\n", job.RelDir)
	}
}

// syncProjectJob clones or updates one project.
func syncProjectJob(job Job) error {
	dir := filepath.Dir(job.RelDir)
	if dir != "." {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return fmt.Errorf("mkdir %s: %w", dir, err)
		}
	}

	if isGitRepo(job.RelDir) {
		cmd := exec.Command("git", "-C", job.RelDir, "pull", "--rebase", "--quiet")
		return cmd.Run()
	}

	cmd := exec.Command("git", "clone", "--quiet", job.URL, job.RelDir)
	return cmd.Run()
}

func isGitRepo(path string) bool {
	stat, err := os.Stat(filepath.Join(path, ".git"))
	if err != nil {
		return false
	}
	return stat.IsDir()
}

func runGlabJSON(args ...string) ([]byte, error) {
	cmd := exec.Command("glab", args...)
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		return nil, fmt.Errorf("glab %v failed: %w (stderr: %s)", args, err, stderr.String())
	}
	return stdout.Bytes(), nil
}
