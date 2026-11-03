package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// Client talks to an audio-transcribe server (github.com/Junyi-99/audio-transcribe).
type Client struct {
	Server string // e.g. https://audio.xtra.science, no trailing slash
	Token  string
	HTTP   *http.Client
}

// Options mirror the server's POST /api/jobs form fields.
type Options struct {
	Language string // "" = auto-detect
	Speakers string // off | speaker | meeting
	Emotion  bool
	Hotwords string // comma / newline separated
	Context  string
}

type Job struct {
	ID                  string   `json:"id"`
	Stage               string   `json:"stage"` // pending converting uploading linking transcribing saving done error
	TranscribeSub       string   `json:"transcribe_sub"`
	TranscribeAttempt   int      `json:"transcribe_attempt"`
	TranscribeStartedAt *float64 `json:"transcribe_started_at"`
	UploadBytes         int64    `json:"upload_bytes"`
	UploadTotal         int64    `json:"upload_total"`
	Error               string   `json:"error"`
	Result              *Result  `json:"result"`
}

type Result struct {
	Text       string         `json:"text"`
	SRT        string         `json:"srt_text"`
	Silent     bool           `json:"silent"`
	DurationMS int64          `json:"duration_ms"`
	Files      map[string]any `json:"files"`
}

// APIError is a non-2xx answer from the server.
type APIError struct {
	Status int
	Detail string
}

func (e *APIError) Error() string {
	if e.Status == http.StatusUnauthorized {
		return "not signed in: " + e.Detail + " (run `moop login`)"
	}
	return fmt.Sprintf("server: %s (HTTP %d)", e.Detail, e.Status)
}

func (c *Client) do(req *http.Request, out any) error {
	if c.Token != "" {
		req.Header.Set("Authorization", "Bearer "+c.Token)
	}
	req.Header.Set("Accept", "application/json")
	req.Header.Set("User-Agent", "moop/"+version)
	hc := c.HTTP
	if hc == nil {
		hc = http.DefaultClient
	}
	resp, err := hc.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 64<<20))
	if err != nil {
		return err
	}
	if resp.StatusCode/100 != 2 {
		var e struct {
			Detail any `json:"detail"`
		}
		detail := strings.TrimSpace(string(body))
		if json.Unmarshal(body, &e) == nil && e.Detail != nil {
			if s, ok := e.Detail.(string); ok {
				detail = s
			} else {
				b, _ := json.Marshal(e.Detail)
				detail = string(b)
			}
		}
		if len(detail) > 300 {
			detail = detail[:300] + "…"
		}
		if detail == "" {
			detail = http.StatusText(resp.StatusCode)
		}
		return &APIError{Status: resp.StatusCode, Detail: detail}
	}
	if out == nil {
		return nil
	}
	if err := json.Unmarshal(body, out); err != nil {
		return fmt.Errorf("server sent unexpected response: %w", err)
	}
	return nil
}

func (c *Client) get(ctx context.Context, path string, out any) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.Server+path, nil)
	if err != nil {
		return err
	}
	return c.do(req, out)
}

// Me returns the account the token belongs to.
func (c *Client) Me(ctx context.Context) (name, email string, err error) {
	var me struct {
		OIDC bool `json:"oidc"`
		User struct {
			Name  string `json:"name"`
			Email string `json:"email"`
		} `json:"user"`
	}
	if err := c.get(ctx, "/auth/me", &me); err != nil {
		return "", "", err
	}
	if !me.OIDC {
		return "(no auth on this server)", "", nil
	}
	return me.User.Name, me.User.Email, nil
}

// SubmitFile streams a local file to the server. onSent, if set, is called
// with the number of bytes sent so far.
func (c *Client) SubmitFile(ctx context.Context, path string, o Options, onSent func(sent, total int64)) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer f.Close()
	st, err := f.Stat()
	if err != nil {
		return "", err
	}

	pr, pw := io.Pipe()
	mw := multipart.NewWriter(pw)
	go func() {
		err := func() error {
			if err := writeFields(mw, o); err != nil {
				return err
			}
			part, err := mw.CreateFormFile("file", filepath.Base(path))
			if err != nil {
				return err
			}
			var r io.Reader = f
			if onSent != nil {
				r = &countingReader{r: f, total: st.Size(), cb: onSent}
			}
			if _, err := io.Copy(part, r); err != nil {
				return err
			}
			return mw.Close()
		}()
		pw.CloseWithError(err)
	}()

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.Server+"/api/jobs", pr)
	if err != nil {
		return "", err
	}
	req.Header.Set("Content-Type", mw.FormDataContentType())
	return c.submit(req)
}

// SubmitURL asks the server to fetch audio from a public URL.
func (c *Client) SubmitURL(ctx context.Context, url string, o Options) (string, error) {
	var buf strings.Builder
	mw := multipart.NewWriter(&buf)
	if err := writeFields(mw, o); err != nil {
		return "", err
	}
	if err := mw.WriteField("audio_url", url); err != nil {
		return "", err
	}
	if err := mw.Close(); err != nil {
		return "", err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.Server+"/api/jobs", strings.NewReader(buf.String()))
	if err != nil {
		return "", err
	}
	req.Header.Set("Content-Type", mw.FormDataContentType())
	return c.submit(req)
}

func (c *Client) submit(req *http.Request) (string, error) {
	var out struct {
		JobID string `json:"job_id"`
	}
	if err := c.do(req, &out); err != nil {
		return "", err
	}
	if out.JobID == "" {
		return "", errors.New("server returned no job id")
	}
	return out.JobID, nil
}

func writeFields(mw *multipart.Writer, o Options) error {
	emotion := ""
	if o.Emotion {
		emotion = "1"
	}
	for _, kv := range [][2]string{
		{"language", o.Language},
		{"speaker", o.Speakers},
		{"emotion", emotion},
		{"hotwords", o.Hotwords},
		{"context", o.Context},
	} {
		if err := mw.WriteField(kv[0], kv[1]); err != nil {
			return err
		}
	}
	return nil
}

func (c *Client) Job(ctx context.Context, id string) (*Job, error) {
	var j Job
	if err := c.get(ctx, "/api/jobs/"+id, &j); err != nil {
		return nil, err
	}
	return &j, nil
}

// Wait polls a job until it is done or failed, calling onUpdate on each poll.
// Transient network errors are retried; the server keeps working regardless.
func (c *Client) Wait(ctx context.Context, id string, every time.Duration, onUpdate func(*Job)) (*Job, error) {
	misses := 0
	for {
		j, err := c.Job(ctx, id)
		switch {
		case err == nil:
			misses = 0
			if onUpdate != nil {
				onUpdate(j)
			}
			switch j.Stage {
			case "done":
				if j.Result == nil {
					return nil, errors.New("server marked the job done without a result")
				}
				return j, nil
			case "error":
				msg := j.Error
				if msg == "" {
					msg = "unknown error"
				}
				return nil, errors.New(msg)
			}
		case ctx.Err() != nil:
			return nil, ctx.Err()
		default:
			var ae *APIError
			if errors.As(err, &ae) && ae.Status != http.StatusBadGateway &&
				ae.Status != http.StatusServiceUnavailable && ae.Status != http.StatusGatewayTimeout {
				return nil, err
			}
			if misses++; misses > 10 {
				return nil, fmt.Errorf("lost contact with server: %w", err)
			}
		}
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(every):
		}
	}
}

type countingReader struct {
	r     io.Reader
	sent  int64
	total int64
	cb    func(sent, total int64)
}

func (c *countingReader) Read(p []byte) (int, error) {
	n, err := c.r.Read(p)
	c.sent += int64(n)
	c.cb(c.sent, c.total)
	return n, err
}

// JobSummary is one row of `moop job ls`: an unfinished or failed job from
// /api/jobs, or a finished transcript from /api/transcripts (same id).
type JobSummary struct {
	ID         string  `json:"id"`
	Status     string  `json:"status"` // a server stage, "done", "failed" or "cancelled"
	Filename   string  `json:"filename"`
	CreatedAt  float64 `json:"created_at"`
	DurationMS int64   `json:"duration_ms,omitempty"`
	Via        string  `json:"via"`
	TokenName  string  `json:"token_name,omitempty"`
	Error      string  `json:"error,omitempty"`
}

func (c *Client) ListJobs(ctx context.Context) ([]JobSummary, error) {
	var active []struct {
		ID        string  `json:"id"`
		Stage     string  `json:"stage"`
		Filename  string  `json:"filename"`
		CreatedAt float64 `json:"created_at"`
		Via       string  `json:"via"`
		TokenName string  `json:"token_name"`
		Error     string  `json:"error"`
		Cancelled bool    `json:"cancelled"`
	}
	if err := c.get(ctx, "/api/jobs?failed=1", &active); err != nil {
		return nil, err
	}
	var done struct {
		Items []struct {
			ID         string  `json:"id"`
			Filename   string  `json:"filename"`
			CreatedAt  float64 `json:"created_at"`
			DurationMS int64   `json:"duration_ms"`
			Via        string  `json:"via"`
			TokenName  string  `json:"token_name"`
		} `json:"items"`
	}
	if err := c.get(ctx, "/api/transcripts", &done); err != nil {
		return nil, err
	}
	seen := map[string]bool{}
	var out []JobSummary
	for _, t := range done.Items {
		seen[t.ID] = true
		out = append(out, JobSummary{ID: t.ID, Status: "done", Filename: t.Filename, CreatedAt: t.CreatedAt,
			DurationMS: t.DurationMS, Via: firstNonEmpty(t.Via, "web"), TokenName: t.TokenName})
	}
	for _, j := range active {
		if seen[j.ID] {
			continue
		}
		status := j.Stage
		if j.Stage == "error" {
			status = "failed"
			if j.Cancelled {
				status = "cancelled"
			}
		}
		out = append(out, JobSummary{ID: j.ID, Status: status, Filename: j.Filename, CreatedAt: j.CreatedAt,
			Via: firstNonEmpty(j.Via, "web"), TokenName: j.TokenName, Error: j.Error})
	}
	return out, nil
}

// Transcript is a finished job's stored result, as the server returns it.
func (c *Client) Transcript(ctx context.Context, id string) (map[string]any, error) {
	var t map[string]any
	if err := c.get(ctx, "/api/transcripts/"+id, &t); err != nil {
		return nil, err
	}
	return t, nil
}

func (c *Client) Cancel(ctx context.Context, id string) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.Server+"/api/jobs/"+id+"/cancel", nil)
	if err != nil {
		return err
	}
	return c.do(req, nil)
}

func isNotFound(err error) bool {
	var ae *APIError
	return errors.As(err, &ae) && ae.Status == http.StatusNotFound
}
