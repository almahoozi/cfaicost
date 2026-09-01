# cfaicost

`cfaicost` fetches Cloudflare AI Gateway logs and prints a cost report for one user.

## Install

```sh
go install github.com/almahoozi/cfaicost@latest
```

Make sure Go's install directory is in your `PATH`, then run:

```sh
cfaicost setup
```

Setup asks for your Cloudflare account ID, gateway name, user ID, and API token. The account, gateway, and user ID are saved in the user configuration directory. The token is saved in the operating system keychain.

To replace the token later:

```sh
cfaicost set-token
```

To print the installed version:

```sh
cfaicost version
```

## Usage

With no arguments, `cfaicost` fetches the last 30 days and prints a report grouped by session and provider/model:

```sh
cfaicost
```

The report includes request counts, sessions, costs, and totals by model. Historical days are cached locally; today is always fetched. Use `-f` to fetch everything again.

For session grouping and filtering to work, your requests must include an `x-session-id` metadata header.

### Options

- `-d 7d` — look back over a duration. Go durations such as `168h` also work.
- `--today` — use the range from the start of today in your local timezone until now.
- `--start TIME --end TIME` — use an explicit RFC3339 time range.
- `--daily` — include daily totals.
- `--all` — include daily totals for each provider/model. Requires `--daily`.
- `--tokens` — show input and output token counts.
- `--join` — combine all models used in a session into one row.
- `-u USER` — fetch the report for a different user ID than the configured user.
- `-s ID` — show only one session.
- `--ua` — show user-agent values.
- `--column LABEL:KEY` — add a metadata column, for example `--column Region:cf.region`.
- `--raw` — print the report's Markdown source. It has no effect with `--json`.
- `--json` — print the report as a single-line, minified JSON object. It includes the same summary, overview, model totals, and optional daily sections as the Markdown report.
- `--utc` — display times in UTC instead of local time.
- `-f` — refetch data instead of using cached historical days.

For example:

```sh
cfaicost -d 7d --daily --tokens
```

To fetch a report for another user:

```sh
cfaicost -u USER_ID
```

Display the currently saved report defaults in copyable CLI form:

```sh
cfaicost defaults
# --daily --join
```

## Saved defaults

Set the report options you want to use every time:

```sh
cfaicost set-defaults --mode=base --ua --join
```

This saves settings like:

```json
"defaults": {
  "mode": "base",
  "ua": true,
  "join": true
}
```

With `default` mode, saved options are used only when you run `cfaicost` without other report options. With `base` mode, saved options are used first and can be combined with other options. The `--force` and `-s` options do not disable saved defaults.
