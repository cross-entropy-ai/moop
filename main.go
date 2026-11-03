// moop transcribes audio with an audio-transcribe server
// (https://github.com/Junyi-99/audio-transcribe). It pairs with Meep:
// `moop <recording-folder>` transcribes the folder's mixed.m4a.
package main

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/signal"
	"strings"
	"syscall"

	"golang.org/x/term"
)

var version = "dev" // set by -ldflags "-X main.version=…"

const (
	exitOK      = 0
	exitFailed  = 1
	exitUsage   = 2
	exitTimeout = 3
	exitSignal  = 130
)

const helpText = `moop — transcribe audio, e.g. Meep recordings.

USAGE
  moop [options] <audio-file | recording-folder | https-url>
      Transcribe one recording. The transcript goes to stdout (or -o).
      A folder means its mixed.m4a (a Meep recording folder).
      Formats: pcm opus mp3 wav spx ogg amr aac m4a; up to 512 MB and 5 hours.

  -l, --language <code>    zh-CN, en-US, ja-JP, … (default: auto-detect)
  -s, --speakers <mode>    off (default) | speaker (a conversation, ≤5 people)
                           | meeting (long multi-speaker meeting).
                           Speaker labels need --language zh-CN or auto.
  -e, --emotion            Tag each sentence with an emotion.
      --hotwords <list>    Names and jargon to favour, comma or newline separated.
      --context <text>     What the recording is about (topic, people, scene).
                           Both take @file to read the value from a file.
  -f, --format <fmt>       txt (default) | srt | json. With -o, defaults to the
                           file's extension (.srt → srt, .json → json).
  -o, --output <path>      Write here instead of stdout (atomically, on success).
      --timeout <sec>      Give up after this many seconds (default: no limit).
  -q, --quiet              No progress on stderr.
      --server <url>       Server (default: $MOOP_SERVER, the saved one, or
                           ` + defaultServer + `).

JOBS
  moop job ls [-n <count>] [--json]
      Recent jobs, newest first (default 20): finished, in progress, and
      failed ones the server still holds (a day). IDs are shown as 8-character
      prefixes; any unique prefix works below.
  moop job wait <id> [--timeout <sec>] [-q]
      Block until the job finishes. Exit 0 done, 1 failed or cancelled, 3 timed out.
  moop job get <id> [--txt | --srt | --json] [-o <path>]
      Print a finished job's transcript (default --txt).
  moop job cancel <id>
      Stop the server working on the job. Audio already handed to the
      recognizer still finishes (and is billed) there; the result is dropped.

ACCOUNT
  moop login [--server <url>]   Save an API token (create one at <server>/tokens).
                                Reads it from stdin when stdin is not a terminal.
  moop logout                   Forget the saved token.
  moop whoami                   Print the account the token belongs to.

OTHER
  moop version
  moop help

ENVIRONMENT
  MOOP_TOKEN    API token; overrides the saved one.
  MOOP_SERVER   Server URL.

CONVENTIONS
  stdout carries only the transcript (or the account, for whoami); progress and
  errors go to stderr. Exit codes: 0 ok, 1 failed, 2 bad usage, 3 timed out.
  An interrupted run stops waiting; the server finishes the job anyway
  ('moop job wait' picks it up again, 'moop job cancel' stops it).

WITH MEEP
  Preferences › Agent › Run After Saving:
    /opt/homebrew/bin/moop -s meeting -o "$1/transcript.txt" "$1"
`

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	os.Exit(run(ctx, os.Args[1:]))
}

func run(ctx context.Context, args []string) int {
	var err error
	cmd := ""
	if len(args) > 0 {
		cmd = args[0]
	}
	switch cmd {
	case "", "help", "-h", "--help":
		if cmd == "" {
			fmt.Fprint(os.Stderr, helpText)
			return exitUsage
		}
		fmt.Print(helpText)
		return exitOK
	case "version", "--version":
		fmt.Println(version)
		return exitOK
	case "login":
		err = cmdLogin(ctx, args[1:])
	case "logout":
		err = cmdLogout(args[1:])
	case "whoami":
		err = cmdWhoami(ctx, args[1:])
	case "job":
		err = cmdJob(ctx, args[1:])
	default:
		err = cmdTranscribe(ctx, args)
	}
	if err == nil {
		return exitOK
	}
	fmt.Fprintln(os.Stderr, "moop:", err)
	var ue usageError
	switch {
	case errors.As(err, &ue):
		fmt.Fprintln(os.Stderr, "Run `moop help` for usage.")
		return exitUsage
	case errors.Is(err, context.DeadlineExceeded):
		return exitTimeout
	case ctx.Err() != nil:
		return exitSignal
	}
	return exitFailed
}

func cmdLogin(ctx context.Context, args []string) error {
	p, err := newSpec("server=").parse(args)
	if err != nil {
		return err
	}
	if len(p.positional) > 0 {
		return usagef("login takes no arguments")
	}
	cfg, err := resolve(p.values["server"])
	if err != nil {
		return err
	}
	token, err := readToken(cfg.Server)
	if err != nil {
		return err
	}
	c := &Client{Server: cfg.Server, Token: token}
	name, email, err := c.Me(ctx)
	if err != nil {
		return err
	}
	path, err := saveConfig(Config{Server: cfg.Server, Token: token})
	if err != nil {
		return err
	}
	fmt.Fprintf(os.Stderr, "Signed in to %s as %s. Token saved to %s\n", cfg.Server, who(name, email), path)
	return nil
}

func readToken(server string) (string, error) {
	var token string
	if term.IsTerminal(int(os.Stdin.Fd())) {
		fmt.Fprintf(os.Stderr, "Create a token at %s/tokens, then paste it here: ", server)
		b, err := term.ReadPassword(int(os.Stdin.Fd()))
		fmt.Fprintln(os.Stderr)
		if err != nil {
			return "", err
		}
		token = string(b)
	} else {
		line, err := bufio.NewReader(os.Stdin).ReadString('\n')
		if err != nil && !errors.Is(err, io.EOF) {
			return "", err
		}
		token = line
	}
	token = strings.TrimSpace(token)
	if token == "" {
		return "", errors.New("no token given")
	}
	return token, nil
}

func cmdLogout(args []string) error {
	if len(args) > 0 {
		return usagef("logout takes no arguments")
	}
	path, err := removeConfig()
	if err != nil {
		return err
	}
	fmt.Fprintf(os.Stderr, "Removed %s. Revoke the token on the server's /tokens page if it may have leaked.\n", path)
	return nil
}

func cmdWhoami(ctx context.Context, args []string) error {
	p, err := newSpec("server=").parse(args)
	if err != nil {
		return err
	}
	if len(p.positional) > 0 {
		return usagef("whoami takes no arguments")
	}
	cfg, err := resolve(p.values["server"])
	if err != nil {
		return err
	}
	if cfg.Token == "" {
		return fmt.Errorf("not signed in to %s (run `moop login`)", cfg.Server)
	}
	name, email, err := (&Client{Server: cfg.Server, Token: cfg.Token}).Me(ctx)
	if err != nil {
		return err
	}
	fmt.Println(who(name, email))
	return nil
}

func who(name, email string) string {
	if email != "" && name != "" && name != email {
		return fmt.Sprintf("%s <%s>", name, email)
	}
	return firstNonEmpty(email, name, "(unknown)")
}
