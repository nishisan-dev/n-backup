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
	"github.com/aws/aws-sdk-go-v2/feature/s3/manager"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/aws/aws-sdk-go-v2/service/s3/types"
)

// S3Backend implementa Backend para qualquer endpoint S3-compatible
// (AWS S3, MinIO, Wasabi, DigitalOcean Spaces, etc.).
type S3Backend struct {
	client       *s3.Client
	uploader     *manager.Uploader
	bucket       string
	logger       *slog.Logger
	stallTimeout time.Duration // inatividade máxima antes de cancelar upload
}

// S3Config contém os parâmetros para criar um S3Backend.
type S3Config struct {
	Endpoint     string        // vazio = AWS default
	Region       string
	Bucket       string
	AccessKey    string
	SecretKey    string
	Logger       *slog.Logger
	StallTimeout time.Duration // 0 = defaultStallTimeout (5min)
}

// multipartPartSize define o tamanho de cada part no multipart upload (64MB).
// Valor escolhido para balancear throughput e memória em arquivos grandes.
const multipartPartSize = 64 * 1024 * 1024 // 64 MB

// multipartConcurrency define quantas parts são enviadas em paralelo.
const multipartConcurrency = 5

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

	uploader := manager.NewUploader(client, func(u *manager.Uploader) {
		u.PartSize = multipartPartSize
		u.Concurrency = multipartConcurrency
	})

	stallTimeout := cfg.StallTimeout
	if stallTimeout <= 0 {
		stallTimeout = defaultStallTimeout
	}

	return &S3Backend{
		client:       client,
		uploader:     uploader,
		bucket:       cfg.Bucket,
		logger:       cfg.Logger,
		stallTimeout: stallTimeout,
	}, nil
}

// Upload envia um arquivo local para o bucket S3.
// Usa o S3 Manager Uploader que automaticamente faz multipart upload para
// arquivos maiores que PartSize (64MB). Suporta arquivos de qualquer tamanho
// (até o limite S3 de 5TB).
//
// Stall detection: o reader é wrapeado com stallDetectReader que cancela
// o contexto se nenhum byte for lido em stallTimeout. Enquanto bytes
// fluírem, o upload continua indefinidamente — sem deadline global.
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

	fileSize := info.Size()
	b.logger.Info("uploading to S3",
		"bucket", b.bucket,
		"key", remotePath,
		"size", fileSize,
		"size_human", humanSize(fileSize),
		"file", filepath.Base(localPath),
		"multipart", fileSize > multipartPartSize,
		"stall_timeout", b.stallTimeout,
	)

	// Contexto com cancel para stall detection
	uploadCtx, cancel := context.WithCancel(ctx)
	defer cancel()

	// Wrapa o reader com stall detection
	stallReader := newStallDetectReader(f, b.stallTimeout, cancel)
	defer stallReader.Close()

	start := time.Now()
	_, err = b.uploader.Upload(uploadCtx, &s3.PutObjectInput{
		Bucket: aws.String(b.bucket),
		Key:    aws.String(remotePath),
		Body:   stallReader,
	})
	if err != nil {
		return fmt.Errorf("objstore/s3: PutObject %q: %w", remotePath, err)
	}

	duration := time.Since(start)
	avgMBps := float64(fileSize) / (1024 * 1024) / duration.Seconds()
	b.logger.Info("upload completed",
		"bucket", b.bucket,
		"key", remotePath,
		"size", fileSize,
		"size_human", humanSize(fileSize),
		"duration", duration.Round(time.Millisecond),
		"avg_mbps", fmt.Sprintf("%.2f", avgMBps),
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

// AbortIncompleteUploads cancela multipart uploads pendentes cujas chaves
// correspondem ao prefixo informado. Usado para limpar lixo após falha
// definitiva de upload (parts órfãs que geram "pastas fantasmas" no bucket).
func (b *S3Backend) AbortIncompleteUploads(ctx context.Context, prefix string) error {
	paginator := s3.NewListMultipartUploadsPaginator(b.client, &s3.ListMultipartUploadsInput{
		Bucket: aws.String(b.bucket),
		Prefix: aws.String(prefix),
	})

	var aborted int
	for paginator.HasMorePages() {
		page, err := paginator.NextPage(ctx)
		if err != nil {
			return fmt.Errorf("objstore/s3: ListMultipartUploads prefix=%q: %w", prefix, err)
		}

		for _, upload := range page.Uploads {
			_, abortErr := b.client.AbortMultipartUpload(ctx, &s3.AbortMultipartUploadInput{
				Bucket:   aws.String(b.bucket),
				Key:      upload.Key,
				UploadId: upload.UploadId,
			})
			if abortErr != nil {
				b.logger.Warn("failed to abort multipart upload",
					"key", aws.ToString(upload.Key),
					"upload_id", aws.ToString(upload.UploadId),
					"error", abortErr,
				)
			} else {
				aborted++
			}
		}
	}

	if aborted > 0 {
		b.logger.Info("aborted incomplete multipart uploads",
			"bucket", b.bucket,
			"prefix", prefix,
			"count", aborted,
		)
	}

	return nil
}

// humanSize formata bytes em representação legível (KB, MB, GB, TB).
func humanSize(bytes int64) string {
	const (
		kb = 1024
		mb = 1024 * kb
		gb = 1024 * mb
		tb = 1024 * gb
	)
	switch {
	case bytes >= tb:
		return fmt.Sprintf("%.2f TB", float64(bytes)/float64(tb))
	case bytes >= gb:
		return fmt.Sprintf("%.2f GB", float64(bytes)/float64(gb))
	case bytes >= mb:
		return fmt.Sprintf("%.2f MB", float64(bytes)/float64(mb))
	case bytes >= kb:
		return fmt.Sprintf("%.2f KB", float64(bytes)/float64(kb))
	default:
		return fmt.Sprintf("%d B", bytes)
	}
}
