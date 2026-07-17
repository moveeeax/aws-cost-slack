# aws-cost-slack

> A morning cost pulse for your team, straight into Slack.

**Status:** 🚧 In development

## Overview

Daily AWS cost breakdown posted to Slack via Cost Explorer.

## Features

- Yesterday's spend by service and by linked account
- Day-over-day and month-to-date deltas with trend arrows
- Anomaly hint when a service jumps > threshold
- Clean Slack Block Kit message
- Runs on a schedule (Lambda/cron)

## Stack

Go + aws-sdk-go-v2 Cost Explorer + Slack webhook.

## Usage

```bash
aws-cost-slack --webhook $SLACK_URL --group-by SERVICE
```

## License

MIT
