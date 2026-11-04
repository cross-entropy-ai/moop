# Moop

Transcribe audio from the terminal. Moop is the companion to [Meep](https://github.com/cross-entropy-ai/meep): Meep records the meeting, Moop writes it down.

Moop is a thin client for [audio-transcribe](https://github.com/Junyi-99/audio-transcribe) (hosted at <https://audio.xtra.science>). The server does the work: format conversion, storage, and Volcengine speech recognition. Moop uploads, waits, and prints the transcript.

## Install

```sh
brew install cross-entropy-ai/tap/moop
```

Upgrade with `brew upgrade moop`. Prebuilt for macOS and Linux on arm64 and amd64; or `go install github.com/cross-entropy-ai/moop@latest`, or download an archive from [Releases](https://github.com/cross-entropy-ai/moop/releases).

## Release

Push a tag like `v0.1.0`. The release workflow runs the tests, builds the archives with `scripts/release.py`, and publishes them with `moop.rb` and `checksums.txt`; the tap picks up `moop.rb` within half an hour.

## Sign in

Create a token at <https://audio.xtra.science/tokens>, then:

```sh
moop login        # paste the token; saved to ~/.config/moop/config.json (0600)
moop whoami
```

Or set `MOOP_TOKEN` in the environment.

## Use

```sh
moop talk.m4a                               # transcript on stdout
moop -s meeting -l zh-CN standup.m4a > standup.txt
moop -o talk.srt talk.m4a                   # format follows the extension
moop --hotwords @names.txt --context "Q4 planning, Alice and Bob" call.mp3
moop https://example.com/podcast.mp3        # the server fetches the URL
moop ~/Recordings/2026-11-03_101500         # a Meep folder: uses mixed.m4a
```

| Option | Meaning |
| --- | --- |
| `-l, --language` | `zh-CN`, `en-US`, `ja-JP`, … Default: auto-detect |
| `-s, --speakers` | `off` (default), `speaker` (conversation, ≤5 people), `meeting` (long, many people). Needs `zh-CN` or auto |
| `-e, --emotion` | Tag each sentence with an emotion |
| `--hotwords` | Names and jargon to favour, comma or newline separated; `@file` reads a file |
| `--context` | What the recording is about; `@file` reads a file |
| `-f, --format` | `txt` (default), `srt`, `json` |
| `-o, --output` | Write to a file instead of stdout, atomically |
| `--timeout` | Give up after N seconds |
| `-q, --quiet` | No progress on stderr |

Input formats: pcm, opus, mp3, wav, spx, ogg, amr, aac, m4a; up to 512 MB and 5 hours.

stdout carries only the transcript; progress and errors go to stderr. Exit codes: 0 ok, 1 failed, 2 bad usage, 3 timed out. `moop help` lists everything.

## Jobs

```sh
moop job ls -n 5                 # recent jobs: id, status, created, length, source, file
moop job wait d82bea93           # block until done; exit 0 done, 1 failed/cancelled, 3 timeout
moop job get d82bea93 --srt      # --txt (default), --srt or --json; -o to write a file
moop job cancel d82bea93         # stop the server working on it
```

Any unique id prefix works. If you interrupt `moop <file>`, the job keeps running on the server; `moop job wait` picks it up again.

## With Meep

Meep › Preferences › Agent › Run After Saving:

```sh
/opt/homebrew/bin/moop -s meeting -o "$1/transcript.txt"
```

Meep adds the recording folder to the end of the command (it is also `$1`), so the command must not name it again. Every recording then gets a `transcript.txt` next to its `mixed.m4a`. More recipes, such as a summary by an agent: [Meep › Recipes](https://github.com/cross-entropy-ai/meep#recipes).

## License

MIT
