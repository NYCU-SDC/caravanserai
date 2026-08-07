package testhelper

import (
	"context"
	"fmt"
	"log"
	"time"

	"github.com/minio/minio-go/v7"
	"github.com/minio/minio-go/v7/pkg/credentials"
	"github.com/ory/dockertest/v3"
	"github.com/ory/dockertest/v3/docker"
)

// MinioCredentials are the fixed root credentials the disposable MinIO
// container is started with.
const (
	MinioAccessKey = "caratest"
	MinioSecretKey = "caratest123"
	MinioBucket    = "cara-backups-test"
)

// StartMinio launches a disposable MinIO container, waits for it to accept
// requests, creates the test bucket, and returns the endpoint host:port along
// with a cleanup function.
//
// The bucket is created here because the agent deliberately never creates
// buckets: an agent that could create one would silently write to a typo'd
// bucket name instead of failing.
func StartMinio() (endpoint string, cleanup func(), err error) {
	dtPool, poolErr := dockertest.NewPool("")
	if poolErr != nil {
		return "", nil, fmt.Errorf("dockertest: connect to docker: %w", poolErr)
	}
	dtPool.MaxWait = 60 * time.Second

	resource, runErr := dtPool.RunWithOptions(&dockertest.RunOptions{
		Repository: "minio/minio",
		Tag:        "latest",
		Cmd:        []string{"server", "/data"},
		Env: []string{
			"MINIO_ROOT_USER=" + MinioAccessKey,
			"MINIO_ROOT_PASSWORD=" + MinioSecretKey,
		},
	}, func(hc *docker.HostConfig) {
		hc.AutoRemove = true
		hc.RestartPolicy = docker.RestartPolicy{Name: "no"}
	})
	if runErr != nil {
		return "", nil, fmt.Errorf("dockertest: run minio: %w", runErr)
	}

	purge := func() {
		if purgeErr := dtPool.Purge(resource); purgeErr != nil {
			log.Printf("testhelper: purge minio container: %v", purgeErr)
		}
	}

	endpoint = resource.GetHostPort("9000/tcp")

	// Wait until MinIO answers, then create the bucket.
	if retryErr := dtPool.Retry(func() error {
		client, clientErr := newMinioClient(endpoint)
		if clientErr != nil {
			return clientErr
		}
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()

		exists, existsErr := client.BucketExists(ctx, MinioBucket)
		if existsErr != nil {
			return existsErr
		}
		if !exists {
			return client.MakeBucket(ctx, MinioBucket, minio.MakeBucketOptions{})
		}
		return nil
	}); retryErr != nil {
		purge()
		return "", nil, fmt.Errorf("dockertest: minio never became ready: %w", retryErr)
	}

	return endpoint, purge, nil
}

func newMinioClient(endpoint string) (*minio.Client, error) {
	return minio.New(endpoint, &minio.Options{
		Creds:        credentials.NewStaticV4(MinioAccessKey, MinioSecretKey, ""),
		Secure:       false,
		BucketLookup: minio.BucketLookupPath,
	})
}
