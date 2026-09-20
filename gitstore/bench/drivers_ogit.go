package main

import (
	"context"
	"io"

	"github.com/aws/aws-sdk-go-v2/aws"
	awscreds "github.com/aws/aws-sdk-go-v2/credentials"
	awss3 "github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/go-git/go-git/v5/plumbing/storer"
	ogit "github.com/labbs/git-server-s3/pkg/storage/s3"
	"github.com/rs/zerolog"
)

func init() {
	registerStorerDriver("ogit", func() StorerDriver { return &ogitDriver{} })
}

// ogitDriver is github.com/Labbs/ogit's storer.Storer: one S3 object per git
// object and per reference, with no pack tier and no local cache. It is the
// straightforward design, and so the baseline the optimisations are priced
// against. Its reference compare-and-set is not backed by a conditional write,
// so it is measured single-writer only.
type ogitDriver struct {
	env    Env
	client *awss3.Client
}

func (d *ogitDriver) Name() string { return "ogit" }
func (d *ogitDriver) Describe() string {
	return "Labbs/ogit S3 Storer: one S3 object per git object and ref, no packs, no cache"
}

func (d *ogitDriver) Setup(_ context.Context, env Env) error {
	d.env = env
	d.client = awss3.New(awss3.Options{
		BaseEndpoint: aws.String(env.Endpoint),
		UsePathStyle: true,
		Region:       env.Region,
		Credentials:  awscreds.NewStaticCredentialsProvider(env.AccessKey, env.SecretKey, ""),
	})
	return nil
}

func (d *ogitDriver) Open(_ context.Context, repo string, _ bool) (storer.Storer, error) {
	return ogit.NewS3Storer(d.client, d.env.Bucket, d.env.Prefix+"/ogit/"+repo, zerolog.New(io.Discard)), nil
}

func (d *ogitDriver) Maintain(context.Context, storer.Storer) (bool, error) { return false, nil }
func (d *ogitDriver) Close() error                                          { return nil }
