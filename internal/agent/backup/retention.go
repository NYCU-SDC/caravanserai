package backup

import (
	"context"
	"io"
	"path"
	"sort"
	"strings"

	v1 "NYCU-SDC/caravanserai/api/v1"

	"go.uber.org/zap"
)

// RetainGenerations is how many of a Project's Offline generations are kept,
// counting the one just committed. Each is a complete copy of its volumes, so
// this is what bounds the Project's share of the object store.
const RetainGenerations = 7

// generationObjects is every object stored under one generation.
type generationObjects struct {
	id   string
	keys []string
}

func (g generationObjects) manifestKey() string {
	for _, k := range g.keys {
		if path.Base(k) == manifestFileName {
			return k
		}
	}
	return ""
}

// pruneGenerations removes this Project's generations beyond RetainGenerations.
// It runs only after a commit the server accepted, so an Agent that lost the
// assignment never deletes what the new owner relies on. Failing to prune is
// logged and retried by the next run: it never fails a backup that succeeded.
//
// Generations live under the Project's name, which an earlier lifetime may have
// used, and those backups are not this Project's to delete. So only a manifest
// naming this UID counts as ours, and one naming another UID, no UID, or
// unreadable is left alone. An upload with no manifest can never be restored,
// and goes once it is older than everything kept; a newer one may be a run still
// in progress on a Node that lost the assignment.
func (r *Runner) pruneGenerations(ctx context.Context, key ResourceKey, project *v1.Project, backupID string, log *zap.Logger) {
	uid := project.ObjectMeta.UID
	if uid == "" {
		return
	}
	prefix, err := ProjectPrefix(key.Namespace, key.Name)
	if err != nil {
		log.Warn("Failed to prune old backups", zap.Error(err))
		return
	}
	objects, err := r.store.List(ctx, prefix+"/snapshots/")
	if err != nil {
		log.Warn("Failed to prune old backups", zap.Error(err))
		return
	}

	byID := map[string]*generationObjects{}
	for _, o := range objects {
		id, _, ok := strings.Cut(strings.TrimPrefix(o.Key, prefix+"/snapshots/"), "/")
		if !ok {
			continue
		}
		if byID[id] == nil {
			byID[id] = &generationObjects{id: id}
		}
		byID[id].keys = append(byID[id].keys, o.Key)
	}

	var ours, abandoned []*generationObjects
	for _, g := range byID {
		manifestKey := g.manifestKey()
		if manifestKey == "" {
			abandoned = append(abandoned, g)
			continue
		}
		if r.manifestUID(ctx, manifestKey, log) == uid {
			ours = append(ours, g)
		}
	}
	// Backup IDs sort by creation time.
	sort.Slice(ours, func(i, j int) bool { return ours[i].id > ours[j].id })
	if len(ours) < RetainGenerations {
		return
	}

	protected := map[string]bool{backupID: true, project.Status.Recovery.CommittedBackupID(): true}
	for _, g := range ours[RetainGenerations:] {
		if !protected[g.id] {
			r.deleteGeneration(ctx, key.String(), *g, log)
		}
	}
	oldestKept := ours[RetainGenerations-1].id
	for _, g := range abandoned {
		if g.id < oldestKept {
			r.deleteGeneration(ctx, key.String(), *g, log)
		}
	}
}

// manifestUID returns the Project UID a manifest names, or "" when it names none
// or cannot be read.
func (r *Runner) manifestUID(ctx context.Context, manifestKey string, log *zap.Logger) string {
	rc, _, err := r.store.Get(ctx, manifestKey)
	if err != nil {
		log.Warn("Failed to read a backup's manifest while pruning", zap.String("key", manifestKey), zap.Error(err))
		return ""
	}
	defer func() { _ = rc.Close() }()
	body, err := io.ReadAll(rc)
	if err != nil {
		log.Warn("Failed to read a backup's manifest while pruning", zap.String("key", manifestKey), zap.Error(err))
		return ""
	}
	manifest, err := UnmarshalManifest(body)
	if err != nil {
		log.Warn("Failed to parse a backup's manifest while pruning", zap.String("key", manifestKey), zap.Error(err))
		return ""
	}
	return manifest.ProjectUID
}

// deleteGeneration removes the manifest first, so a generation interrupted here
// reads as abandoned rather than as complete with archives missing.
func (r *Runner) deleteGeneration(ctx context.Context, project string, g generationObjects, log *zap.Logger) {
	keys := append([]string(nil), g.keys...)
	sort.SliceStable(keys, func(i, j int) bool {
		return path.Base(keys[i]) == manifestFileName && path.Base(keys[j]) != manifestFileName
	})
	for _, k := range keys {
		if err := r.store.Delete(ctx, k); err != nil {
			log.Warn("Failed to delete an old backup", zap.String("backupID", g.id), zap.String("key", k), zap.Error(err))
			return
		}
	}
	log.Info("Deleted an old backup", zap.String("project", project), zap.String("backupID", g.id))
}
