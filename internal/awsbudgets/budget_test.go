package awsbudgets

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/budgets"
	btypes "github.com/aws/aws-sdk-go-v2/service/budgets/types"
)

type fakeAPI struct {
	budget *btypes.Budget
	err    error
	in     *budgets.DescribeBudgetInput
}

func (f *fakeAPI) DescribeBudget(_ context.Context, in *budgets.DescribeBudgetInput, _ ...func(*budgets.Options)) (*budgets.DescribeBudgetOutput, error) {
	f.in = in
	if f.err != nil {
		return nil, f.err
	}
	return &budgets.DescribeBudgetOutput{Budget: f.budget}, nil
}

func monthly(amount string) *btypes.Budget {
	return &btypes.Budget{
		BudgetName:  aws.String("team"),
		BudgetType:  btypes.BudgetTypeCost,
		TimeUnit:    btypes.TimeUnitMonthly,
		BudgetLimit: &btypes.Spend{Amount: aws.String(amount), Unit: aws.String("USD")},
	}
}

func TestMonthlyLimit(t *testing.T) {
	api := &fakeAPI{budget: monthly("5000.0")}
	lim, err := MonthlyLimit(context.Background(), api, "123456789012", "team")
	if err != nil {
		t.Fatalf("limit: %v", err)
	}
	if lim.Amount != 5000 || lim.Unit != "USD" {
		t.Fatalf("limit = %+v", lim)
	}
	if *api.in.AccountId != "123456789012" || *api.in.BudgetName != "team" {
		t.Errorf("request = %+v", api.in)
	}
}

func TestMonthlyLimitRejects(t *testing.T) {
	quarterly := monthly("100")
	quarterly.TimeUnit = btypes.TimeUnitQuarterly
	usage := monthly("100")
	usage.BudgetType = btypes.BudgetTypeUsage
	noLimit := monthly("100")
	noLimit.BudgetLimit = nil

	cases := map[string]*fakeAPI{
		"QUARTERLY":      {budget: quarterly},
		"USAGE":          {budget: usage},
		"no fixed limit": {budget: noLimit},
		"NotFound":       {err: errors.New("NotFoundException")},
	}
	for want, api := range cases {
		_, err := MonthlyLimit(context.Background(), api, "1", "team")
		if err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("want error containing %q, got %v", want, err)
		}
	}
}
