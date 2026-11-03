package main

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

// fakeServer imitates audio-transcribe's /api/jobs and /auth/me.
type fakeServer struct {
	mu       sync.Mutex
	form     map[string]string
	fileName string
	fileBody string
	polls    int
	failWith string
}

func (f *fakeServer) handler(t *testing.T) http.Handler {
	mux := http.NewServeMux()
	auth := func(w http.ResponseWriter, r *http.Request) bool {
		if r.Header.Get("Authorization") != "Bearer at_good" {
			w.WriteHeader(401)
			io.WriteString(w, `{"detail":"Invalid or revoked API token"}`)
			return false
		}
		return true
	}
	mux.HandleFunc("GET /auth/me", func(w http.ResponseWriter, r *http.Request) {
		if auth(w, r) {
			io.WriteString(w, `{"oidc":true,"authed":true,"user":{"name":"Ada","email":"ada@x.com"}}`)
		}
	})
	mux.HandleFunc("POST /api/jobs", func(w http.ResponseWriter, r *http.Request) {
		if !auth(w, r) {
			return
		}
		if err := r.ParseMultipartForm(1 << 20); err != nil {
			t.Errorf("parse form: %v", err)
		}
		f.mu.Lock()
		defer f.mu.Unlock()
		f.form = map[string]string{}
		for k, v := range r.MultipartForm.Value {
			f.form[k] = v[0]
		}
		if fh := r.MultipartForm.File["file"]; len(fh) > 0 {
			f.fileName = fh[0].Filename
			rd, _ := fh[0].Open()
			b, _ := io.ReadAll(rd)
			f.fileBody = string(b)
		}
		if f.form["speaker"] == "bogus" {
			w.WriteHeader(400)
			io.WriteString(w, `{"detail":"speaker must be one of off, speaker, meeting"}`)
			return
		}
		io.WriteString(w, `{"job_id":"j1"}`)
	})
	mux.HandleFunc("GET /api/jobs/j1", func(w http.ResponseWriter, r *http.Request) {
		if !auth(w, r) {
			return
		}
		f.mu.Lock()
		f.polls++
		n, failWith := f.polls, f.failWith
		f.mu.Unlock()
		switch {
		case n < 3:
			io.WriteString(w, `{"id":"j1","stage":"transcribing","transcribe_sub":"queued"}`)
		case failWith != "":
			json.NewEncoder(w).Encode(map[string]any{"id": "j1", "stage": "error", "error": failWith})
		default:
			io.WriteString(w, `{"id":"j1","stage":"done","result":{"text":"Speaker 1\n  hello","srt_text":"1\n00:00:00,000 --> 00:00:01,000\nhello\n","silent":false,"duration_ms":1000,"files":{}}}`)
		}
	})
	return mux
}

func setup(t *testing.T, f *fakeServer) (dir string) {
	t.Helper()
	srv := httptest.NewServer(f.handler(t))
	t.Cleanup(srv.Close)
	dir = t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(dir, "cfg"))
	t.Setenv("MOOP_SERVER", srv.URL)
	t.Setenv("MOOP_TOKEN", "at_good")
	old := pollInterval
	pollInterval = time.Millisecond
	t.Cleanup(func() { pollInterval = old })
	return dir
}

// runCapture runs moop with args and returns exit code, stdout, stderr.
func runCapture(t *testing.T, args ...string) (int, string, string) {
	t.Helper()
	outR, outW, _ := os.Pipe()
	errR, errW, _ := os.Pipe()
	oldOut, oldErr := os.Stdout, os.Stderr
	os.Stdout, os.Stderr = outW, errW
	var outB, errB strings.Builder
	var wg sync.WaitGroup
	wg.Add(2)
	go func() { io.Copy(&outB, outR); wg.Done() }()
	go func() { io.Copy(&errB, errR); wg.Done() }()
	code := run(context.Background(), args)
	outW.Close()
	errW.Close()
	wg.Wait()
	os.Stdout, os.Stderr = oldOut, oldErr
	return code, outB.String(), errB.String()
}

func TestTranscribeMeepFolder(t *testing.T) {
	f := &fakeServer{}
	dir := setup(t, f)
	rec := filepath.Join(dir, "2026-11-03_101500")
	os.MkdirAll(rec, 0o755)
	os.WriteFile(filepath.Join(rec, "mixed.m4a"), []byte("AUDIO"), 0o644)
	os.WriteFile(filepath.Join(dir, "words.txt"), []byte("TPU, JAX\n"), 0o644)

	out := filepath.Join(rec, "transcript.srt")
	code, stdout, stderr := runCapture(t, rec, "-s", "meeting", "-e", "-l", "zh-CN",
		"--hotwords", "@"+filepath.Join(dir, "words.txt"), "--context=weekly sync", "-o", out)
	if code != 0 {
		t.Fatalf("exit %d, stderr: %s", code, stderr)
	}
	if stdout != "" {
		t.Errorf("stdout should be empty with -o, got %q", stdout)
	}
	want := map[string]string{"speaker": "meeting", "emotion": "1", "language": "zh-CN",
		"hotwords": "TPU, JAX", "context": "weekly sync"}
	for k, v := range want {
		if f.form[k] != v {
			t.Errorf("form[%s] = %q, want %q", k, f.form[k], v)
		}
	}
	if f.fileName != "mixed.m4a" || f.fileBody != "AUDIO" {
		t.Errorf("uploaded %q = %q", f.fileName, f.fileBody)
	}
	b, _ := os.ReadFile(out)
	if !strings.HasPrefix(string(b), "1\n00:00:00,000") {
		t.Errorf(".srt output should pick srt format, got %q", b)
	}
}

func TestTranscribeStdoutAndJSON(t *testing.T) {
	f := &fakeServer{}
	dir := setup(t, f)
	audio := filepath.Join(dir, "a.mp3")
	os.WriteFile(audio, []byte("x"), 0o644)

	code, stdout, _ := runCapture(t, "-q", audio)
	if code != 0 || stdout != "Speaker 1\n  hello\n" {
		t.Fatalf("exit %d, stdout %q", code, stdout)
	}
	f.polls = 0
	code, stdout, _ = runCapture(t, "-q", "-f", "json", audio)
	var r Result
	if code != 0 || json.Unmarshal([]byte(stdout), &r) != nil || r.DurationMS != 1000 {
		t.Fatalf("exit %d, json %q", code, stdout)
	}
}

func TestURLInput(t *testing.T) {
	f := &fakeServer{}
	setup(t, f)
	code, _, stderr := runCapture(t, "-q", "https://example.com/talk.mp3")
	if code != 0 || f.form["audio_url"] != "https://example.com/talk.mp3" || f.fileName != "" {
		t.Fatalf("exit %d, form %v, stderr %s", code, f.form, stderr)
	}
}

func TestErrors(t *testing.T) {
	f := &fakeServer{failWith: "Audio could not be decoded"}
	dir := setup(t, f)
	audio := filepath.Join(dir, "a.wav")
	os.WriteFile(audio, []byte("x"), 0o644)
	bad := filepath.Join(dir, "a.flac")
	os.WriteFile(bad, []byte("x"), 0o644)

	cases := []struct {
		args []string
		code int
		msg  string
	}{
		{[]string{}, exitUsage, "USAGE"},
		{[]string{"-q", audio}, exitFailed, "Audio could not be decoded"},
		{[]string{"-s", "bogus", audio}, exitUsage, "--speakers must be one of"},
		{[]string{"--nope", audio}, exitUsage, "unknown option: --nope"},
		{[]string{"-f", "doc", audio}, exitUsage, "--format must be one of"},
		{[]string{audio, audio}, exitUsage, "one recording at a time"},
		{[]string{bad}, exitFailed, `unsupported format "flac"`},
		{[]string{dir}, exitFailed, "no mixed.m4a"},
		{[]string{"--timeout", "0", audio}, exitUsage, "--timeout"},
	}
	for _, c := range cases {
		f.polls = 0
		code, _, stderr := runCapture(t, c.args...)
		if code != c.code || !strings.Contains(stderr, c.msg) {
			t.Errorf("%v: exit %d, stderr %q; want exit %d containing %q", c.args, code, stderr, c.code, c.msg)
		}
	}

	t.Setenv("MOOP_TOKEN", "at_bad")
	code, _, stderr := runCapture(t, audio)
	if code != exitFailed || !strings.Contains(stderr, "moop login") {
		t.Errorf("bad token: exit %d, stderr %q", code, stderr)
	}
}

func TestLoginWhoamiLogout(t *testing.T) {
	f := &fakeServer{}
	setup(t, f)
	t.Setenv("MOOP_TOKEN", "")

	// login reads the token from stdin when it isn't a terminal.
	r, w, _ := os.Pipe()
	io.WriteString(w, "at_good\n")
	w.Close()
	oldIn := os.Stdin
	os.Stdin = r
	code, _, stderr := runCapture(t, "login")
	os.Stdin = oldIn
	if code != 0 || !strings.Contains(stderr, "Ada <ada@x.com>") {
		t.Fatalf("login: exit %d, %s", code, stderr)
	}
	p, _ := configPath()
	if st, err := os.Stat(p); err != nil || st.Mode().Perm() != 0o600 {
		t.Fatalf("config %s: %v %v", p, st, err)
	}

	code, stdout, _ := runCapture(t, "whoami")
	if code != 0 || stdout != "Ada <ada@x.com>\n" {
		t.Fatalf("whoami: exit %d, %q", code, stdout)
	}
	if code, _, _ = runCapture(t, "logout"); code != 0 {
		t.Fatal("logout failed")
	}
	if code, _, stderr = runCapture(t, "whoami"); code != exitFailed || !strings.Contains(stderr, "not signed in") {
		t.Fatalf("whoami after logout: exit %d, %s", code, stderr)
	}
}

func TestSavedTokenIsPerServer(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	t.Setenv("MOOP_SERVER", "")
	t.Setenv("MOOP_TOKEN", "")
	saveConfig(Config{Server: "https://a.example", Token: "at_a"})
	if c, _ := resolve(""); c.Server != "https://a.example" || c.Token != "at_a" {
		t.Errorf("default: %+v", c)
	}
	if c, _ := resolve("https://b.example/"); c.Server != "https://b.example" || c.Token != "" {
		t.Errorf("other server must not get a's token: %+v", c)
	}
}
