package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"golang.org/x/term"
)

var (
	supportedExts = []string{"pcm", "opus", "mp3", "wav", "spx", "ogg", "amr", "aac", "m4a"}
	speakerModes  = []string{"off", "speaker", "meeting"}
	formats       = []string{"txt", "srt", "json"}
)

const (
	maxUpload   = 512 << 20 // server limit
	maxHotwords = 50_000    // server limits, in characters
	maxContext  = 5_000
)

var pollInterval = 2 * time.Second

var transcribeSpec = newSpec(
	"language,l=", "speakers,s=", "emotion,e", "hotwords=", "context=",
	"format,f=", "output,o=", "timeout=", "quiet,q", "server=",
)

func cmdTranscribe(ctx context.Context, args []string) error {
	p, err := transcribeSpec.parse(args)
	if err != nil {
		return err
	}
	switch len(p.positional) {
	case 0:
		return usagef("missing the audio file, folder or URL")
	case 1:
	default:
		return usagef("one recording at a time (got %d); use a shell loop or xargs for more", len(p.positional))
	}

	opts := Options{
		Language: p.values["language"],
		Speakers: firstNonEmpty(p.values["speakers"], "off"),
		Emotion:  p.flags["emotion"],
	}
	if !slices.Contains(speakerModes, opts.Speakers) {
		return usagef("--speakers must be one of %s", strings.Join(speakerModes, ", "))
	}
	if opts.Hotwords, err = valueOrFile(p.values["hotwords"]); err != nil {
		return err
	}
	if opts.Context, err = valueOrFile(p.values["context"]); err != nil {
		return err
	}
	if n := len([]rune(opts.Hotwords)); n > maxHotwords {
		return usagef("--hotwords is %d characters; the server takes at most %d", n, maxHotwords)
	}
	if n := len([]rune(opts.Context)); n > maxContext {
		return usagef("--context is %d characters; the server takes at most %d", n, maxContext)
	}

	output := p.values["output"]
	format := p.values["format"]
	if format == "" {
		format = "txt"
		if ext := strings.TrimPrefix(filepath.Ext(output), "."); slices.Contains(formats, ext) {
			format = ext
		}
	}
	if !slices.Contains(formats, format) {
		return usagef("--format must be one of %s", strings.Join(formats, ", "))
	}

	ctx, cancel, err := withTimeout(ctx, p.values["timeout"])
	if err != nil {
		return err
	}
	defer cancel()

	src := p.positional[0]
	isURL := strings.HasPrefix(src, "https://") || strings.HasPrefix(src, "http://")
	if !isURL {
		if src, err = resolveInput(src); err != nil {
			return err
		}
	}

	c, err := signedIn(p.values["server"])
	if err != nil {
		return err
	}
	prog := newProgress(os.Stderr, p.flags["quiet"])
	defer prog.done()

	var id string
	if isURL {
		prog.set("submitting")
		id, err = c.SubmitURL(ctx, src, opts)
	} else {
		id, err = c.SubmitFile(ctx, src, opts, func(sent, total int64) {
			prog.set(fmt.Sprintf("sending %d%%", percent(sent, total)))
		})
	}
	if err != nil {
		return err
	}

	job, err := c.Wait(ctx, id, pollInterval, func(j *Job) { prog.set(describe(j)) })
	if err != nil {
		if ctx.Err() != nil {
			prog.done()
			fmt.Fprintf(os.Stderr, "moop: stopped waiting; the server keeps going. Resume with: moop job wait %s\n", id[:8])
			if errors.Is(ctx.Err(), context.DeadlineExceeded) {
				return fmt.Errorf("timed out: %w", context.DeadlineExceeded)
			}
		}
		return err
	}
	prog.done()

	r := job.Result
	if r.Silent {
		fmt.Fprintln(os.Stderr, "moop: no speech found in the recording")
	}
	var out []byte
	switch format {
	case "txt":
		out = []byte(ensureNewline(r.Text))
	case "srt":
		out = []byte(ensureNewline(r.SRT))
	case "json":
		out, _ = json.MarshalIndent(r, "", "  ")
		out = append(out, '\n')
	}
	return emit(output, out)
}

// resolveInput turns a recording folder into its mixed.m4a and checks the file
// is something the server accepts.
func resolveInput(path string) (string, error) {
	st, err := os.Stat(path)
	if err != nil {
		return "", err
	}
	if st.IsDir() {
		mixed := filepath.Join(path, "mixed.m4a")
		if st, err = os.Stat(mixed); err != nil {
			return "", fmt.Errorf("%s has no mixed.m4a (is it a Meep recording folder?)", path)
		}
		path = mixed
	}
	ext := strings.ToLower(strings.TrimPrefix(filepath.Ext(path), "."))
	if !slices.Contains(supportedExts, ext) {
		return "", fmt.Errorf("%s: unsupported format %q (supported: %s)", path, ext, strings.Join(supportedExts, " "))
	}
	if st.Size() > maxUpload {
		return "", fmt.Errorf("%s is %d MB; the server takes at most %d MB", path, st.Size()>>20, maxUpload>>20)
	}
	if st.Size() == 0 {
		return "", fmt.Errorf("%s is empty", path)
	}
	return path, nil
}

// valueOrFile reads "@path" from a file ("@-" = stdin) and returns anything else as is.
func valueOrFile(v string) (string, error) {
	if !strings.HasPrefix(v, "@") {
		return v, nil
	}
	var b []byte
	var err error
	if v == "@-" {
		b, err = io.ReadAll(os.Stdin)
	} else {
		b, err = os.ReadFile(v[1:])
	}
	return strings.TrimSpace(string(b)), err
}

func describe(j *Job) string {
	switch j.Stage {
	case "pending":
		return "starting"
	case "converting":
		return "converting"
	case "uploading", "linking":
		if j.UploadTotal > 0 {
			return fmt.Sprintf("storing %d%%", percent(j.UploadBytes, j.UploadTotal))
		}
		return "storing"
	case "transcribing":
		s := "transcribing"
		if j.TranscribeSub == "queued" || j.TranscribeSub == "retrying" {
			s += " · " + j.TranscribeSub
		}
		if j.TranscribeAttempt > 1 {
			s += fmt.Sprintf(" · attempt %d", j.TranscribeAttempt)
		}
		if j.TranscribeStartedAt != nil {
			started := time.Unix(0, int64(*j.TranscribeStartedAt*1e9))
			s += " · " + time.Since(started).Truncate(time.Second).String()
		}
		return s
	case "saving":
		return "saving"
	case "done", "error":
		return "" // reported by the caller
	}
	return j.Stage
}

func percent(a, b int64) int64 {
	if b <= 0 {
		return 0
	}
	return min(100, a*100/b)
}

func ensureNewline(s string) string {
	if s == "" || strings.HasSuffix(s, "\n") {
		return s
	}
	return s + "\n"
}

// progress shows one status line on a terminal, or one line per change when
// stderr is a file or pipe (e.g. Meep's after-save log).
type progress struct {
	w     io.Writer
	tty   bool
	quiet bool
	last  string
	shown bool
}

func newProgress(f *os.File, quiet bool) *progress {
	return &progress{w: f, tty: term.IsTerminal(int(f.Fd())), quiet: quiet}
}

func (p *progress) set(s string) {
	if p.quiet || s == "" || s == p.last {
		return
	}
	if !p.tty {
		// Upload percentages would flood a log; report only the stage.
		stage, _, _ := strings.Cut(s, " ")
		prev, _, _ := strings.Cut(p.last, " ")
		p.last = s
		if stage != prev {
			fmt.Fprintln(p.w, "moop:", stage)
		}
		return
	}
	p.last = s
	p.shown = true
	fmt.Fprintf(p.w, "\r\033[Kmoop: %s", s)
}

func (p *progress) done() {
	if p.shown {
		fmt.Fprint(p.w, "\r\033[K")
		p.shown = false
	}
}
