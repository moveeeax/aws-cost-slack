package state

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"strings"
	"testing"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	s3types "github.com/aws/aws-sdk-go-v2/service/s3/types"
	"github.com/aws/smithy-go"

	"github.com/moveeeax/aws-cost-slack/internal/costs"
)

// fakeS3 is an in-memory bucket that honors If-Match / If-None-Match the way
// S3 does (412 PreconditionFailed on a mismatch).
type fakeS3 struct {
	objects map[string][]byte
	etags   map[string]string
	n       int
	getErr  error
	puts    []*s3.PutObjectInput
}

func newFakeS3() *fakeS3 {
	return &fakeS3{objects: map[string][]byte{}, etags: map[string]string{}}
}

func (f *fakeS3) GetObject(_ context.Context, in *s3.GetObjectInput, _ ...func(*s3.Options)) (*s3.GetObjectOutput, error) {
	if f.getErr != nil {
		return nil, f.getErr
	}
	k := *in.Bucket + "/" + *in.Key
	b, ok := f.objects[k]
	if !ok {
		return nil, &s3types.NoSuchKey{Message: aws.String("The specified key does not exist.")}
	}
	return &s3.GetObjectOutput{Body: io.NopCloser(bytes.NewReader(b)), ETag: aws.String(f.etags[k])}, nil
}

func (f *fakeS3) PutObject(_ context.Context, in *s3.PutObjectInput, _ ...func(*s3.Options)) (*s3.PutObjectOutput, error) {
	f.puts = append(f.puts, in)
	k := *in.Bucket + "/" + *in.Key
	cur, exists := f.etags[k]
	if in.IfNoneMatch != nil && exists {
		return nil, &smithy.GenericAPIError{Code: "PreconditionFailed", Message: "At least one of the pre-conditions you specified did not hold"}
	}
	if in.IfMatch != nil && (!exists || *in.IfMatch != cur) {
		return nil, &smithy.GenericAPIError{Code: "PreconditionFailed", Message: "At least one of the pre-conditions you specified did not hold"}
	}
	b, err := io.ReadAll(in.Body)
	if err != nil {
		return nil, err
	}
	f.n++
	f.objects[k] = b
	f.etags[k] = fmt.Sprintf(`"etag-%d"`, f.n)
	return &s3.PutObjectOutput{ETag: aws.String(f.etags[k])}, nil
}

func TestParseS3URI(t *testing.T) {
	b, k, err := ParseS3URI("s3://my-bucket/aws-cost-slack/state.json")
	if err != nil || b != "my-bucket" || k != "aws-cost-slack/state.json" {
		t.Fatalf("got %q %q %v", b, k, err)
	}
	for _, bad := range []string{"s3://", "s3://bucket", "s3://bucket/", "s3:///key", "s3://bucket/prefix/", "./state.json"} {
		if _, _, err := ParseS3URI(bad); err == nil {
			t.Errorf("%q should be rejected", bad)
		}
	}
}

func TestOpen(t *testing.T) {
	st, err := Open("./state.json", nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := st.(*File); !ok {
		t.Fatalf("local path should open a File store, got %T", st)
	}
	st, err = Open("s3://b/k.json", newFakeS3())
	if err != nil {
		t.Fatal(err)
	}
	if s, ok := st.(*S3); !ok || s.Bucket != "b" || s.Key != "k.json" {
		t.Fatalf("got %#v", st)
	}
	if _, err := Open("s3://b/k.json", nil); err == nil {
		t.Fatal("s3 location without a client should fail")
	}
}

func TestS3MissingIsEmptyThenCreate(t *testing.T) {
	ctx := context.Background()
	api := newFakeS3()
	s := &S3{API: api, Bucket: "b", Key: "state.json"}
	st, err := s.Load(ctx)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if st.Open == nil || len(st.Open) != 0 {
		t.Fatalf("want empty open map, got %+v", st)
	}
	st.Open["SERVICE/Amazon S3"] = costs.OpenAlert{GroupBy: "SERVICE", Name: "Amazon S3", Since: "2026-07-21", Amount: 48}
	if err := s.Save(ctx, st); err != nil {
		t.Fatalf("save: %v", err)
	}
	put := api.puts[0]
	if aws.ToString(put.IfNoneMatch) != "*" || put.IfMatch != nil {
		t.Fatalf("first write should be If-None-Match: *, got %+v", put)
	}
	if aws.ToString(put.ContentType) != "application/json" {
		t.Fatalf("content type %q", aws.ToString(put.ContentType))
	}
}

func TestS3RoundTripUsesETag(t *testing.T) {
	ctx := context.Background()
	api := newFakeS3()
	api.objects["b/state.json"] = []byte(`{"open":{"SERVICE/Amazon CloudWatch":{"group_by":"SERVICE","name":"Amazon CloudWatch","since":"2026-08-02","amount":30}}}`)
	api.etags["b/state.json"] = `"seed"`

	s := &S3{API: api, Bucket: "b", Key: "state.json"}
	st, err := s.Load(ctx)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if got := st.Open["SERVICE/Amazon CloudWatch"]; got.Since != "2026-08-02" || got.Amount != 30 {
		t.Fatalf("lost data: %+v", st)
	}
	delete(st.Open, "SERVICE/Amazon CloudWatch")
	if err := s.Save(ctx, st); err != nil {
		t.Fatalf("save: %v", err)
	}
	if got := aws.ToString(api.puts[0].IfMatch); got != `"seed"` {
		t.Fatalf("want If-Match with the loaded ETag, got %q", got)
	}
	// A second save in the same run uses the ETag returned by the first.
	if err := s.Save(ctx, st); err != nil {
		t.Fatalf("second save: %v", err)
	}

	again, err := (&S3{API: api, Bucket: "b", Key: "state.json"}).Load(ctx)
	if err != nil {
		t.Fatalf("reload: %v", err)
	}
	if len(again.Open) != 0 {
		t.Fatalf("want resolved alert dropped, got %+v", again)
	}
}

func TestS3SaveConflict(t *testing.T) {
	ctx := context.Background()
	api := newFakeS3()
	a := &S3{API: api, Bucket: "b", Key: "state.json"}
	b := &S3{API: api, Bucket: "b", Key: "state.json"}
	stA, _ := a.Load(ctx)
	stB, _ := b.Load(ctx)
	if err := a.Save(ctx, stA); err != nil {
		t.Fatalf("first writer: %v", err)
	}
	err := b.Save(ctx, stB)
	if err == nil || !strings.Contains(err.Error(), "changed since it was read") {
		t.Fatalf("second writer should get a conflict, got %v", err)
	}
}

func TestS3LoadErrors(t *testing.T) {
	ctx := context.Background()
	api := newFakeS3()
	api.getErr = &smithy.GenericAPIError{Code: "AccessDenied", Message: "Access Denied"}
	if _, err := (&S3{API: api, Bucket: "b", Key: "k"}).Load(ctx); err == nil {
		t.Fatal("AccessDenied must not read as an empty state")
	}

	api = newFakeS3()
	api.objects["b/k"] = []byte("{not json")
	if _, err := (&S3{API: api, Bucket: "b", Key: "k"}).Load(ctx); err == nil {
		t.Fatal("want parse error for a corrupt state object")
	}

	if err := (&S3{API: newFakeS3(), Bucket: "b", Key: "k"}).Save(ctx, costs.AlertState{}); err == nil {
		t.Fatal("Save before Load should fail")
	}
}
