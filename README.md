# aws-cost-slack

[![ci](https://github.com/moveeeax/aws-cost-slack/actions/workflows/ci.yml/badge.svg)](https://github.com/moveeeax/aws-cost-slack/actions/workflows/ci.yml)
[![Go](https://img.shields.io/badge/Go-1.26%2B-00ADD8?logo=go&logoColor=white)](go.mod)
[![License: MIT](https://img.shields.io/badge/License-MIT-yellow.svg)](LICENSE)

A morning cost pulse for your team, straight into Slack. `aws-cost-slack` reads
yesterday's AWS spend from Cost Explorer, diffs it against the day before and
month-to-date, flags anomalies, and posts a clean Slack Block Kit message.

## How it works

Three Cost Explorer windows are pulled and normalized into a flat
`service → amount` map each:

- **yesterday** — the full calendar day just ended, grouped by `SERVICE`
  (or `LINKED_ACCOUNT`);
- **day before** — for the day-over-day (DoD) delta and trend arrow;
- **month-to-date** — running total since the 1st.

The report builder ranks yesterday's spend, joins each group to its day-before
figure, computes deltas and trend arrows (`▲`/`▼`/`▬`), and flags a group as an
**anomaly** when its DoD increase clears both a percentage threshold *and* a
small absolute-dollar floor (so a `$0.00 → $0.50` service never pages anyone).
The result renders to Block Kit and posts to an incoming webhook.

Two more Cost Explorer calls feed the month-end line: last month's total and
`GetCostForecast` for the rest of the current month. The message shows
`Month-end forecast: $X (+Y% vs last month)`, where `$X` is month-to-date plus
the forecast. When the reported day is the last of its month the line becomes
`Month total` and no forecast is requested. A failed forecast (new account
without enough history, missing `ce:GetCostForecast` permission) prints a
warning to stderr and the report goes out without that line.

All of the interesting logic lives in pure functions (`internal/costs`,
`internal/slack`) that are unit-tested against fixtures — no live billing
account required.

## Install

```sh
go install github.com/moveeeax/aws-cost-slack/cmd/aws-cost-slack@latest
```

or build from source:

```sh
go build -o aws-cost-slack ./cmd/aws-cost-slack
```

## Usage

Point it at a Slack incoming webhook. AWS credentials come from the usual SDK
chain (env, shared config, or an IAM role):

```sh
aws-cost-slack --webhook "$SLACK_URL" --group-by SERVICE
```

Flags:

| flag | default | meaning |
|------|---------|---------|
| `--webhook` | `$SLACK_WEBHOOK_URL` | Slack incoming-webhook URL |
| `--group-by` | `SERVICE` | `SERVICE` or `LINKED_ACCOUNT` |
| `--metric` | `UnblendedCost` | Cost Explorer metric |
| `--top` | `8` | how many groups to list |
| `--threshold` | `50` | DoD % increase that flags an anomaly |
| `--region` | SDK default | Cost Explorer endpoint region |
| `--dry-run` | `false` | print the Slack payload, don't post |
| `--input` | — | render from a saved JSON fixture (demo/testing) |
| `--forecast` | `true` | month-end forecast and last-month comparison |
| `--budget` | `0` | monthly budget amount; adds a progress bar, warns at 80% and 100% |
| `--budget-name` | — | read the monthly limit from this AWS Budgets budget instead |
| `--baseline-days` | `0` | score anomalies against a rolling baseline of N days (0 = DoD) |
| `--sigma` | `2.5` | baseline mode: standard deviations above the mean that flag an anomaly |
| `--min-delta` | `1` | absolute increase an anomaly must also clear, in currency units |
| `--state` | — | alert state file: alert once per anomaly, report resolutions |
| `--alerts-only` | `false` | post only when an anomaly is new or resolved |
| `--backtest` | `0` | print what would have fired over the last N days, then exit |

Preview the message without AWS or Slack, using the bundled fixture:

```sh
$ aws-cost-slack --input examples/input.json --group-by SERVICE --top 5 --dry-run
{
  "text": "AWS cost for 2026-08-02: $209.58 (+10.7% DoD)",
  "blocks": [
    { "type": "header", "text": { "type": "plain_text", "text": "💸 AWS cost — 2026-08-02", "emoji": true } },
    ...
    { "type": "section", "text": { "type": "mrkdwn",
      "text": ":rotating_light: *Anomaly (> +100.0% DoD):*\n• *Amazon CloudWatch* $3.00 → $30.00  (+900.0%)" } },
    ...
  ]
}
```

The full rendered payload is checked in at
[`examples/slack_message.json`](examples/slack_message.json) (rendered with
`--top 5 --threshold 100 --budget 6000`).

### Budget

`--budget 5000` compares month-to-date spend with a fixed monthly amount.
`--budget-name team-prod` reads the limit of an existing AWS Budgets budget
instead (`sts:GetCallerIdentity` for the account id, then
`budgets:DescribeBudget`); only `MONTHLY` `COST` budgets with a fixed limit in
the report currency are accepted. The budget line shows spend, a ten-cell bar
and the percentage used, with `:warning:` from 80% and `:rotating_light:` from
100%. With a forecast it also shows the projected month end as a percentage of
the budget and warns when that crosses 100%.

### Rolling baseline and alert state

Day-over-day comparison is noisy for spend that follows a weekly cycle.
`--baseline-days 28` fetches 28 days of daily history and scores yesterday
against the mean and standard deviation of the same kind of day: weekdays
against weekdays, Saturdays and Sundays against weekend days (with fewer than
three matching days in the window, all days are used). A group is an anomaly
when it is `--sigma` deviations above its baseline *and* at least
`--min-delta` above the mean in money, so a 400% jump on a $2 service stays
quiet with `--min-delta 50`. Each anomaly line carries the last seven days as
a sparkline.

`--state anomalies.json` remembers which anomalies were already posted
(fingerprint: group-by dimension and group name). An anomaly that is still
open on the next run is marked `ongoing since <date>`; an open alert whose
group is back within one standard deviation of its baseline is listed under
`Resolved` once and dropped from the file. Add `--alerts-only` to post only
when something is new or resolved, which makes an hourly cron safe:

```sh
aws-cost-slack --baseline-days 28 --sigma 2.5 --min-delta 50 \
  --state ./anomalies.json --alerts-only --webhook "$SLACK_URL"
```

Without `--baseline-days`, the state works the same way with the DoD rule, and
an alert resolves as soon as the group is no longer flagged.

To tune thresholds before turning alerts on, replay a past window. Nothing is
posted:

```sh
$ aws-cost-slack --input examples/baseline_input.json --baseline-days 14 --backtest 14
2026-07-21  fire     Amazon S3  $48.00 vs baseline $15.65 (55.8σ)
2026-07-23  resolve  Amazon S3  back to $16.00, alerted since 2026-07-21
2026-08-02  fire     Amazon CloudWatch  $30.00 vs baseline $3.08 (52.6σ)
2 fired, 1 resolved over 14 replayed days
```

Against AWS, `--backtest N` makes one `GetCostAndUsage` call (paged) for
`N + baseline-days` days of daily data. The baseline groups by one dimension,
so `--group-by SERVICE` gives per-service baselines and
`--group-by LINKED_ACCOUNT` per-account ones.

## Run it on a schedule (Lambda)

The same binary is a Lambda handler when `AWS_LAMBDA_RUNTIME_API` is set —
configure it with env vars (`SLACK_WEBHOOK_URL`, `GROUP_BY`, `METRIC`, `TOP`,
`THRESHOLD`, `FORECAST`, `BUDGET`, `BUDGET_NAME`, `BASELINE_DAYS`, `SIGMA`,
`MIN_DELTA`) and let EventBridge Scheduler trigger it daily. The alert state
file is CLI-only: Lambda posts the full report every run. A ready Terraform
example (IAM role scoped to `ce:GetCostAndUsage` and `ce:GetCostForecast`,
plus `budgets:ViewBudget` on the named budget when `budget_name` is set,
function, and daily schedule) lives in [`deploy/terraform/`](deploy/terraform):

```sh
GOOS=linux GOARCH=arm64 CGO_ENABLED=0 go build -o bootstrap ./cmd/aws-cost-slack
zip -j deploy/terraform/function.zip bootstrap
cd deploy/terraform
terraform init && terraform apply -var "slack_webhook_url=$SLACK_URL"
```

## Development

```sh
go test ./...        # unit tests
go vet ./...
gofmt -l .           # should print nothing
```

## License

MIT — see [LICENSE](LICENSE).
