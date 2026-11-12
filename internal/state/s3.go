package state

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	s3types "github.com/aws/aws-sdk-go-v2/service/s3/types"
	"github.com/aws/smithy-go"

	"github.com/moveeeax/aws-cost-slack/internal/costs"
)

// S3API is the slice of the S3 client we use.
type S3API interface {
	GetObject(ctx context.Context, in *s3.GetObjectInput, optFns ...func(*s3.Options)) (*s3.GetObjectOutput, error)
	PutObject(ctx context.Context, in *s3.PutObjectInput, optFns ...func(*s3.Options)) (*s3.PutObjectOutput, error)
}

// S3 is a state file kept as one S3 object, for the Lambda deployment where
// there is no disk that outlives an invocation.
//
// Save is a conditional write: If-Match with the ETag seen by Load, or
// If-None-Match: * when Load found no object. Two overlapping runs therefore
// cannot silently overwrite each other's state; the second Save fails.
type S3 struct {
	API    S3API
	Bucket string
	Key    string

	loaded bool
	etag   string // empty after Load found no object
}

func (s *S3) String() string { return "s3://" + s.Bucket + "/" + s.Key }

// Load reads the object. A missing object is an empty state (first run).
//
// S3 reports a missing object as 404 only when the caller may also
// s3:ListBucket; with GetObject alone it is 403 AccessDenied, which is
// returned as an error rather than guessed to mean "missing".
func (s *S3) Load(ctx context.Context) (costs.AlertState, error) {
	out, err := s.API.GetObject(ctx, &s3.GetObjectInput{
		Bucket: aws.String(s.Bucket),
		Key:    aws.String(s.Key),
	})
	if isNotFound(err) {
		s.loaded, s.etag = true, ""
		return costs.AlertState{Open: map[string]costs.OpenAlert{}}, nil
	}
	if err != nil {
		return costs.AlertState{}, fmt.Errorf("read state %s: %w", s, err)
	}
	defer out.Body.Close()
	b, err := io.ReadAll(out.Body)
	if err != nil {
		return costs.AlertState{}, fmt.Errorf("read state %s: %w", s, err)
	}
	var st costs.AlertState
	if err := json.Unmarshal(b, &st); err != nil {
		return costs.AlertState{}, fmt.Errorf("parse state %s: %w", s, err)
	}
	if st.Open == nil {
		st.Open = map[string]costs.OpenAlert{}
	}
	s.loaded, s.etag = true, aws.ToString(out.ETag)
	return st, nil
}

// Save writes the object, conditional on it being unchanged since Load.
func (s *S3) Save(ctx context.Context, st costs.AlertState) error {
	if !s.loaded {
		return fmt.Errorf("write state %s: Save called before Load", s)
	}
	b, err := json.MarshalIndent(st, "", "  ")
	if err != nil {
		return fmt.Errorf("marshal state: %w", err)
	}
	in := &s3.PutObjectInput{
		Bucket:      aws.String(s.Bucket),
		Key:         aws.String(s.Key),
		Body:        bytes.NewReader(append(b, '\n')),
		ContentType: aws.String("application/json"),
	}
	if s.etag != "" {
		in.IfMatch = aws.String(s.etag)
	} else {
		in.IfNoneMatch = aws.String("*")
	}
	out, err := s.API.PutObject(ctx, in)
	if isConflict(err) {
		return fmt.Errorf("write state %s: object changed since it was read (another run?): %w", s, err)
	}
	if err != nil {
		return fmt.Errorf("write state %s: %w", s, err)
	}
	s.etag = aws.ToString(out.ETag)
	return nil
}

func isNotFound(err error) bool {
	if err == nil {
		return false
	}
	var nsk *s3types.NoSuchKey
	var nf *s3types.NotFound
	return errors.As(err, &nsk) || errors.As(err, &nf)
}

// isConflict matches a failed conditional write: 412 PreconditionFailed, or
// 409 ConditionalRequestConflict when another write raced this one.
func isConflict(err error) bool {
	var ae smithy.APIError
	if !errors.As(err, &ae) {
		return false
	}
	switch ae.ErrorCode() {
	case "PreconditionFailed", "ConditionalRequestConflict":
		return true
	}
	return false
}
