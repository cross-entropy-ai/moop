package main

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

const (
	idDone    = "aaaa1111000000000000000000000000"
	idRunning = "bbbb2222000000000000000000000000"
	idFailed  = "bbbb3333000000000000000000000000"
)

func jobServer(t *testing.T) (cancelled *[]string) {
	cancelled = &[]string{}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /api/jobs", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("failed") != "1" {
			t.Error("ls should ask for failed jobs too")
		}
		io.WriteString(w, `[
		 {"id":"`+idRunning+`","stage":"transcribing","filename":"b.m4a","created_at":300,"via":"api","token_name":"laptop"},
		 {"id":"`+idFailed+`","stage":"error","cancelled":true,"error":"已取消","filename":"c.m4a","created_at":200}]`)
	})
	mux.HandleFunc("GET /api/transcripts", func(w http.ResponseWriter, r *http.Request) {
		io.WriteString(w, `{"stats":{},"items":[{"id":"`+idDone+`","filename":"a.m4a","created_at":100,"duration_ms":65000,"via":"web","token_name":""}]}`)
	})
	mux.HandleFunc("GET /api/transcripts/{id}", func(w http.ResponseWriter, r *http.Request) {
		if r.PathValue("id") != idDone {
			w.WriteHeader(404)
			io.WriteString(w, `{"detail":"Not found"}`)
			return
		}
		io.WriteString(w, `{"id":"`+idDone+`","text":"hello","srt_text":"1\n00:00:00,000 --> 00:00:01,000\nhello"}`)
	})
	mux.HandleFunc("GET /api/jobs/{id}", func(w http.ResponseWriter, r *http.Request) {
		switch r.PathValue("id") {
		case idRunning:
			io.WriteString(w, `{"id":"`+idRunning+`","stage":"transcribing"}`)
		case idFailed:
			io.WriteString(w, `{"id":"`+idFailed+`","stage":"error","error":"已取消","cancelled":true}`)
		default: // finished long ago: server no longer holds job state
			w.WriteHeader(404)
			io.WriteString(w, `{"detail":"Unknown job"}`)
		}
	})
	mux.HandleFunc("POST /api/jobs/{id}/cancel", func(w http.ResponseWriter, r *http.Request) {
		*cancelled = append(*cancelled, r.PathValue("id"))
		io.WriteString(w, `{"cancelling":true}`)
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	t.Setenv("MOOP_SERVER", srv.URL)
	t.Setenv("MOOP_TOKEN", "at_x")
	old := pollInterval
	pollInterval = time.Millisecond
	t.Cleanup(func() { pollInterval = old })
	return cancelled
}

func TestJobLs(t *testing.T) {
	jobServer(t)
	code, out, errOut := runCapture(t, "job", "ls", "-n", "2")
	if code != 0 {
		t.Fatalf("exit %d: %s", code, errOut)
	}
	lines := strings.Split(strings.TrimSpace(out), "\n")
	if len(lines) != 3 || !strings.HasPrefix(lines[0], "ID") {
		t.Fatalf("want header + 2 rows, got:\n%s", out)
	}
	if !strings.Contains(lines[1], "bbbb2222") || !strings.Contains(lines[1], "transcribing") || !strings.Contains(lines[1], "api:laptop") {
		t.Errorf("newest first, with source: %q", lines[1])
	}
	if !strings.Contains(lines[2], "cancelled") {
		t.Errorf("cancelled job: %q", lines[2])
	}

	code, out, _ = runCapture(t, "job", "ls", "--json")
	var rows []JobSummary
	if code != 0 || json.Unmarshal([]byte(out), &rows) != nil || len(rows) != 3 || rows[2].ID != idDone || rows[2].Status != "done" {
		t.Fatalf("json: exit %d %q", code, out)
	}
}

func TestJobGetWaitCancel(t *testing.T) {
	cancelled := jobServer(t)
	dir := t.TempDir()
	cases := []struct {
		args []string
		code int
		out  string // stdout, or stderr substring when code != 0
	}{
		{[]string{"job", "get", "aaaa"}, 0, "hello\n"},
		{[]string{"job", "get", idDone, "--srt"}, 0, "1\n00:00:00,000 --> 00:00:01,000\nhello\n"},
		{[]string{"job", "get", "bbbb2222"}, 1, "still transcribing"},
		{[]string{"job", "get", "bbbb3333"}, 1, "job failed: 已取消"},
		{[]string{"job", "get", "bbbb"}, 1, "matches 2 jobs"},
		{[]string{"job", "get", "zzzz"}, 1, "no job matches"},
		{[]string{"job", "get", "aaaa", "--txt", "--srt"}, 2, "pick one"},
		{[]string{"job", "wait", "aaaa"}, 0, ""}, // gone from /api/jobs, found as transcript
		{[]string{"job", "wait", "-q", "bbbb3333"}, 1, "已取消"},
		{[]string{"job", "wait", "-q", "--timeout", "0.05", "bbbb2222"}, 3, "timed out"},
		{[]string{"job", "cancel", "bbbb2222"}, 0, ""},
		{[]string{"job"}, 2, "subcommand"},
		{[]string{"job", "rm", "x"}, 2, "unknown job subcommand"},
	}
	for _, c := range cases {
		code, out, errOut := runCapture(t, c.args...)
		got := out
		if c.code != 0 {
			got = errOut
		}
		if code != c.code || (c.code == 0 && got != c.out) || (c.code != 0 && !strings.Contains(got, c.out)) {
			t.Errorf("%v: exit %d, stdout %q, stderr %q; want exit %d, %q", c.args, code, out, errOut, c.code, c.out)
		}
	}
	if len(*cancelled) != 1 || (*cancelled)[0] != idRunning {
		t.Errorf("cancelled %v", *cancelled)
	}

	p := filepath.Join(dir, "t.json")
	if code, _, _ := runCapture(t, "job", "get", "aaaa", "--json", "-o", p); code != 0 {
		t.Fatal("get -o failed")
	}
	if b, _ := os.ReadFile(p); !strings.Contains(string(b), `"text": "hello"`) {
		t.Errorf("json file: %s", b)
	}
}
