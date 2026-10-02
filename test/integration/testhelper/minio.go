package testhelper

import (
	"context"
	"fmt"
	"log"
	"strings"
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

// The MinIO server image, pinned by digest.
//
// Chainguard's rebuild, because MinIO no longer serves an image anyone can
// pull. It deleted minio/minio from Docker Hub in September 2026, and days
// later made quay.io/minio/minio private too: an anonymous token for that
// repository now carries no pull permission, so every tag answers 401 and
// Docker reports "unauthorized: access to the requested resource is not
// authorized". Re-pinning to another Quay tag does not help.
//
// The one image MinIO still publishes, quay.io/minio/aistor/minio, is its
// commercial product. Without a license it starts with every S3 operation
// blocked, so using it would mean handing a license to CI and to every
// machine that runs these tests.
//
// Chainguard builds the same server from source and publishes only "latest",
// so the digest is the pin; there is no release tag to fall back on. Keep it
// in step with the minio service in docker-compose.yaml.
const (
	minioRepository = "cgr.dev/chainguard/minio"
	minioDigest     = "sha256:bd014394a80898e68c149f2311fdf8d5a2c2f3bb2c33b9327ae6d02b4b065ae1"
)

// pinMinioImage makes the pinned MinIO image available under a local tag and
// returns that tag.
//
// dockertest names an image as "repository:tag" both when it pulls and when
// it creates the container, so it cannot express "repository@digest". Docker's
// pull API does accept a bare digest in place of a tag, so the image is
// pulled by digest here and then tagged locally under a name derived from
// that digest. RunWithOptions finds the tag already present and never pulls.
func pinMinioImage(dtPool *dockertest.Pool) (string, error) {
	tag := "pinned-" + strings.TrimPrefix(minioDigest, "sha256:")[:12]
	ref := minioRepository + ":" + tag
	if _, err := dtPool.Client.InspectImage(ref); err == nil {
		return tag, nil
	}

	if err := dtPool.Client.PullImage(docker.PullImageOptions{
		Repository: minioRepository,
		Tag:        minioDigest,
	}, docker.AuthConfiguration{}); err != nil {
		return "", fmt.Errorf("dockertest: pull %s@%s: %w", minioRepository, minioDigest, err)
	}
	if err := dtPool.Client.TagImage(minioRepository+"@"+minioDigest, docker.TagImageOptions{
		Repo: minioRepository,
		Tag:  tag,
	}); err != nil {
		return "", fmt.Errorf("dockertest: tag %s: %w", ref, err)
	}
	return tag, nil
}

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

	tag, pinErr := pinMinioImage(dtPool)
	if pinErr != nil {
		return "", nil, pinErr
	}

	resource, runErr := dtPool.RunWithOptions(&dockertest.RunOptions{
		Repository: minioRepository,
		Tag:        tag,
		Cmd:        []string{"server", "/data"},
		// The Chainguard image declares no EXPOSE, and dockertest publishes
		// only the ports an image declares, so without this the container
		// runs with no host port and GetHostPort returns "".
		ExposedPorts: []string{"9000/tcp"},
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
