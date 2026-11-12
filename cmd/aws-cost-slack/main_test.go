package main

import (
	"bytes"
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	s3types "github.com/aws/aws-sdk-go-v2/service/s3/types"

	"github.com/moveeeax/aws-cost-slack/internal/state"
)

// TestAlertsOnlyPostsOnce runs the CLI twice against the baseline fixture
// with a state file: the first run posts the CloudWatch spike, the second
// finds it already open and posts nothing.
func TestAlertsOnlyPostsOnce(t *testing.T) {
	var posts atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		posts.Add(1)
		_, _ = w.Write([]byte("ok"))
	}))
	defer srv.Close()

	statePath := filepath.Join(t.TempDir(), "state.json")
	args := []string{
		"--input", "../../examples/baseline_input.json",
		"--baseline-days", "28",
		"--state", statePath,
		"--alerts-only",
		"--webhook", srv.URL,
	}
	for i := 0; i < 2; i++ {
		if err := run(args); err != nil {
			t.Fatalf("run %d: %v", i+1, err)
		}
	}
	if got := posts.Load(); got != 1 {
		t.Fatalf("want exactly 1 post over two runs, got %d", got)
	}
	b, err := os.ReadFile(statePath)
	if err != nil {
		t.Fatalf("state not written: %v", err)
	}
	if !strings.Contains(string(b), `"SERVICE/Amazon CloudWatch"`) {
		t.Fatalf("state should hold the open CloudWatch alert:\n%s", b)
	}
}

// memS3 holds one object per key in memory; enough for the CLI round trip.
type memS3 struct {
	objects map[string][]byte
	puts    int
}

func (m *memS3) GetObject(_ context.Context, in *s3.GetObjectInput, _ ...func(*s3.Options)) (*s3.GetObjectOutput, error) {
	b, ok := m.objects[*in.Bucket+"/"+*in.Key]
	if !ok {
		return nil, &s3types.NoSuchKey{}
	}
	return &s3.GetObjectOutput{Body: io.NopCloser(bytes.NewReader(b)), ETag: aws.String(`"1"`)}, nil
}

func (m *memS3) PutObject(_ context.Context, in *s3.PutObjectInput, _ ...func(*s3.Options)) (*s3.PutObjectOutput, error) {
	b, err := io.ReadAll(in.Body)
	if err != nil {
		return nil, err
	}
	m.puts++
	m.objects[*in.Bucket+"/"+*in.Key] = b
	return &s3.PutObjectOutput{ETag: aws.String(`"1"`)}, nil
}

// TestAlertsOnlyS3State is TestAlertsOnlyPostsOnce with the state in S3: the
// first run finds no object (empty state) and posts, the second reads the
// open alert back and posts nothing.
func TestAlertsOnlyS3State(t *testing.T) {
	// Keep the SDK away from the developer's AWS config; nothing here calls AWS.
	t.Setenv("AWS_CONFIG_FILE", filepath.Join(t.TempDir(), "none"))
	t.Setenv("AWS_SHARED_CREDENTIALS_FILE", filepath.Join(t.TempDir(), "none"))
	t.Setenv("AWS_REGION", "us-east-1")

	fake := &memS3{objects: map[string][]byte{}}
	orig := newS3
	newS3 = func(aws.Config) state.S3API { return fake }
	defer func() { newS3 = orig }()

	var posts atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		posts.Add(1)
		_, _ = w.Write([]byte("ok"))
	}))
	defer srv.Close()

	args := []string{
		"--input", "../../examples/baseline_input.json",
		"--baseline-days", "28",
		"--state", "s3://cost-state/aws-cost-slack/state.json",
		"--alerts-only",
		"--webhook", srv.URL,
	}
	for i := 0; i < 2; i++ {
		if err := run(args); err != nil {
			t.Fatalf("run %d: %v", i+1, err)
		}
	}
	if got := posts.Load(); got != 1 {
		t.Fatalf("want exactly 1 post over two runs, got %d", got)
	}
	if fake.puts != 2 {
		t.Fatalf("want the state saved on both runs, got %d puts", fake.puts)
	}
	b := fake.objects["cost-state/aws-cost-slack/state.json"]
	if !strings.Contains(string(b), `"SERVICE/Amazon CloudWatch"`) {
		t.Fatalf("state should hold the open CloudWatch alert:\n%s", b)
	}
}

func TestValidateLambda(t *testing.T) {
	bad := []config{
		{StateFile: "/tmp/state.json"},
		{AlertsOnly: true},
		{StateFile: "s3://bucket"},
	}
	for _, c := range bad {
		if err := c.validateLambda(); err == nil {
			t.Errorf("lambda config %+v should be rejected", c)
		}
	}
	if err := (config{StateFile: "s3://bucket/state.json", AlertsOnly: true}).validateLambda(); err != nil {
		t.Errorf("valid lambda config rejected: %v", err)
	}
}

func TestValidate(t *testing.T) {
	bad := []config{
		{Budget: 100, BudgetName: "team"},
		{BudgetName: "team", InputFile: "x.json"},
		{Budget: -1},
		{BaselineDays: 7, Sigma: 0},
		{Backtest: -1},
		{StateFile: "s3://bucket/"},
	}
	for _, c := range bad {
		if err := c.validate(); err == nil {
			t.Errorf("config %+v should be rejected", c)
		}
	}
	if err := (config{BaselineDays: 28, Sigma: 2.5, Budget: 5000}).validate(); err != nil {
		t.Errorf("valid config rejected: %v", err)
	}
}
