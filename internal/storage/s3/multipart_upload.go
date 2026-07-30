package s3

// multipart_upload.go — helpers extracted from putObjectMultipart in backend.go.
//
// The multipart upload lifecycle consists of four steps:
//
//  1. initiateMultipartUpload  – CreateMultipartUpload → uploadID
//  2. uploadParts              – concurrent UploadPart fan-out
//  3. abortMultipartUpload     – AbortMultipartUpload on failure (cleanup)
//  4. completeMultipartUpload  – CompleteMultipartUpload on success
//
// partSlice and partResult are pure helpers used by uploadParts.

import (
	"bytes"
	"context"
	"fmt"
	"sort"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	s3types "github.com/aws/aws-sdk-go-v2/service/s3/types"
)

// partSlice returns the byte slice for part partNum (1-indexed) given chunkSize.
// The final part may be smaller than chunkSize.
func partSlice(data []byte, chunkSize int64, partNum int) []byte {
	dataSize := int64(len(data))
	start := int64(partNum-1) * chunkSize
	end := start + chunkSize
	if end > dataSize {
		end = dataSize
	}
	return data[start:end]
}

// partResult carries the outcome of a single concurrent part upload.
type partResult struct {
	partNumber int
	etag       string
	size       int64
	err        error
}

// initiateMultipartUpload calls CreateMultipartUpload and returns the upload ID.
// contentEncoding is the HTTP Content-Encoding token (e.g. "zstd") or "" if
// no transparent compression was applied.
// objectMeta is the S3 user metadata to attach: the SHA-256 of the uncompressed
// content under "objectfs-sha256", plus "objectfs-original-size" when the payload
// was compressed.
func (b *Backend) initiateMultipartUpload(
	ctx context.Context,
	key, contentType, contentEncoding string,
	objectMeta map[string]string,
	storageClass s3types.StorageClass,
) (string, error) {
	var uploadID string
	err := b.executeWithAccelerationFallback(ctx, "CreateMultipartUpload", func(client *s3.Client) error {
		input := &s3.CreateMultipartUploadInput{
			Bucket:       aws.String(b.bucket),
			Key:          aws.String(key),
			ContentType:  aws.String(contentType),
			StorageClass: storageClass,
			Metadata:     objectMeta,
		}
		if contentEncoding != "" {
			input.ContentEncoding = aws.String(contentEncoding)
		}
		result, err := client.CreateMultipartUpload(ctx, input)
		if err != nil {
			b.metricsCollector.RecordError(err)
			return b.translateError(err, "CreateMultipartUpload", key)
		}
		uploadID = aws.ToString(result.UploadId)
		return nil
	})
	return uploadID, err
}

// uploadSinglePart uploads one part with retry and returns the ETag and byte count.
func (b *Backend) uploadSinglePart(
	ctx context.Context,
	key, uploadID string,
	partNum int,
	partData []byte,
	uploadState *MultipartUploadState,
) (etag string, size int64, err error) {
	size = int64(len(partData))
	err = b.retryer.DoWithContext(ctx, func(retryCtx context.Context) error {
		return b.executeWithAccelerationFallback(retryCtx, "UploadPart", func(client *s3.Client) error {
			result, uploadErr := client.UploadPart(retryCtx, &s3.UploadPartInput{
				Bucket:        aws.String(b.bucket),
				Key:           aws.String(key),
				UploadId:      aws.String(uploadID),
				PartNumber:    aws.Int32(int32(partNum)),
				Body:          bytes.NewReader(partData),
				ContentLength: aws.Int64(size),
			})
			if uploadErr != nil {
				b.metricsCollector.RecordError(uploadErr)
				return b.translateError(uploadErr, "UploadPart", key)
			}
			etag = aws.ToString(result.ETag)
			b.multipartManager.UpdatePartStatus(uploadID, partNum, size, etag, nil)
			b.logger.Debug("Part uploaded successfully",
				"upload_id", uploadID,
				"part_number", partNum,
				"size", size,
				"progress", fmt.Sprintf("%.1f%%", uploadState.GetProgress()))
			return nil
		})
	})
	return etag, size, err
}

// uploadParts fans out goroutines (bounded by MultipartConcurrency) to upload all
// parts concurrently. On success it returns the completed parts list and total
// bytes uploaded. On failure it returns the first part error encountered.
func (b *Backend) uploadParts(
	ctx context.Context,
	key, uploadID string,
	data []byte,
	chunkSize int64,
	totalParts int,
	uploadState *MultipartUploadState,
) ([]s3types.CompletedPart, int64, error) {
	resultCh := make(chan partResult, totalParts)
	semaphore := make(chan struct{}, b.config.MultipartConcurrency)

	for partNum := 1; partNum <= totalParts; partNum++ {
		go func(pn int) {
			semaphore <- struct{}{}
			defer func() { <-semaphore }()

			pd := partSlice(data, chunkSize, pn)
			etag, size, err := b.uploadSinglePart(ctx, key, uploadID, pn, pd, uploadState)
			if err != nil {
				b.multipartManager.UpdatePartStatus(uploadID, pn, 0, "", err)
			}
			resultCh <- partResult{partNumber: pn, etag: etag, size: size, err: err}
		}(partNum)
	}

	completedParts := make([]s3types.CompletedPart, 0, totalParts)
	var uploadErrors []error
	var totalBytes int64

	for i := 0; i < totalParts; i++ {
		r := <-resultCh
		if r.err != nil {
			uploadErrors = append(uploadErrors, fmt.Errorf("part %d failed: %w", r.partNumber, r.err))
			continue
		}
		completedParts = append(completedParts, s3types.CompletedPart{
			PartNumber: aws.Int32(int32(r.partNumber)),
			ETag:       aws.String(r.etag),
		})
		totalBytes += r.size
	}

	if len(uploadErrors) > 0 {
		return nil, 0, fmt.Errorf("%d parts failed: %w", len(uploadErrors), uploadErrors[0])
	}

	// S3 requires parts in ascending PartNumber order for CompleteMultipartUpload.
	// Goroutines finish in completion order, so sort before returning.
	sort.Slice(completedParts, func(i, j int) bool {
		return aws.ToInt32(completedParts[i].PartNumber) < aws.ToInt32(completedParts[j].PartNumber)
	})

	return completedParts, totalBytes, nil
}

// abortMultipartUpload calls AbortMultipartUpload to release S3 resources after
// a failed upload.
func (b *Backend) abortMultipartUpload(ctx context.Context, key, uploadID string) error {
	return b.executeWithAccelerationFallback(ctx, "AbortMultipartUpload", func(client *s3.Client) error {
		_, err := client.AbortMultipartUpload(ctx, &s3.AbortMultipartUploadInput{
			Bucket:   aws.String(b.bucket),
			Key:      aws.String(key),
			UploadId: aws.String(uploadID),
		})
		return err
	})
}

// completeMultipartUpload calls CompleteMultipartUpload to assemble all parts
// into the final S3 object.
func (b *Backend) completeMultipartUpload(
	ctx context.Context,
	key, uploadID string,
	parts []s3types.CompletedPart,
) error {
	return b.executeWithAccelerationFallback(ctx, "CompleteMultipartUpload", func(client *s3.Client) error {
		_, err := client.CompleteMultipartUpload(ctx, &s3.CompleteMultipartUploadInput{
			Bucket:   aws.String(b.bucket),
			Key:      aws.String(key),
			UploadId: aws.String(uploadID),
			MultipartUpload: &s3types.CompletedMultipartUpload{
				Parts: parts,
			},
		})
		if err != nil {
			b.metricsCollector.RecordError(err)
			return b.translateError(err, "CompleteMultipartUpload", key)
		}
		return nil
	})
}
