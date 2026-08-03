// Command aws-cost-slack posts a daily AWS cost breakdown to Slack.
//
// It reads yesterday's spend from Cost Explorer (grouped by service or linked
// account), diffs it against the day before and month-to-date, flags spend
// anomalies, and posts a Slack Block Kit message.
//
// The same binary runs two ways:
//   - as a CLI (flags), for local runs and cron;
//   - as an AWS Lambda (when AWS_LAMBDA_RUNTIME_API is set), configured by env
//     vars and triggered on a schedule by EventBridge Scheduler.
package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"strconv"
	"time"

	"github.com/aws/aws-lambda-go/lambda"
	awsconfig "github.com/aws/aws-sdk-go-v2/config"
	ce "github.com/aws/aws-sdk-go-v2/service/costexplorer"

	"github.com/moveeeax/aws-cost-slack/internal/awsce"
	"github.com/moveeeax/aws-cost-slack/internal/costs"
	"github.com/moveeeax/aws-cost-slack/internal/slack"
)

// config is the resolved runtime configuration shared by the CLI and Lambda
// entry points.
type config struct {
	Webhook   string
	GroupBy   string
	Metric    string
	Region    string
	Top       int
	Threshold float64
	DryRun    bool
	InputFile string
}

func main() {
	if os.Getenv("AWS_LAMBDA_RUNTIME_API") != "" {
		lambda.Start(lambdaHandler)
		return
	}
	if err := run(os.Args[1:]); err != nil {
		fmt.Fprintln(os.Stderr, "aws-cost-slack:", err)
		os.Exit(1)
	}
}

func run(args []string) error {
	fs := flag.NewFlagSet("aws-cost-slack", flag.ContinueOnError)
	cfg := config{}
	fs.StringVar(&cfg.Webhook, "webhook", os.Getenv("SLACK_WEBHOOK_URL"), "Slack incoming-webhook URL (or SLACK_WEBHOOK_URL)")
	fs.StringVar(&cfg.GroupBy, "group-by", "SERVICE", "Cost Explorer dimension: SERVICE or LINKED_ACCOUNT")
	fs.StringVar(&cfg.Metric, "metric", "UnblendedCost", "cost metric: UnblendedCost, AmortizedCost, ...")
	fs.StringVar(&cfg.Region, "region", "", "AWS region for the Cost Explorer endpoint (default: SDK chain)")
	fs.IntVar(&cfg.Top, "top", 8, "how many top groups to list")
	fs.Float64Var(&cfg.Threshold, "threshold", 50, "day-over-day % increase that flags an anomaly")
	fs.BoolVar(&cfg.DryRun, "dry-run", false, "print the Slack payload instead of posting")
	fs.StringVar(&cfg.InputFile, "input", "", "read a normalized JSON cost input from a file instead of AWS (testing/demo)")
	fs.Usage = func() {
		fmt.Fprintf(os.Stderr, "usage: aws-cost-slack --webhook URL [--group-by SERVICE|LINKED_ACCOUNT] [flags]\n\n")
		fs.PrintDefaults()
	}
	if err := fs.Parse(args); err != nil {
		return err
	}

	ctx := context.Background()
	report, msg, err := build(ctx, cfg)
	if err != nil {
		return err
	}

	if cfg.DryRun || (cfg.Webhook == "" && cfg.InputFile != "") {
		enc := json.NewEncoder(os.Stdout)
		enc.SetIndent("", "  ")
		return enc.Encode(msg)
	}
	if cfg.Webhook == "" {
		return fmt.Errorf("no --webhook and no SLACK_WEBHOOK_URL set")
	}
	if err := slack.Post(ctx, nil, cfg.Webhook, msg); err != nil {
		return err
	}
	fmt.Printf("posted %s report for %s: total %s (%s DoD)\n",
		cfg.GroupBy, report.Date, costs.Money(report.Currency, report.Total), costs.SignedPct(report.TotalDoDPct))
	return nil
}

// lambdaHandler configures the run from environment variables and posts to
// Slack. Any EventBridge Scheduler payload is ignored — the schedule is the
// trigger, the env is the config.
func lambdaHandler(ctx context.Context, _ json.RawMessage) error {
	cfg := config{
		Webhook:   os.Getenv("SLACK_WEBHOOK_URL"),
		GroupBy:   envOr("GROUP_BY", "SERVICE"),
		Metric:    envOr("METRIC", "UnblendedCost"),
		Region:    os.Getenv("AWS_REGION"),
		Top:       envInt("TOP", 8),
		Threshold: envFloat("THRESHOLD", 50),
	}
	if cfg.Webhook == "" {
		return fmt.Errorf("SLACK_WEBHOOK_URL is required")
	}
	_, msg, err := build(ctx, cfg)
	if err != nil {
		return err
	}
	return slack.Post(ctx, nil, cfg.Webhook, msg)
}

// build resolves cost input (fixture or live AWS), assembles the report, and
// renders the Slack message.
func build(ctx context.Context, cfg config) (costs.Report, slack.Message, error) {
	in, err := loadInput(ctx, cfg)
	if err != nil {
		return costs.Report{}, slack.Message{}, err
	}
	in.GroupBy = cfg.GroupBy
	report := costs.BuildReport(in, cfg.Top, cfg.Threshold)
	return report, slack.Render(report), nil
}

func loadInput(ctx context.Context, cfg config) (costs.Input, error) {
	if cfg.InputFile != "" {
		b, err := os.ReadFile(cfg.InputFile)
		if err != nil {
			return costs.Input{}, fmt.Errorf("read input file: %w", err)
		}
		var in costs.Input
		if err := json.Unmarshal(b, &in); err != nil {
			return costs.Input{}, fmt.Errorf("parse input file: %w", err)
		}
		return in, nil
	}

	opts := []func(*awsconfig.LoadOptions) error{}
	if cfg.Region != "" {
		opts = append(opts, awsconfig.WithRegion(cfg.Region))
	}
	awsCfg, err := awsconfig.LoadDefaultConfig(ctx, opts...)
	if err != nil {
		return costs.Input{}, fmt.Errorf("load AWS config: %w", err)
	}
	fetcher := awsce.New(ce.NewFromConfig(awsCfg), cfg.GroupBy, cfg.Metric)
	return fetcher.Fetch(ctx, time.Now().UTC())
}

func envOr(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

func envInt(key string, def int) int {
	if v := os.Getenv(key); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			return n
		}
	}
	return def
}

func envFloat(key string, def float64) float64 {
	if v := os.Getenv(key); v != "" {
		if f, err := strconv.ParseFloat(v, 64); err == nil {
			return f
		}
	}
	return def
}
