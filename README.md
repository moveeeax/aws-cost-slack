# aws-cost-slack

[![ci](https://github.com/moveeeax/aws-cost-slack/actions/workflows/ci.yml/badge.svg)](https://github.com/moveeeax/aws-cost-slack/actions/workflows/ci.yml)
[![Go](https://img.shields.io/badge/Go-1.22-00ADD8?logo=go&logoColor=white)](go.mod)
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
[`examples/slack_message.json`](examples/slack_message.json).

## Run it on a schedule (Lambda)

The same binary is a Lambda handler when `AWS_LAMBDA_RUNTIME_API` is set —
configure it with env vars (`SLACK_WEBHOOK_URL`, `GROUP_BY`, `TOP`,
`THRESHOLD`) and let EventBridge Scheduler trigger it daily. A ready Terraform
example (IAM role scoped to `ce:GetCostAndUsage`, function, and daily schedule)
lives in [`deploy/terraform/`](deploy/terraform):

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
