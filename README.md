# firehose-detection
Watch the Bluesky firehose for posts and profiles that match your rules, and get them in front of a human (or straight to Bluesky moderation) fast.

## Why does this exist
Bluesky has a child safety problem. Accounts that exist to trade CSAM or groom kids show up constantly, and they use the same coded emoji, misspellings, and bio patterns over and over. I was reporting these by hand after stumbling across them, hours after they were created, and T&S was taking hours more on top of that. The firehose is public. The patterns are obvious. There is no reason a human should be the first thing that sees these.

So this subscribes to Jetstream, runs every new post and profile through a YAML file of phrases and regexes, and sends hits to a Discord channel with a "Report to Bluesky" button. Rules that have no false positives can skip the human and file the report the second the record lands.

It is a single Go binary with no database. Rules reload on SIGHUP. If it crashes it resumes from where it left off.

## What it does and doesn't do
* Scans `app.bsky.feed.post` (body, alt text, link card title/description/URL, link facets) and `app.bsky.actor.profile` (display name, bio, pronouns, website).
* Writes every hit to stdout as JSONL. This is the lossless record; pipe it wherever you want.
* Posts hits to Discord with Report and Dismiss buttons. Discord output is best-effort: if the channel can't keep up, detections are dropped and counted, never stalling the stream.
* Files Bluesky reports, either from the button or automatically for rules marked `auto_report`.
* **Never renders media.** Images, video, avatars, banners, and link thumbnails from matched records are treated as hazardous and do not reach Discord. You get a count ("2 images (not shown)") and nothing else. Record text is put in a code block so Discord can't unfurl links in it either. Tests enforce both of these. Do not "fix" this.
* Does not report anything to NCMEC or law enforcement. That is still on you.

## Running it
You need Go 1.27 or newer.

```
go build -o firehose-detection .
cp env.example .env
cp rules.example.yaml rules.yaml
$EDITOR .env rules.yaml
set -a; . ./.env; set +a
./firehose-detection -rules rules.yaml -cursor state.cursor
```

Flags:
* `-rules` path to the rules file. Default `rules.yaml`. Send SIGHUP to reload it without restarting. A broken file is rejected and the old rules stay active.
* `-cursor` file to persist the Jetstream cursor in. With it, a restart picks up where it left off (at-least-once, so you may see a duplicate or two after a crash). Without it, you start at the live tip and miss whatever happened while you were down.
* `-host` Jetstream host. Default `jetstream.us-west.bsky.network`.

Secrets come from the environment, not flags, so they stay out of `ps`. `env.example` walks through creating the Discord bot and the Bluesky app password step by step, including the permission bits and the "This interaction failed" gotcha. Read it.

What you get with each piece configured:
* Nothing but the binary: JSONL on stdout.
* `DISCORD_TOKEN` + `DISCORD_CHANNEL_ID`: hits in Discord with no buttons.
* Plus `BSKY_HANDLE` + `BSKY_APP_PASSWORD`: Report buttons work and `auto_report` rules are allowed. Use a dedicated moderation account and an app password. Never the account's real password.
* `DISCORD_REPORT_ROLE_ID` (optional): only that role can press Report. Without it, anyone who can see the channel can.

## Writing rules
`rules.yaml` is a list of rules. A rule fires if ANY of its phrases or patterns match.

```yaml
rules:
  - name: scammer
    report_reason: misleading-impersonation
    auto_report: true
    phrases:
      - 'some-known-scam-email@example.com'
```

* `name` required and unique. Shows up in Discord and in the report text sent to Bluesky.
* `phrases` literal substrings, case-insensitive.
* `patterns` Go RE2 regexes, case-sensitive unless you start them with `(?i)`. Use single quotes so YAML leaves your backslashes alone. No backreferences or lookaround; that's RE2.
* `collections` restrict the rule to `app.bsky.feed.post` and/or `app.bsky.actor.profile`. Default is both. Use this when a pattern means something different in a bio than in a post. 
* `report_reason` which Bluesky queue the report goes to. Default `spam`. Every reason in both the `com.atproto.moderation` and `tools.ozone.report` lexicons is available; the full list is in `rules.example.yaml`. Prefer the specific ones, they route straight to the right moderation team.
* `auto_report` files the report immediately on every hit, no human involved. The report text says it was filed automatically and lists every rule that fired. Reserve this for rules you have watched for a while and have never seen a false positive on. The process refuses to start (and SIGHUP reloads are rejected) if any rule sets this and there are no Bluesky credentials, because silently filing nothing is worse than not starting. There is no dedupe: an edit to a matched post, or a crash replay, files a second report.

Some advice on patterns:
* Use `[ \t]*` instead of `\s*` when you mean "some spaces". Profile fields and post lines are joined with newlines, and `\s` will happily match across them.
* If you're writing a "digits with junk between them" pattern, exclude Unicode letters with `\pL`, not just `A-Za-z`. Otherwise every Japanese post with two numbers in it is junk.
* Keep the real false positives you've seen as negative test cases somewhere, even if that somewhere is not this repo. `rules.yaml` is gitignored on purpose: the real rules are not published because they tell people exactly what to avoid. Tests in this repo only run against `rules.example.yaml`.

## Output
Each line on stdout is one detection:

```json
{"uri":"at://did:plc:.../app.bsky.actor.profile/self","did":"did:plc:...","collection":"app.bsky.actor.profile","operation":"create","seq":123,"time_us":1760000000000000,"text":"...","media":"1 other image (avatar/banner/thumbnail)","hits":[{"rule":"scammer","report_reason":"misleading-impersonation","matches":["some-known-scam-email@example.com"]}]}
```

`hits` lists every rule that fired with the specific phrases or regex matches, so you can grep for a rule's noise later. `auto_report: true` appears on hits from auto-filed rules.

Once a minute the log prints `records`, `matched`, and `stream_errors` counters so you can tell it's alive.

## Building and testing
```
go build -buildvcs=false -o firehose-detection .
go vet ./... && go test ./...
```

If you change anything in `rules.go` or add a YAML field, rebuild and restart. SIGHUP only reloads the file. The old binary will parse the new file, ignore the fields it doesn't know about, and quietly do the wrong thing.
