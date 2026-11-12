// Command aws-cost-slack posts a daily AWS cost breakdown to Slack.
//
// It reads yesterday's spend from Cost Explorer (grouped by service or linked
// account), diffs it against the day before and month-to-date, projects the
// month end, flags spend anomalies (day-over-day or against a rolling
// baseline), and posts a Slack Block Kit message.
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
	"github.com/aws/aws-sdk-go-v2/aws"
	awsconfig "github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/service/budgets"
	ce "github.com/aws/aws-sdk-go-v2/service/costexplorer"
	"github.com/aws/aws-sdk-go-v2/service/sts"

	"github.com/moveeeax/aws-cost-slack/internal/awsbudgets"
	"github.com/moveeeax/aws-cost-slack/internal/awsce"
	"github.com/moveeeax/aws-cost-slack/internal/costs"
	"github.com/moveeeax/aws-cost-slack/internal/slack"
	"github.com/moveeeax/aws-cost-slack/internal/state"
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

	Forecast   bool
	Budget     float64
	BudgetName string

	BaselineDays int
	Sigma        float64
	MinDelta     float64
	StateFile    string
	AlertsOnly   bool
	Backtest     int
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
	fs.BoolVar(&cfg.Forecast, "forecast", true, "show the month-end forecast (ce:GetCostForecast) and compare with last month")
	fs.Float64Var(&cfg.Budget, "budget", 0, "monthly budget amount in the report currency; shows progress and warns at 80/100%")
	fs.StringVar(&cfg.BudgetName, "budget-name", "", "read the monthly limit from this AWS Budgets budget instead of --budget")
	fs.IntVar(&cfg.BaselineDays, "baseline-days", 0, "score anomalies against a rolling baseline of this many days (0 = day-over-day)")
	fs.Float64Var(&cfg.Sigma, "sigma", 2.5, "baseline mode: standard deviations above the baseline that flag an anomaly")
	fs.Float64Var(&cfg.MinDelta, "min-delta", 1, "absolute increase (currency units) an anomaly must also clear")
	fs.StringVar(&cfg.StateFile, "state", "", "alert state file; anomalies already alerted are marked ongoing and resolutions are reported")
	fs.BoolVar(&cfg.AlertsOnly, "alerts-only", false, "post only when there is a new or resolved anomaly")
	fs.IntVar(&cfg.Backtest, "backtest", 0, "print the alerts that would have fired over this many past days, then exit")
	fs.Usage = func() {
		fmt.Fprintf(os.Stderr, "usage: aws-cost-slack --webhook URL [--group-by SERVICE|LINKED_ACCOUNT] [flags]\n\n")
		fs.PrintDefaults()
	}
	if err := fs.Parse(args); err != nil {
		return err
	}
	if err := cfg.validate(); err != nil {
		return err
	}

	ctx := context.Background()
	if cfg.Backtest > 0 {
		return backtest(ctx, cfg)
	}

	report, st, err := build(ctx, cfg)
	if err != nil {
		return err
	}
	msg := slack.Render(report)

	if cfg.DryRun || (cfg.Webhook == "" && cfg.InputFile != "") {
		enc := json.NewEncoder(os.Stdout)
		enc.SetIndent("", "  ")
		return enc.Encode(msg)
	}
	if cfg.Webhook == "" {
		return fmt.Errorf("no --webhook and no SLACK_WEBHOOK_URL set")
	}
	if cfg.AlertsOnly && !report.HasAlerts() {
		fmt.Printf("no new or resolved anomalies for %s; nothing posted\n", report.Date)
		return saveState(cfg, st)
	}
	if err := slack.Post(ctx, nil, cfg.Webhook, msg); err != nil {
		return err
	}
	fmt.Printf("posted %s report for %s: total %s (%s DoD)\n",
		cfg.GroupBy, report.Date, costs.Money(report.Currency, report.Total), costs.SignedPct(report.TotalDoDPct))
	return saveState(cfg, st)
}

func (c config) validate() error {
	if c.Budget < 0 {
		return fmt.Errorf("--budget must not be negative")
	}
	if c.Budget > 0 && c.BudgetName != "" {
		return fmt.Errorf("use either --budget or --budget-name, not both")
	}
	if c.BudgetName != "" && c.InputFile != "" {
		return fmt.Errorf("--budget-name reads AWS Budgets; with --input use --budget")
	}
	if c.BaselineDays < 0 || c.Backtest < 0 {
		return fmt.Errorf("--baseline-days and --backtest must not be negative")
	}
	if c.BaselineDays > 0 && c.Sigma <= 0 {
		return fmt.Errorf("--sigma must be positive")
	}
	return nil
}

func options(cfg config) costs.Options {
	return costs.Options{
		TopN:         cfg.Top,
		ThresholdPct: cfg.Threshold,
		MinDelta:     cfg.MinDelta,
		BaselineDays: cfg.BaselineDays,
		Sigma:        cfg.Sigma,
		Budget:       cfg.Budget,
		BudgetName:   cfg.BudgetName,
	}
}

func saveState(cfg config, st costs.AlertState) error {
	if cfg.StateFile == "" {
		return nil
	}
	return state.Save(cfg.StateFile, st)
}

// lambdaHandler configures the run from environment variables and posts to
// Slack. Any EventBridge Scheduler payload is ignored — the schedule is the
// trigger, the env is the config.
func lambdaHandler(ctx context.Context, _ json.RawMessage) error {
	cfg := config{
		Webhook:      os.Getenv("SLACK_WEBHOOK_URL"),
		GroupBy:      envOr("GROUP_BY", "SERVICE"),
		Metric:       envOr("METRIC", "UnblendedCost"),
		Region:       os.Getenv("AWS_REGION"),
		Top:          envInt("TOP", 8),
		Threshold:    envFloat("THRESHOLD", 50),
		Forecast:     envBool("FORECAST", true),
		Budget:       envFloat("BUDGET", 0),
		BudgetName:   os.Getenv("BUDGET_NAME"),
		BaselineDays: envInt("BASELINE_DAYS", 0),
		Sigma:        envFloat("SIGMA", 2.5),
		MinDelta:     envFloat("MIN_DELTA", 1),
	}
	if cfg.Webhook == "" {
		return fmt.Errorf("SLACK_WEBHOOK_URL is required")
	}
	if err := cfg.validate(); err != nil {
		return err
	}
	report, _, err := build(ctx, cfg)
	if err != nil {
		return err
	}
	return slack.Post(ctx, nil, cfg.Webhook, slack.Render(report))
}

// build resolves cost input (fixture or live AWS) and the budget, assembles
// the report, and reconciles it with the alert state when one is configured.
// It returns the state to save after a successful post.
func build(ctx context.Context, cfg config) (costs.Report, costs.AlertState, error) {
	var awsCfg aws.Config
	if cfg.InputFile == "" {
		var err error
		if awsCfg, err = loadAWSConfig(ctx, cfg); err != nil {
			return costs.Report{}, costs.AlertState{}, err
		}
	}
	in, err := loadInput(ctx, cfg, awsCfg)
	if err != nil {
		return costs.Report{}, costs.AlertState{}, err
	}
	in.GroupBy = cfg.GroupBy

	opts := options(cfg)
	if cfg.BudgetName != "" {
		lim, err := budgetLimit(ctx, awsCfg, cfg.BudgetName)
		if err != nil {
			return costs.Report{}, costs.AlertState{}, err
		}
		if lim.Unit != "" && lim.Unit != in.Currency {
			return costs.Report{}, costs.AlertState{}, fmt.Errorf("budget %q is in %s but costs are in %s", cfg.BudgetName, lim.Unit, in.Currency)
		}
		opts.Budget = lim.Amount
	}

	report := costs.Build(in, opts)
	var st costs.AlertState
	if cfg.StateFile != "" {
		prev, err := state.Load(cfg.StateFile)
		if err != nil {
			return costs.Report{}, costs.AlertState{}, err
		}
		st = report.Reconcile(prev)
	}
	return report, st, nil
}

func loadAWSConfig(ctx context.Context, cfg config) (aws.Config, error) {
	opts := []func(*awsconfig.LoadOptions) error{}
	if cfg.Region != "" {
		opts = append(opts, awsconfig.WithRegion(cfg.Region))
	}
	awsCfg, err := awsconfig.LoadDefaultConfig(ctx, opts...)
	if err != nil {
		return aws.Config{}, fmt.Errorf("load AWS config: %w", err)
	}
	return awsCfg, nil
}

func readInputFile(path string) (costs.Input, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return costs.Input{}, fmt.Errorf("read input file: %w", err)
	}
	var in costs.Input
	if err := json.Unmarshal(b, &in); err != nil {
		return costs.Input{}, fmt.Errorf("parse input file: %w", err)
	}
	return in, nil
}

func fetcher(cfg config, awsCfg aws.Config) *awsce.Fetcher {
	f := awsce.New(ce.NewFromConfig(awsCfg), cfg.GroupBy, cfg.Metric)
	f.Forecast = cfg.Forecast
	f.BaselineDays = cfg.BaselineDays
	f.Warnf = func(format string, args ...any) {
		fmt.Fprintf(os.Stderr, "aws-cost-slack: warning: "+format+"\n", args...)
	}
	return f
}

func loadInput(ctx context.Context, cfg config, awsCfg aws.Config) (costs.Input, error) {
	if cfg.InputFile != "" {
		in, err := readInputFile(cfg.InputFile)
		if err != nil {
			return costs.Input{}, err
		}
		if !cfg.Forecast {
			in.Forecast, in.LastMonth = nil, nil
		}
		return in, nil
	}
	return fetcher(cfg, awsCfg).Fetch(ctx, time.Now().UTC())
}

// budgetLimit resolves the caller's account id and reads the named budget.
func budgetLimit(ctx context.Context, awsCfg aws.Config, name string) (awsbudgets.Limit, error) {
	id, err := sts.NewFromConfig(awsCfg).GetCallerIdentity(ctx, &sts.GetCallerIdentityInput{})
	if err != nil {
		return awsbudgets.Limit{}, fmt.Errorf("resolve account id for budget: %w", err)
	}
	return awsbudgets.MonthlyLimit(ctx, budgets.NewFromConfig(awsCfg), aws.ToString(id.Account), name)
}

// backtest replays anomaly detection over the last cfg.Backtest days and
// prints what would have fired and resolved. Nothing is posted.
func backtest(ctx context.Context, cfg config) error {
	var days []costs.Period
	currency := "USD"
	if cfg.InputFile != "" {
		in, err := readInputFile(cfg.InputFile)
		if err != nil {
			return err
		}
		days = append(append(days, in.History...), in.Yesterday)
		if in.Currency != "" {
			currency = in.Currency
		}
	} else {
		awsCfg, err := loadAWSConfig(ctx, cfg)
		if err != nil {
			return err
		}
		now := time.Now().UTC()
		end := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, time.UTC)
		lookback := cfg.Backtest + max(cfg.BaselineDays, 1)
		var cur string
		days, cur, err = fetcher(cfg, awsCfg).Daily(ctx, end.AddDate(0, 0, -lookback), end)
		if err != nil {
			return fmt.Errorf("backtest history: %w", err)
		}
		if cur != "" {
			currency = cur
		}
	}
	// Keep just enough lead-in days to score the first replayed day.
	if keep := cfg.Backtest + max(cfg.BaselineDays, 1); len(days) > keep {
		days = days[len(days)-keep:]
	}

	events := costs.Backtest(cfg.GroupBy, currency, days, options(cfg))
	fired, resolved := 0, 0
	for _, e := range events {
		fmt.Println(formatEvent(currency, cfg.BaselineDays > 0, e))
		if e.Kind == costs.EventFire {
			fired++
		} else {
			resolved++
		}
	}
	replayed := len(days) - max(cfg.BaselineDays, 1)
	if replayed < 0 {
		replayed = 0
	}
	fmt.Printf("%d fired, %d resolved over %d replayed days\n", fired, resolved, replayed)
	return nil
}

func formatEvent(cur string, baselineMode bool, e costs.Event) string {
	r := e.Row
	if e.Kind == costs.EventResolve {
		return fmt.Sprintf("%s  resolve  %s  back to %s, alerted since %s", e.Date, r.Name, costs.Money(cur, r.Amount), e.Since)
	}
	if baselineMode {
		return fmt.Sprintf("%s  fire     %s  %s vs baseline %s (%.1fσ)", e.Date, r.Name,
			costs.Money(cur, r.Amount), costs.Money(cur, r.Baseline), r.Score)
	}
	return fmt.Sprintf("%s  fire     %s  %s -> %s (%s)", e.Date, r.Name,
		costs.Money(cur, r.PrevAmount), costs.Money(cur, r.Amount), costs.SignedPct(r.DoDPct))
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

func envBool(key string, def bool) bool {
	if v := os.Getenv(key); v != "" {
		if b, err := strconv.ParseBool(v); err == nil {
			return b
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
