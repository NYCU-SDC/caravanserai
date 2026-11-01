package agent

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"time"

	v1 "NYCU-SDC/caravanserai/api/v1"
	"NYCU-SDC/caravanserai/internal/agent/pgbackup"
	"NYCU-SDC/caravanserai/internal/objectstore"
)

// completionAttempts bounds how often a report is retried within one run when
// the server cannot be reached. The backup itself is kept either way; an
// unreported one is only not shown as accepted.
const completionAttempts = 3

// completionRecorder publishes a PostgreSQL backup run: the manifest first,
// then the fenced report. The two are not atomic. A run whose assignment is
// replaced in between leaves a manifest the server never accepted, under the
// lifetime's own prefix, which nothing restores from.
type completionRecorder struct {
	client      *Client
	store       objectstore.ObjectStore
	caraVersion string
	now         func() time.Time
	backoff     time.Duration
}

func newCompletionRecorder(client *Client, store objectstore.ObjectStore, caraVersion string) *completionRecorder {
	return &completionRecorder{client: client, store: store, caraVersion: caraVersion, now: time.Now, backoff: 2 * time.Second}
}

func (r *completionRecorder) Record(ctx context.Context, project *v1.Project, backupID string, items []pgbackup.Item) error {
	manifest, err := pgbackup.BuildManifest(project, backupID, r.caraVersion, r.now(), items).Marshal()
	if err != nil {
		return err
	}
	key := v1.BackupManifestKey(project.ObjectMeta.UID, backupID)
	if _, err := r.store.Put(ctx, key, bytes.NewReader(manifest), objectstore.PutOptions{
		ContentType: "application/json", Size: int64(len(manifest)),
	}); err != nil {
		return fmt.Errorf("write manifest: %w", err)
	}

	report := make([]v1.BackupCompletionItem, 0, len(items))
	for _, it := range items {
		report = append(report, v1.BackupCompletionItem{Volume: it.Volume, Stanza: it.Stanza, Label: it.Label, Type: it.Type})
	}
	fence := fenceForProject(project)
	for attempt := 1; ; attempt++ {
		err = r.client.RecordBackupCompletion(ctx, project.Name, fence, backupID, report)
		if err == nil || errors.Is(err, ErrStaleAssignment) || errors.Is(err, ErrProjectNotFound) || attempt == completionAttempts {
			return err
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(r.backoff):
		}
	}
}
