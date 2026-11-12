# Scheduled Lambda that runs aws-cost-slack once a day and posts to Slack.
#
# Build the binary for the custom runtime first (from the repo root):
#
#   GOOS=linux GOARCH=arm64 CGO_ENABLED=0 go build -o bootstrap ./cmd/aws-cost-slack
#   zip -j deploy/terraform/function.zip bootstrap
#
# Then:  terraform init && terraform apply -var 'slack_webhook_url=https://hooks.slack.com/...'
#
# Add -var 'state_bucket=my-bucket' to keep the alert state in S3 (one object,
# state_key), and -var 'alerts_only=true' to post only new or resolved anomalies.

terraform {
  required_version = ">= 1.5"
  required_providers {
    aws = {
      source  = "hashicorp/aws"
      version = ">= 5.0"
    }
  }
}

provider "aws" {
  region = var.region
}

variable "region" {
  type    = string
  default = "us-east-1"
}

variable "slack_webhook_url" {
  type      = string
  sensitive = true
}

variable "group_by" {
  type    = string
  default = "SERVICE"
}

variable "budget" {
  type        = number
  default     = 0
  description = "Monthly budget amount in the report currency (0 = no budget line)"
}

variable "budget_name" {
  type        = string
  default     = ""
  description = "AWS Budgets budget to read the monthly limit from, instead of var.budget"
}

variable "baseline_days" {
  type        = number
  default     = 0
  description = "Score anomalies against a rolling baseline of this many days (0 = day-over-day)"
}

variable "state_bucket" {
  type        = string
  default     = ""
  description = "Existing S3 bucket for the alert state object (empty = no state: every run posts the full report)"
}

variable "state_key" {
  type        = string
  default     = "aws-cost-slack/state.json"
  description = "Object key of the alert state inside state_bucket"

  validation {
    condition     = var.state_key != "" && !endswith(var.state_key, "/") && !startswith(var.state_key, "/")
    error_message = "state_key must name an object, e.g. aws-cost-slack/state.json."
  }
}

variable "alerts_only" {
  type        = bool
  default     = false
  description = "Post only when an anomaly is new or resolved (requires state_bucket)"
}

variable "schedule" {
  type        = string
  default     = "cron(0 8 * * ? *)" # 08:00 UTC daily
  description = "EventBridge Scheduler expression"
}

data "aws_iam_policy_document" "assume" {
  statement {
    actions = ["sts:AssumeRole"]
    principals {
      type        = "Service"
      identifiers = ["lambda.amazonaws.com"]
    }
  }
}

resource "aws_iam_role" "fn" {
  name               = "aws-cost-slack"
  assume_role_policy = data.aws_iam_policy_document.assume.json
}

data "aws_caller_identity" "current" {}

locals {
  state_enabled = var.state_bucket != ""
  state_uri     = local.state_enabled ? "s3://${var.state_bucket}/${var.state_key}" : ""
}

# Cost Explorer is global and has no resource-level permissions. The forecast
# call powers the month-end line; budgets:ViewBudget is only granted when a
# named budget is configured.
data "aws_iam_policy_document" "ce" {
  statement {
    actions   = ["ce:GetCostAndUsage", "ce:GetCostForecast"]
    resources = ["*"]
  }

  dynamic "statement" {
    for_each = var.budget_name == "" ? [] : [var.budget_name]
    content {
      actions   = ["budgets:ViewBudget"]
      resources = ["arn:aws:budgets::${data.aws_caller_identity.current.account_id}:budget/${statement.value}"]
    }
  }
}

resource "aws_iam_role_policy" "ce" {
  name   = "cost-explorer-read"
  role   = aws_iam_role.fn.id
  policy = data.aws_iam_policy_document.ce.json
}

# Read and write the one state object, nothing else in the bucket. Without
# s3:ListBucket, S3 answers GetObject on a missing key with 403 instead of
# 404, so the object is created below rather than on the first run.
data "aws_iam_policy_document" "state" {
  count = local.state_enabled ? 1 : 0

  statement {
    actions   = ["s3:GetObject", "s3:PutObject"]
    resources = ["arn:aws:s3:::${var.state_bucket}/${var.state_key}"]
  }
}

resource "aws_iam_role_policy" "state" {
  count  = local.state_enabled ? 1 : 0
  name   = "alert-state"
  role   = aws_iam_role.fn.id
  policy = data.aws_iam_policy_document.state[0].json
}

# Empty state. The function rewrites it on every run; Terraform only creates
# it and then leaves the content alone.
resource "aws_s3_object" "state" {
  count        = local.state_enabled ? 1 : 0
  bucket       = var.state_bucket
  key          = var.state_key
  content      = jsonencode({ open = {} })
  content_type = "application/json"

  lifecycle {
    ignore_changes = [content, content_type, etag]
  }
}

resource "aws_iam_role_policy_attachment" "logs" {
  role       = aws_iam_role.fn.name
  policy_arn = "arn:aws:iam::aws:policy/service-role/AWSLambdaBasicExecutionRole"
}

resource "aws_lambda_function" "cost_slack" {
  function_name    = "aws-cost-slack"
  role             = aws_iam_role.fn.arn
  runtime          = "provided.al2023"
  handler          = "bootstrap"
  architectures    = ["arm64"]
  filename         = "${path.module}/function.zip"
  source_code_hash = filebase64sha256("${path.module}/function.zip")
  timeout          = 30

  environment {
    variables = {
      SLACK_WEBHOOK_URL = var.slack_webhook_url
      GROUP_BY          = var.group_by
      BUDGET            = tostring(var.budget)
      BUDGET_NAME       = var.budget_name
      BASELINE_DAYS     = tostring(var.baseline_days)
      STATE             = local.state_uri
      ALERTS_ONLY       = tostring(var.alerts_only)
    }
  }

  depends_on = [aws_iam_role_policy.state, aws_s3_object.state]

  lifecycle {
    # A precondition rather than a variable validation: validations that
    # read another variable need Terraform 1.9.
    precondition {
      condition     = !var.alerts_only || local.state_enabled
      error_message = "alerts_only needs state_bucket: without a state every anomaly is new on every run."
    }
  }
}

resource "aws_iam_role" "scheduler" {
  name = "aws-cost-slack-scheduler"
  assume_role_policy = jsonencode({
    Version = "2012-10-17"
    Statement = [{
      Effect    = "Allow"
      Principal = { Service = "scheduler.amazonaws.com" }
      Action    = "sts:AssumeRole"
    }]
  })
}

resource "aws_iam_role_policy" "scheduler_invoke" {
  name = "invoke-cost-slack"
  role = aws_iam_role.scheduler.id
  policy = jsonencode({
    Version = "2012-10-17"
    Statement = [{
      Effect   = "Allow"
      Action   = "lambda:InvokeFunction"
      Resource = aws_lambda_function.cost_slack.arn
    }]
  })
}

resource "aws_scheduler_schedule" "daily" {
  name = "aws-cost-slack-daily"
  flexible_time_window {
    mode = "OFF"
  }
  schedule_expression = var.schedule
  target {
    arn      = aws_lambda_function.cost_slack.arn
    role_arn = aws_iam_role.scheduler.arn
  }
}
