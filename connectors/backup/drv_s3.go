package main

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/credentials"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/aws/aws-sdk-go-v2/service/s3/types"
	smithy "github.com/aws/smithy-go"
)

// s3Objects stores objects at <bucket>/<prefix>/<set>/<name> in any
// S3-compatible store (AWS, RustFS, MinIO, B2, R2, Wasabi, ...) with a
// native client: no aws, rclone or restic binary needed. HTTP honours
// conductor's egress proxy env under the jail.
type s3Objects struct {
	client *s3.Client
	bucket string
	prefix string
	create bool
}

func newS3(endpoint, region, bucket, prefix, key, secret string, pathStyle, create bool) s3Objects {
	opts := s3.Options{
		Region:       strOr(region, "us-east-1"),
		Credentials:  credentials.NewStaticCredentialsProvider(key, secret, ""),
		UsePathStyle: pathStyle,
		HTTPClient:   &http.Client{Timeout: 10 * time.Minute, Transport: &http.Transport{Proxy: http.ProxyFromEnvironment}},
		// Some S3-compatibles reject the newer default checksum trailers.
		RequestChecksumCalculation: aws.RequestChecksumCalculationWhenRequired,
		ResponseChecksumValidation: aws.ResponseChecksumValidationWhenRequired,
	}
	if endpoint != "" {
		opts.BaseEndpoint = aws.String(endpoint)
	}
	return s3Objects{client: s3.New(opts), bucket: bucket, prefix: strings.Trim(prefix, "/"), create: create}
}

func (s s3Objects) key(set, name string) string {
	k := set + "/" + name
	if s.prefix != "" {
		k = s.prefix + "/" + k
	}
	return k
}

func (s s3Objects) write(ctx context.Context, set, name string, data []byte) error {
	put := func() error {
		_, err := s.client.PutObject(ctx, &s3.PutObjectInput{Bucket: &s.bucket, Key: aws.String(s.key(set, name)),
			Body: bytes.NewReader(data), ContentLength: aws.Int64(int64(len(data)))})
		return err
	}
	err := put()
	var nb *types.NoSuchBucket
	if err != nil && s.create && (errors.As(err, &nb) || apiCode(err) == "NoSuchBucket") {
		if _, cerr := s.client.CreateBucket(ctx, &s3.CreateBucketInput{Bucket: &s.bucket}); cerr != nil {
			return fmt.Errorf("s3 create bucket %s: %w", s.bucket, cerr)
		}
		err = put()
	}
	return err
}

func apiCode(err error) string {
	var ae smithy.APIError
	if errors.As(err, &ae) {
		return ae.ErrorCode()
	}
	return ""
}

func (s s3Objects) read(ctx context.Context, set, name string) ([]byte, error) {
	o, err := s.client.GetObject(ctx, &s3.GetObjectInput{Bucket: &s.bucket, Key: aws.String(s.key(set, name))})
	if err != nil {
		return nil, err
	}
	defer o.Body.Close()
	return io.ReadAll(o.Body)
}

func (s s3Objects) names(ctx context.Context, set string) ([]string, error) {
	pre := s.key(set, "")
	var out []string
	p := s3.NewListObjectsV2Paginator(s.client, &s3.ListObjectsV2Input{Bucket: &s.bucket, Prefix: &pre, Delimiter: aws.String("/")})
	for p.HasMorePages() {
		page, err := p.NextPage(ctx)
		if err != nil {
			var nb *types.NoSuchBucket
			if errors.As(err, &nb) || apiCode(err) == "NoSuchBucket" {
				return nil, nil
			}
			return nil, err
		}
		for _, o := range page.Contents {
			out = append(out, strings.TrimPrefix(aws.ToString(o.Key), pre))
		}
	}
	return out, nil
}

func (s s3Objects) del(ctx context.Context, set, name string) error {
	_, err := s.client.DeleteObject(ctx, &s3.DeleteObjectInput{Bucket: &s.bucket, Key: aws.String(s.key(set, name))})
	return err
}

// s3Host is the endpoint's host:port for allow_hosts (AWS default when unset).
func s3Host(endpoint, region, bucket string, pathStyle bool) string {
	if endpoint == "" {
		h := "s3." + strOr(region, "us-east-1") + ".amazonaws.com"
		if !pathStyle && bucket != "" {
			h = bucket + "." + h
		}
		return net.JoinHostPort(h, "443")
	}
	u, err := url.Parse(endpoint)
	if err != nil || u.Host == "" {
		return ""
	}
	port := u.Port()
	if port == "" {
		port = map[bool]string{true: "443", false: "80"}[u.Scheme == "https"]
	}
	host := u.Hostname()
	if !pathStyle && bucket != "" {
		host = bucket + "." + host
	}
	return net.JoinHostPort(host, port)
}
