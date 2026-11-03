package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"sort"
	"strconv"
	"strings"
	"text/tabwriter"
	"time"
)

func cmdJob(ctx context.Context, args []string) error {
	if len(args) == 0 {
		return usagef("job needs a subcommand: ls, wait, get, cancel")
	}
	switch args[0] {
	case "ls", "list":
		return cmdJobLs(ctx, args[1:])
	case "wait":
		return cmdJobWait(ctx, args[1:])
	case "get":
		return cmdJobGet(ctx, args[1:])
	case "cancel":
		return cmdJobCancel(ctx, args[1:])
	}
	return usagef("unknown job subcommand: %s (ls, wait, get, cancel)", args[0])
}

// signedIn returns a client for the configured server, or an error if there
// is no token.
func signedIn(server string) (*Client, error) {
	cfg, err := resolve(server)
	if err != nil {
		return nil, err
	}
	if cfg.Token == "" {
		return nil, fmt.Errorf("not signed in to %s (run `moop login`, or set MOOP_TOKEN)", cfg.Server)
	}
	return &Client{Server: cfg.Server, Token: cfg.Token}, nil
}

// jobArgs parses `<id> [options]` and resolves an id prefix against the
// account's jobs, so the short ids `moop job ls` prints work.
func jobArgs(ctx context.Context, s spec, args []string) (parsed, *Client, string, error) {
	p, err := s.parse(args)
	if err != nil {
		return p, nil, "", err
	}
	if len(p.positional) != 1 {
		return p, nil, "", usagef("expected one job id")
	}
	c, err := signedIn(p.values["server"])
	if err != nil {
		return p, nil, "", err
	}
	id := p.positional[0]
	if len(id) == 32 {
		return p, c, id, nil
	}
	jobs, err := c.ListJobs(ctx)
	if err != nil {
		return p, nil, "", err
	}
	var hits []string
	for _, j := range jobs {
		if strings.HasPrefix(j.ID, id) {
			hits = append(hits, j.ID)
		}
	}
	switch len(hits) {
	case 0:
		return p, nil, "", fmt.Errorf("no job matches %q", id)
	case 1:
		return p, c, hits[0], nil
	}
	return p, nil, "", fmt.Errorf("%q matches %d jobs; give more of the id", id, len(hits))
}

func cmdJobLs(ctx context.Context, args []string) error {
	p, err := newSpec("n,n=", "json", "server=").parse(args)
	if err != nil {
		return err
	}
	if len(p.positional) > 0 {
		return usagef("job ls takes no arguments")
	}
	n := 20
	if v := p.values["n"]; v != "" {
		if n, err = strconv.Atoi(v); err != nil || n <= 0 {
			return usagef("-n must be a positive number")
		}
	}
	c, err := signedIn(p.values["server"])
	if err != nil {
		return err
	}
	jobs, err := c.ListJobs(ctx)
	if err != nil {
		return err
	}
	sort.SliceStable(jobs, func(a, b int) bool { return jobs[a].CreatedAt > jobs[b].CreatedAt })
	jobs = jobs[:min(n, len(jobs))]

	if p.flags["json"] {
		if jobs == nil {
			jobs = []JobSummary{}
		}
		b, _ := json.MarshalIndent(jobs, "", "  ")
		fmt.Println(string(b))
		return nil
	}
	if len(jobs) == 0 {
		return nil
	}
	w := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
	fmt.Fprintln(w, "ID\tSTATUS\tCREATED\tLENGTH\tSOURCE\tFILE")
	for _, j := range jobs {
		length := "-"
		if j.DurationMS > 0 {
			length = (time.Duration(j.DurationMS) * time.Millisecond).Round(time.Second).String()
		}
		source := j.Via
		if j.Via == "api" && j.TokenName != "" {
			source = "api:" + j.TokenName
		}
		created := time.Unix(int64(j.CreatedAt), 0).Format("2006-01-02 15:04")
		fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%s\t%s\n", j.ID[:8], j.Status, created, length, source, j.Filename)
	}
	return w.Flush()
}

// cmdJobWait blocks until the job finishes. Exit 0 = done, 1 = failed or
// cancelled. It prints nothing on stdout; fetch the result with `job get`.
func cmdJobWait(ctx context.Context, args []string) error {
	p, c, id, err := jobArgs(ctx, newSpec("timeout=", "quiet,q", "server="), args)
	if err != nil {
		return err
	}
	ctx, cancel, err := withTimeout(ctx, p.values["timeout"])
	if err != nil {
		return err
	}
	defer cancel()
	prog := newProgress(os.Stderr, p.flags["quiet"])
	defer prog.done()
	_, err = c.Wait(ctx, id, pollInterval, func(j *Job) { prog.set(describe(j)) })
	if isNotFound(err) {
		// The server forgets job state after a day; finished ones live on as transcripts.
		if _, terr := c.Transcript(ctx, id); terr == nil {
			return nil
		}
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return fmt.Errorf("timed out: %w", err)
	}
	return err
}

func cmdJobGet(ctx context.Context, args []string) error {
	p, c, id, err := jobArgs(ctx, newSpec("txt", "srt", "json", "output,o=", "server="), args)
	if err != nil {
		return err
	}
	var picked []string
	for _, f := range formats {
		if p.flags[f] {
			picked = append(picked, f)
		}
	}
	if len(picked) > 1 {
		return usagef("pick one of --txt, --srt, --json")
	}
	format := "txt"
	if len(picked) == 1 {
		format = picked[0]
	}

	t, err := c.Transcript(ctx, id)
	if isNotFound(err) {
		if j, jerr := c.Job(ctx, id); jerr == nil {
			if j.Stage == "error" {
				return fmt.Errorf("job failed: %s", j.Error)
			}
			return fmt.Errorf("job is still %s; run `moop job wait %s` first", j.Stage, id[:8])
		}
		return fmt.Errorf("no job %s", id)
	}
	if err != nil {
		return err
	}
	var out []byte
	switch format {
	case "txt":
		s, _ := t["text"].(string)
		out = []byte(ensureNewline(s))
	case "srt":
		s, _ := t["srt_text"].(string)
		out = []byte(ensureNewline(s))
	case "json":
		out, _ = json.MarshalIndent(t, "", "  ")
		out = append(out, '\n')
	}
	return emit(p.values["output"], out)
}

func cmdJobCancel(ctx context.Context, args []string) error {
	p, c, id, err := jobArgs(ctx, newSpec("server="), args)
	if err != nil {
		return err
	}
	_ = p
	if err := c.Cancel(ctx, id); err != nil {
		return err
	}
	fmt.Fprintf(os.Stderr, "Cancelling %s. Audio already handed to the recognizer still finishes (and is billed) there.\n", id[:8])
	return nil
}

func withTimeout(ctx context.Context, v string) (context.Context, context.CancelFunc, error) {
	if v == "" {
		return ctx, func() {}, nil
	}
	sec, err := strconv.ParseFloat(v, 64)
	if err != nil || sec <= 0 {
		return ctx, nil, usagef("--timeout must be a positive number of seconds")
	}
	ctx, cancel := context.WithTimeout(ctx, time.Duration(sec*float64(time.Second)))
	return ctx, cancel, nil
}

// emit writes to stdout, or atomically to path.
func emit(path string, data []byte) error {
	if path == "" || path == "-" {
		_, err := os.Stdout.Write(data)
		return err
	}
	return writeAtomic(path, data, 0o644)
}
