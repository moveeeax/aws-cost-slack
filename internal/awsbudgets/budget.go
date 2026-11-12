// Package awsbudgets reads the limit of a named AWS Budgets budget, so the
// report can show spend against the budget finance already maintains instead
// of a number copied into a flag.
package awsbudgets

import (
	"context"
	"fmt"
	"strconv"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/budgets"
	btypes "github.com/aws/aws-sdk-go-v2/service/budgets/types"
)

// API is the slice of the Budgets client we use.
type API interface {
	DescribeBudget(ctx context.Context, in *budgets.DescribeBudgetInput, optFns ...func(*budgets.Options)) (*budgets.DescribeBudgetOutput, error)
}

// Limit is a monthly cost budget's limit.
type Limit struct {
	Amount float64
	Unit   string // currency, e.g. USD
}

// MonthlyLimit looks up budget `name` in `accountID` and returns its limit. Only
// MONTHLY COST budgets are accepted: a quarterly limit or a usage budget
// cannot be compared with month-to-date dollars.
func MonthlyLimit(ctx context.Context, api API, accountID, name string) (Limit, error) {
	out, err := api.DescribeBudget(ctx, &budgets.DescribeBudgetInput{
		AccountId:  aws.String(accountID),
		BudgetName: aws.String(name),
	})
	if err != nil {
		return Limit{}, fmt.Errorf("describe budget %q: %w", name, err)
	}
	b := out.Budget
	if b == nil {
		return Limit{}, fmt.Errorf("budget %q: empty response", name)
	}
	if b.BudgetType != btypes.BudgetTypeCost {
		return Limit{}, fmt.Errorf("budget %q is a %s budget; only COST budgets are supported", name, b.BudgetType)
	}
	if b.TimeUnit != btypes.TimeUnitMonthly {
		return Limit{}, fmt.Errorf("budget %q is %s; only MONTHLY budgets are supported", name, b.TimeUnit)
	}
	if b.BudgetLimit == nil || b.BudgetLimit.Amount == nil {
		return Limit{}, fmt.Errorf("budget %q has no fixed limit", name)
	}
	amt, err := strconv.ParseFloat(*b.BudgetLimit.Amount, 64)
	if err != nil {
		return Limit{}, fmt.Errorf("budget %q: parse limit %q: %w", name, *b.BudgetLimit.Amount, err)
	}
	return Limit{Amount: amt, Unit: aws.ToString(b.BudgetLimit.Unit)}, nil
}
