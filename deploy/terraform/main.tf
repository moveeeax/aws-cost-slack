# Scheduled Lambda that runs aws-cost-slack once a day and posts to Slack.
#
# Build the binary for the custom runtime first (from the repo root):
#
#   GOOS=linux GOARCH=arm64 CGO_ENABLED=0 go build -o bootstrap ./cmd/aws-cost-slack
#   zip -j deploy/terraform/function.zip bootstrap
#
# Then:  terraform init && terraform apply -var 'slack_webhook_url=https://hooks.slack.com/...'

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
