package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"log"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/briandowns/spinner"
	"github.com/schollz/progressbar/v3"
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
	flag.IntVar(&cloneConcurrency, "j", defaultCloneConcurrency(), "Number of concurrent git clone/pull operations")
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

	// 1. Collect all jobs (projects), including every descendant subgroup.
	// The first request is a spinner (we do not know the size yet); from its
	// X-Total-Pages header onwards it becomes a real progress bar.
	s := spinner.New(spinner.CharSets[9], 100*time.Millisecond)
	s.Suffix = " Discovering projects..."
	s.Start()
	defer s.Stop() // Ensure spinner is always stopped and cursor restored

	var discovery *progressbar.ProgressBar
	jobs, err := collectJobs(group.ID, group.FullPath, includeInactive, func(p pageProgress) {
		if discovery == nil {
			s.Stop()
			discovery = newBar(p.TotalPages, "Discovering")
		}
		discovery.Describe(fitWidth(fmt.Sprintf("Discovering %d/%d projects", p.Found, p.Total), descWidth))
		_ = discovery.Set(p.Page)
	})
	if discovery != nil {
		_ = discovery.Finish()
	}
	s.Stop()
	if err != nil {
		log.Fatalf("Error collecting projects: %v", err)
	}
	fmt.Printf("Found %d projects.\n", len(jobs))

	if len(jobs) == 0 {
		log.Println("No projects found.")
		return
	}

	// 2. Run jobs concurrently with spinners.
	res := runJobsConcurrently(jobs)

	parts := []string{fmt.Sprintf("%d synced", res.ok)}
	if res.noAccess > 0 {
		parts = append(parts, fmt.Sprintf("%d skipped (no access)", res.noAccess))
	}
	if res.empty > 0 {
		parts = append(parts, fmt.Sprintf("%d skipped (empty)", res.empty))
	}
	if res.failed > 0 {
		parts = append(parts, fmt.Sprintf("%d failed", res.failed))
	}
	log.Printf("Done: %s.", strings.Join(parts, ", "))
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
	resp, err := glabAPI("groups/" + escaped)
	if err != nil {
		return nil, err
	}
	var g Group
	if err := json.Unmarshal(resp.Body, &g); err != nil {
		return nil, fmt.Errorf("unmarshal group: %w", err)
	}
	return &g, nil
}

// collectJobs lists every project under a group, including all of its
// descendant subgroups. We ask GitLab to do the recursion for us with
// include_subgroups=true: walking the subgroup tree ourselves meant one
// request per group (times pages), all fired concurrently, which is what
// tripped the throttle_authenticated_api rate limit.
//
// pageProgress is what one page of the listing tells us about the whole walk.
// Page/TotalPages and Total come straight from GitLab's X-Page, X-Total-Pages
// and X-Total headers, so the caller can show a real progress bar instead of
// an open-ended spinner. TotalPages and Total are 0 if GitLab omits them
// (which it does past a certain result-set size).
type pageProgress struct {
	Page       int
	TotalPages int
	Found      int // projects kept so far
	Total      int // projects matching the query, per X-Total
}

// progress, if non-nil, is called once per fetched page.
func collectJobs(groupID int, rootFullPath string, includeInactive bool, progress func(pageProgress)) ([]Job, error) {
	var jobs []Job
	page := 1
	for {
		q := url.Values{}
		q.Set("include_subgroups", "true")
		q.Set("per_page", "100")
		q.Set("page", strconv.Itoa(page))
		// simple=true trims the payload a lot; it omits "archived", so let
		// the server filter archived projects instead of doing it here.
		q.Set("simple", "true")
		if !includeInactive {
			q.Set("archived", "false")
		}
		endpoint := fmt.Sprintf("groups/%d/projects?%s", groupID, q.Encode())

		resp, err := glabAPI(endpoint)
		if err != nil {
			return nil, fmt.Errorf("list projects for group %d (page %d): %w", groupID, page, err)
		}

		var batch []Project
		if err := json.Unmarshal(resp.Body, &batch); err != nil {
			return nil, fmt.Errorf("unmarshal projects: %w", err)
		}
		for _, p := range batch {
			if p.Archived && !includeInactive {
				continue
			}
			rel := relativePath(p.PathWithNamespace, rootFullPath)
			if rel == "" {
				rel = p.PathWithNamespace
			}
			jobs = append(jobs, Job{URL: p.HTTPURLToRepo, RelDir: rel})
		}
		if progress != nil {
			totalPages, _ := headerInt(resp.Header, "X-Total-Pages")
			total, _ := headerInt(resp.Header, "X-Total")
			progress(pageProgress{Page: page, TotalPages: totalPages, Found: len(jobs), Total: total})
		}

		// X-Next-Page is empty on the last page, which saves us the extra
		// request that the old "loop until an empty batch" logic needed.
		next := resp.Header.Get("X-Next-Page")
		if next == "" || next == strconv.Itoa(page) || len(batch) == 0 {
			break
		}
		page, err = strconv.Atoi(next)
		if err != nil {
			return nil, fmt.Errorf("bad X-Next-Page header %q: %w", next, err)
		}
	}
	return jobs, nil
}

func relativePath(pathWithNamespace, rootFullPath string) string {
	prefix := rootFullPath + "/"
	return strings.TrimPrefix(pathWithNamespace, prefix)
}

// runJobsConcurrently runs clone/pull jobs with a worker pool and a spinner per job.
// cloneConcurrency is the number of git clone/pull operations run in parallel.
var cloneConcurrency int

// defaultCloneConcurrency keeps a lid on parallel git processes: each one holds
// open sockets, and an unbounded fan-out can exhaust the file-descriptor limit
// of anything in between (a local mitmproxy, for instance).
func defaultCloneConcurrency() int {
	n := runtime.NumCPU()
	if n > 8 {
		n = 8
	}
	if n < 2 {
		n = 2
	}
	return n
}

// results tallies the outcome of every clone/pull.
type results struct {
	ok       int
	noAccess int
	empty    int
	failed   int
}

func runJobsConcurrently(jobs []Job) results {
	concurrency := cloneConcurrency
	if concurrency < 1 {
		concurrency = 1
	}

	var res results
	var mu sync.Mutex

	bar := newBar(len(jobs), "Syncing")

	jobCh := make(chan Job)
	var wg sync.WaitGroup

	for i := 0; i < concurrency; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for job := range jobCh {
				runJob(job, bar, &res, &mu)
			}
		}()
	}

	for _, j := range jobs {
		jobCh <- j
	}
	close(jobCh)

	wg.Wait()
	_ = bar.Finish()
	return res
}

func runJob(job Job, bar *progressbar.ProgressBar, res *results, mu *sync.Mutex) {
	err := syncProjectJob(job)

	// One lock for both the tally and the terminal: anything printed has to be
	// interleaved with the bar's own redraw, or the two garble each other.
	mu.Lock()
	defer mu.Unlock()

	switch {
	case err == nil:
		res.ok++
	case errors.Is(err, errNoAccess):
		// A group can contain projects whose repository we are not allowed to
		// read. That is expected, not a failure: counted, not printed.
		res.noAccess++
	case errors.Is(err, errEmptyRepo):
		// Same for projects created but never pushed to: there is nothing to
		// sync, and that is not something the user can act on.
		res.empty++
	default:
		res.failed++
		printAboveBar(bar, fmt.Sprintf("❌ %s (%v)", job.RelDir, err))
	}

	bar.Describe(fitWidth(job.RelDir, descWidth))
	_ = bar.Add(1)
}

// descWidth is the fixed number of cells reserved for a bar's description.
const descWidth = 40

// newBar builds a progress bar on stderr. A max of 0 or less means the total
// is unknown, in which case the bar renders as a spinner.
func newBar(max int, description string) *progressbar.ProgressBar {
	if max <= 0 {
		max = -1
	}
	return progressbar.NewOptions(max,
		progressbar.OptionSetWriter(os.Stderr),
		progressbar.OptionSetDescription(fitWidth(description, descWidth)),
		progressbar.OptionSetPredictTime(true),
		progressbar.OptionShowCount(),
		progressbar.OptionSetRenderBlankState(true),
		progressbar.OptionClearOnFinish(),
		progressbar.OptionFullWidth(),
		progressbar.OptionSetTheme(progressbar.Theme{
			Saucer: "█", SaucerHead: "█", SaucerPadding: "░",
			BarStart: "", BarEnd: "",
		}),
	)
}

// printAboveBar wipes the bar's line, writes a message, and lets the next
// Add() redraw the bar underneath it.
func printAboveBar(bar *progressbar.ProgressBar, msg string) {
	_ = bar.Clear()
	fmt.Fprintln(os.Stderr, msg)
}

// fitWidth makes s exactly n cells wide: long values keep their tail (the
// project name matters more than the group prefix), short ones are padded.
// The bar is laid out around the description, so a description of varying
// length makes the bar itself jump left and right on every update.
func fitWidth(s string, n int) string {
	r := []rune(s)
	if len(r) > n {
		return "…" + string(r[len(r)-n+1:])
	}
	return s + strings.Repeat(" ", n-len(r))
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
		err := runGit("git", "-C", job.RelDir, "pull", "--rebase", "--quiet")
		// A pull against a project that was emptied (or never pushed to) fails
		// with "no such ref was fetched", which reads like a local problem but
		// is not one. The remote itself has the answer.
		if err != nil && !errors.Is(err, errNoAccess) && remoteHasNoRefs(job.RelDir) {
			return errEmptyRepo
		}
		return err
	}

	return runGit("git", "clone", "--quiet", job.URL, job.RelDir)
}

// remoteHasNoRefs reports whether origin advertises no refs at all, i.e. the
// project exists but has never been pushed to.
func remoteHasNoRefs(dir string) bool {
	out, err := exec.Command("git", "-C", dir, "ls-remote", "--quiet", "origin").Output()
	return err == nil && len(bytes.TrimSpace(out)) == 0
}

// errNoAccess marks a repository we are allowed to see in the group listing
// but not allowed to clone (typically Guest access on a private project).
var errNoAccess = errors.New("no access to repository")

// errEmptyRepo marks a project that exists in the group listing but whose
// repository has no commits yet, so there is nothing to clone or pull.
var errEmptyRepo = errors.New("repository is empty")

// emptyRepoPatterns are the ways git and GitLab phrase "this project has no
// commits yet" when cloning.
var emptyRepoPatterns = []string{
	"a repository for this project does not exist yet",
	"you appear to have cloned an empty repository",
}

// noAccessPatterns are the ways GitLab and git phrase "you may see this
// project, but you may not read its repository".
var noAccessPatterns = []string{
	"you are not allowed to download code from this project",
	"returned error: 403",
	"authentication failed",
	"repository not found",
	"remote: the project you were looking for could not be found",
}

// runGit reports git's stderr along with the exit status; "exit status 128" on
// its own says nothing about whether it was auth, a proxy hiccup, or a rebase
// conflict.
func runGit(name string, args ...string) error {
	cmd := exec.Command(name, args...)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		msg := stderr.String()
		low := strings.ToLower(msg)
		for _, p := range emptyRepoPatterns {
			if strings.Contains(low, p) {
				return errEmptyRepo
			}
		}
		for _, p := range noAccessPatterns {
			if strings.Contains(low, p) {
				return errNoAccess
			}
		}
		if s := snippet([]byte(msg)); s != "" {
			return fmt.Errorf("%w: %s", err, s)
		}
		return err
	}
	return nil
}

func isGitRepo(path string) bool {
	stat, err := os.Stat(filepath.Join(path, ".git"))
	if err != nil {
		return false
	}
	return stat.IsDir()
}

// apiResponse is one parsed `glab api -i` response.
type apiResponse struct {
	Status int
	Header http.Header
	Body   []byte
}

const (
	// maxAPIAttempts caps how many times a single endpoint is retried.
	maxAPIAttempts = 6
	// rateLimitFloor is how many requests we keep in reserve. Once
	// ratelimit-remaining drops below this, we sleep until ratelimit-reset
	// rather than pushing into the 429 zone.
	rateLimitFloor = 50
)

// throttle is the process-wide GitLab rate-limit gate. Every API call waits on
// it, so a 429 (or a near-exhausted budget) pauses all callers, not just the
// one that saw the header.
var throttle struct {
	mu    sync.Mutex
	until time.Time
}

// throttleWait blocks until any globally scheduled pause has elapsed.
func throttleWait() {
	for {
		throttle.mu.Lock()
		until := throttle.until
		throttle.mu.Unlock()
		d := time.Until(until)
		if d <= 0 {
			return
		}
		if d > 5*time.Minute { // sanity clamp against a bogus reset header
			d = 5 * time.Minute
		}
		time.Sleep(d)
	}
}

// throttlePauseUntil schedules a global pause, extending any existing one.
func throttlePauseUntil(t time.Time) {
	throttle.mu.Lock()
	defer throttle.mu.Unlock()
	if t.After(throttle.until) {
		throttle.until = t
	}
}

// glabAPI performs a GET against the GitLab API through glab, retrying on rate
// limits (429) and transient upstream failures (5xx, "Bad Gateway", broken
// connections from a saturated local proxy).
func glabAPI(endpoint string) (*apiResponse, error) {
	var lastErr error
	for attempt := 1; attempt <= maxAPIAttempts; attempt++ {
		throttleWait()

		resp, err := runGlabAPIOnce(endpoint)
		if err != nil {
			// No HTTP response at all: transport-level failure. Back off and
			// retry; these are usually transient (proxy out of file
			// descriptors, connection reset, Bad Gateway before headers).
			lastErr = err
			if attempt == maxAPIAttempts {
				break
			}
			time.Sleep(backoff(attempt))
			continue
		}

		switch {
		case resp.Status == http.StatusTooManyRequests:
			wait := retryAfter(resp.Header, backoff(attempt))
			throttlePauseUntil(time.Now().Add(wait))
			lastErr = fmt.Errorf("rate limited (HTTP 429) on %s", endpoint)
			if attempt == maxAPIAttempts {
				return nil, lastErr
			}
			continue

		case resp.Status >= 500:
			lastErr = fmt.Errorf("HTTP %d on %s: %s", resp.Status, endpoint, snippet(resp.Body))
			if attempt == maxAPIAttempts {
				return nil, lastErr
			}
			time.Sleep(backoff(attempt))
			continue

		case resp.Status >= 400:
			// Client errors are not worth retrying.
			return nil, fmt.Errorf("HTTP %d on %s: %s", resp.Status, endpoint, snippet(resp.Body))
		}

		// Success. If we are close to the limit, pause everyone until the
		// window resets instead of racing into a 429.
		if remaining, ok := headerInt(resp.Header, "RateLimit-Remaining"); ok && remaining < rateLimitFloor {
			if reset, ok := headerInt(resp.Header, "RateLimit-Reset"); ok {
				throttlePauseUntil(time.Unix(int64(reset), 0))
			}
		}
		return resp, nil
	}
	return nil, lastErr
}

// runGlabAPIOnce runs `glab api -i` once and parses the response. It returns a
// non-nil *apiResponse whenever headers came back, even for 4xx/5xx, because
// glab exits non-zero in those cases but still prints the response.
func runGlabAPIOnce(endpoint string) (*apiResponse, error) {
	cmd := exec.Command("glab", "api", "-i", endpoint)
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	runErr := cmd.Run()

	resp, parseErr := parseAPIResponse(stdout.Bytes())
	if parseErr != nil {
		if runErr != nil {
			return nil, fmt.Errorf("glab api %s failed: %w (stderr: %s)", endpoint, runErr, strings.TrimSpace(stderr.String()))
		}
		return nil, fmt.Errorf("glab api %s: %w", endpoint, parseErr)
	}
	return resp, nil
}

// parseAPIResponse splits the `-i` output into status line, headers and body.
func parseAPIResponse(out []byte) (*apiResponse, error) {
	sep := []byte("\r\n\r\n")
	idx := bytes.Index(out, sep)
	if idx < 0 {
		sep = []byte("\n\n")
		idx = bytes.Index(out, sep)
	}
	if idx < 0 {
		return nil, errors.New("no HTTP header block in glab output")
	}
	head := string(out[:idx])
	body := out[idx+len(sep):]

	lines := strings.Split(strings.ReplaceAll(head, "\r\n", "\n"), "\n")
	if len(lines) == 0 || !strings.HasPrefix(lines[0], "HTTP/") {
		return nil, errors.New("malformed HTTP status line in glab output")
	}
	fields := strings.Fields(lines[0])
	if len(fields) < 2 {
		return nil, fmt.Errorf("malformed HTTP status line %q", lines[0])
	}
	status, err := strconv.Atoi(fields[1])
	if err != nil {
		return nil, fmt.Errorf("malformed HTTP status code %q", fields[1])
	}

	header := http.Header{}
	for _, line := range lines[1:] {
		name, value, ok := strings.Cut(line, ":")
		if !ok {
			continue
		}
		header.Add(strings.TrimSpace(name), strings.TrimSpace(value))
	}

	// glab appends its own error line after the body on non-2xx; trim it so
	// the body stays parseable as JSON.
	body = bytes.TrimSpace(body)
	return &apiResponse{Status: status, Header: header, Body: body}, nil
}

// retryAfter derives how long to wait from Retry-After or RateLimit-Reset,
// falling back to the caller's backoff.
func retryAfter(h http.Header, fallback time.Duration) time.Duration {
	if secs, ok := headerInt(h, "Retry-After"); ok && secs > 0 {
		return time.Duration(secs) * time.Second
	}
	if reset, ok := headerInt(h, "RateLimit-Reset"); ok {
		if d := time.Until(time.Unix(int64(reset), 0)); d > 0 {
			return d + time.Second
		}
	}
	return fallback
}

func headerInt(h http.Header, name string) (int, bool) {
	v := h.Get(name)
	if v == "" {
		return 0, false
	}
	n, err := strconv.Atoi(v)
	if err != nil {
		return 0, false
	}
	return n, true
}

// backoff returns an exponential delay for the given 1-based attempt, capped
// at 30s. The jitter is derived from the clock so concurrent callers spread
// out instead of retrying in lockstep.
func backoff(attempt int) time.Duration {
	d := time.Second << (attempt - 1)
	if d > 30*time.Second {
		d = 30 * time.Second
	}
	jitter := time.Duration(time.Now().UnixNano()%int64(d/2+1)) - d/4
	return d + jitter
}

// snippet collapses a multi-line command output into one short line so a
// failure stays readable in the middle of a long progress list.
func snippet(b []byte) string {
	s := strings.Join(strings.Fields(string(b)), " ")
	if len(s) > 200 {
		s = s[:200] + "…"
	}
	return s
}
