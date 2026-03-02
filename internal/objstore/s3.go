// Copyright (c) 2025 Nishisan. All rights reserved.
// Use of this source code is governed by the N-Backup License (Non-Commercial Evaluation)
// that can be found in the LICENSE file.

package objstore

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"sort"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	awsconfig "github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/credentials"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/aws/aws-sdk-go-v2/service/s3/types"
)

// S3Backend implementa Backend para qualquer endpoint S3-compatible
// (AWS S3, MinIO, Wasabi, DigitalOcean Spaces, etc.).
type S3Backend struct {
	client *s3.Client
	bucket string
	logger *slog.Logger
}

// S3Config contém os parâmetros para criar um S3Backend.
type S3Config struct {
	Endpoint  string // vazio = AWS default
	Region    string
	Bucket    string
	AccessKey string
	SecretKey string
	Logger    *slog.Logger
}

// NewS3Backend cria um S3Backend com credenciais estáticas e endpoint configurável.
func NewS3Backend(ctx context.Context, cfg S3Config) (*S3Backend, error) {
	if cfg.Region == "" {
		cfg.Region = "us-east-1"
	}
	if cfg.Logger == nil {
		cfg.Logger = slog.Default()
	}

	opts := []func(*awsconfig.LoadOptions) error{
		awsconfig.WithRegion(cfg.Region),
		awsconfig.WithCredentialsProvider(
			credentials.NewStaticCredentialsProvider(cfg.AccessKey, cfg.SecretKey, ""),
		),
	}

	awsCfg, err := awsconfig.LoadDefaultConfig(ctx, opts...)
	if err != nil {
		return nil, fmt.Errorf("objstore/s3: loading AWS config: %w", err)
	}

	s3Opts := []func(*s3.Options){}
	if cfg.Endpoint != "" {
		s3Opts = append(s3Opts, func(o *s3.Options) {
			o.BaseEndpoint = aws.String(cfg.Endpoint)
			o.UsePathStyle = true // obrigatório para MinIO e endpoints custom
		})
	}

	client := s3.NewFromConfig(awsCfg, s3Opts...)
	return &S3Backend{
		client: client,
		bucket: cfg.Bucket,
		logger: cfg.Logger,
	}, nil
}

// Upload envia um arquivo local para o bucket S3.
// Usa PutObject simples — suficiente para backups de até ~5GB.
// Para arquivos maiores, uma evolução futura pode usar multipart upload.
func (b *S3Backend) Upload(ctx context.Context, localPath, remotePath string) error {
	f, err := os.Open(localPath)
	if err != nil {
		return fmt.Errorf("objstore/s3: opening file %q: %w", localPath, err)
	}
	defer f.Close()

	info, err := f.Stat()
	if err != nil {
		return fmt.Errorf("objstore/s3: stat file %q: %w", localPath, err)
	}

	b.logger.Info("uploading to S3",
		"bucket", b.bucket,
		"key", remotePath,
		"size", info.Size(),
		"file", filepath.Base(localPath),
	)

	start := time.Now()
	_, err = b.client.PutObject(ctx, &s3.PutObjectInput{
		Bucket: aws.String(b.bucket),
		Key:    aws.String(remotePath),
		Body:   f,
	})
	if err != nil {
		return fmt.Errorf("objstore/s3: PutObject %q: %w", remotePath, err)
	}

	b.logger.Info("upload completed",
		"bucket", b.bucket,
		"key", remotePath,
		"duration", time.Since(start).Round(time.Millisecond),
	)
	return nil
}

// Delete remove um objeto do bucket S3.
func (b *S3Backend) Delete(ctx context.Context, remotePath string) error {
	b.logger.Info("deleting from S3", "bucket", b.bucket, "key", remotePath)

	_, err := b.client.DeleteObject(ctx, &s3.DeleteObjectInput{
		Bucket: aws.String(b.bucket),
		Key:    aws.String(remotePath),
	})
	if err != nil {
		return fmt.Errorf("objstore/s3: DeleteObject %q: %w", remotePath, err)
	}
	return nil
}

// List lista objetos no bucket com o prefixo dado, ordenados por chave.
func (b *S3Backend) List(ctx context.Context, prefix string) ([]ObjectInfo, error) {
	var result []ObjectInfo

	paginator := s3.NewListObjectsV2Paginator(b.client, &s3.ListObjectsV2Input{
		Bucket: aws.String(b.bucket),
		Prefix: aws.String(prefix),
	})

	for paginator.HasMorePages() {
		page, err := paginator.NextPage(ctx)
		if err != nil {
			return nil, fmt.Errorf("objstore/s3: ListObjectsV2 prefix=%q: %w", prefix, err)
		}
		for _, obj := range page.Contents {
			result = append(result, s3ObjectToInfo(obj))
		}
	}

	// Ordena por chave (timestamp no nome → ordem cronológica)
	sort.Slice(result, func(i, j int) bool {
		return result[i].Key < result[j].Key
	})

	return result, nil
}

func s3ObjectToInfo(obj types.Object) ObjectInfo {
	oi := ObjectInfo{}
	if obj.Key != nil {
		oi.Key = *obj.Key
	}
	if obj.Size != nil {
		oi.Size = *obj.Size
	}
	if obj.LastModified != nil {
		oi.LastModified = *obj.LastModified
	}
	return oi
}
