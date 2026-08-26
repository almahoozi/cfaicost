# cfaicost

`cfaicost` fetches Cloudflare AI Gateway logs for one `cf.user_id` over a time range and prints a Glamour-rendered Markdown report. It groups request usage by session and provider/model, with overall and per-model totals.

## Usage

Create a Cloudflare API token that can read the account's AI Gateway logs, store it in the OS keychain, then run:

```sh
go run . setup
go run .
```

`setup` securely prompts for the token and stores it in the keychain for the configured account and gateway. To replace it later without changing the rest of the configuration, run:

```sh
go run . set-token
```

Account ID, gateway, and user ID are stored in the user configuration file. Run `go run . setup` to create or change them. By default, the request covers the last 30 days; supply an explicit RFC3339 range when needed:

```sh
go run . --start 2026-08-24T04:29:31.844Z --end 2026-08-25T04:29:31.844Z
```

Use `--duration` (or `-d`) for a lookback ending now. Standard Go durations work, and whole-day values such as `7d` are supported:

```sh
go run . -d 7d
```

The program fetches every API page, reads the matching bearer token from the OS keychain, and never writes the token to output or the config file.

Completed UTC days are cached under the operating system cache directory (for example, `~/Library/Caches/cfaicost` on macOS). Each cache file is scoped to its account, gateway, user, and date. Today is never cached; cached historical days are reused and only missing day ranges are fetched. Use `--force` (or `-f`) to refetch every requested day and refresh its historical cache files.

## Defaults and report options

Use `set-defaults` to save default report flags. With the default mode, defaults apply only when no non-`--force` flag is supplied. Base mode applies defaults first, then explicit flags add or override them:

```sh
# Reset saved defaults.
go run . set-defaults

# Use daily joined reports by default, and retain them when other flags are supplied.
go run . set-defaults --mode=base --join --daily
```

`--mode`/`-m` accepts `default` (the default) or `base`, case-insensitively. `--force` never suppresses defaults.

- `--daily` includes the all-model daily usage table.
- `--daily --all` additionally includes one daily table per provider/model, with a totals footer.
- `--tokens` adds input and output token columns; they are hidden by default.
- `--join` combines all provider/models used by each session into one row.
- `--ua` adds the user-agent column.
- `--raw` writes the Markdown source directly rather than rendering it through Glamour, suitable for redirecting to a file or sharing.
- Dates and times use the local timezone by default; `--utc` displays them in UTC. It can also be saved with `set-defaults --utc`.

## Render saved JSON

Pipe a Cloudflare logs response (the wrapper object with a `result` field) or a JSON array of log entries into the command. No API token, account ID, or gateway is used in this mode. If the input contains more than one `cf.user_id`, the configured user is selected; a single-user input is rendered as-is. A piped input is not time-filtered unless `--duration` is explicitly supplied.

```sh
cat logs.json | go run .
```
