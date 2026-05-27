package storage

import (
	"context"
	"fmt"
	"io"
	"log"
	"os"
	"path/filepath"
	"time"

	"cloud.google.com/go/storage"
	"google.golang.org/api/option"
)

type Uploader struct {
	client     *storage.Client
	bucketName string
}

// NewUploader initializes the Firebase Storage client
func NewUploader(ctx context.Context, projectID string, credentialsPath string) (*Uploader, error) {
	bucketName := fmt.Sprintf("%s.appspot.com", projectID)

	// If you are using a custom bucket name, you can pass it here
	// For Firebase, the default is usually project-id.appspot.com

	opt := option.WithCredentialsFile(credentialsPath)
	client, err := storage.NewClient(ctx, opt)
	if err != nil {
		return nil, fmt.Errorf("failed to create storage client: %w", err)
	}

	return &Uploader{
		client:     client,
		bucketName: bucketName,
	}, nil
}

// UploadRecord uploads a local file to: hi-silver/sessionId/YYYY-MM-DD/filename
func (u *Uploader) UploadRecord(ctx context.Context, sessionID string, filePath string) (string, error) {
	f, err := os.Open(filePath)
	if err != nil {
		return "", fmt.Errorf("failed to open local file: %w", err)
	}
	defer f.Close()

	// 1. Construct the path: hi-silver/sessionId/date
	dateStr := time.Now().Format("2006-01-02")
	fileName := filepath.Base(filePath)
	remotePath := fmt.Sprintf("hi-silver/%s/%s/%s", sessionID, dateStr, fileName)

	log.Printf("☁️ [Storage] Uploading %s to %s...", fileName, remotePath)

	// 2. Upload to Firebase Storage
	wc := u.client.Bucket(u.bucketName).Object(remotePath).NewWriter(ctx)

	// Set Metadata (important for the mobile app to know the file type)
	wc.ContentType = "video/x-ivf"

	if _, err = io.Copy(wc, f); err != nil {
		return "", fmt.Errorf("io.Copy failed: %w", err)
	}

	if err := wc.Close(); err != nil {
		return "", fmt.Errorf("writer.Close failed: %w", err)
	}

	log.Printf("✅ [Storage] Upload complete: %s", remotePath)

	// 3. Generate the path/reference for the mobile app
	// Note: We don't usually use signed URLs for high-frequency cat videos,
	// the app can use the Firebase SDK to get the download URL via the path.
	return remotePath, nil
}
