package docker

import (
	"context"
	"testing"

	v1 "NYCU-SDC/caravanserai/api/v1"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"
)

func containerLabels(uid string, generation int64, service string) map[string]string {
	return map[string]string{
		labelProject:    "blog",
		labelNamespace:  "default",
		labelService:    service,
		labelUID:        uid,
		labelGeneration: generationLabel(generation),
	}
}

func lifetimeLabels(uid string) map[string]string {
	return map[string]string{labelProject: "blog", labelNamespace: "default", labelUID: uid}
}

// cleanupFixture holds, on one daemon, lifetime uid-1 at generations 3 and 5
// and a same-name successor lifetime uid-2's container. The network and the
// Ephemeral volume belong to uid-1.
func cleanupFixture() (*DockerRuntime, *fakeDocker) {
	fake := newFakeDocker()
	fake.withContainer("cara-blog-web-g3", "exited", containerLabels("uid-1", 3, "web"))
	fake.withContainer("cara-blog-db-g3", "running", containerLabels("uid-1", 3, "db"))
	fake.withContainer("cara-blog-web-g5", "exited", containerLabels("uid-1", 5, "web"))
	fake.withContainer("cara-blog-web-uid2", "running", containerLabels("uid-2", 1, "web"))
	fake.networks[NetworkName("blog")] = lifetimeLabels("uid-1")
	fake.volumes[VolumeName("blog", "cache")] = true
	fake.volumeLabels[VolumeName("blog", "cache")] = lifetimeLabels("uid-1")
	return &DockerRuntime{client: fake, logger: zap.NewNop()}, fake
}

func gen(n int64) Assignment {
	return Assignment{Namespace: "default", Project: "blog", UID: "uid-1", Generation: n}
}

// Two obligations of one lifetime on one daemon are resolved independently:
// each removes only its own containers, and the lifetime's shared network and
// volumes go with whichever is resolved last.
func TestRemoveAssignmentResolvesGenerationsIndependently(t *testing.T) {
	rt, fake := cleanupFixture()
	ctx := context.Background()

	require.NoError(t, rt.RemoveAssignment(ctx, gen(3)))
	assert.NotContains(t, fake.containers, "cara-blog-web-g3")
	assert.NotContains(t, fake.containers, "cara-blog-db-g3")
	assert.Contains(t, fake.containers, "cara-blog-web-g5", "removed another generation's container")
	assert.Contains(t, fake.networks, NetworkName("blog"), "removed the network while generation 5 still uses it")
	assert.True(t, fake.volumes[VolumeName("blog", "cache")], "removed the volume while generation 5 still uses it")

	left, err := rt.AssignmentRemaining(ctx, gen(3))
	require.NoError(t, err)
	assert.Empty(t, left, "generation 3 is done; the shared resources are generation 5's to account for")
	left, err = rt.AssignmentRemaining(ctx, gen(5))
	require.NoError(t, err)
	assert.ElementsMatch(t, []string{"container /cara-blog-web-g5", "network " + NetworkName("blog"), "volume " + VolumeName("blog", "cache")}, left)

	require.NoError(t, rt.RemoveAssignment(ctx, gen(5)))
	left, err = rt.AssignmentRemaining(ctx, gen(5))
	require.NoError(t, err)
	assert.Empty(t, left)
	assert.NotContains(t, fake.networks, NetworkName("blog"))
	assert.False(t, fake.volumes[VolumeName("blog", "cache")])

	assert.Contains(t, fake.containers, "cara-blog-web-uid2", "touched another lifetime's container")
}

// A same-named network or volume of another lifetime is neither removed nor
// counted as this assignment's, whatever the UID enforcement setting.
func TestRemoveAssignmentLeavesOtherLifetimes(t *testing.T) {
	for _, enforce := range []bool{false, true} {
		fake := newFakeDocker()
		fake.withContainer("cara-blog-web", "exited", containerLabels("uid-1", 2, "web"))
		fake.networks[NetworkName("blog")] = lifetimeLabels("uid-2")
		fake.volumes[VolumeName("blog", "cache")] = true
		fake.volumeLabels[VolumeName("blog", "cache")] = lifetimeLabels("uid-2")
		rt := &DockerRuntime{client: fake, logger: zap.NewNop(), enforceUID: enforce}
		a := Assignment{Namespace: "default", Project: "blog", UID: "uid-1", Generation: 2}

		require.NoError(t, rt.RemoveAssignment(context.Background(), a))
		left, err := rt.AssignmentRemaining(context.Background(), a)
		require.NoError(t, err)
		assert.Empty(t, left, "enforceUID=%v", enforce)
		assert.Contains(t, fake.networks, NetworkName("blog"), "enforceUID=%v: removed another lifetime's network", enforce)
		assert.True(t, fake.volumes[VolumeName("blog", "cache")], "enforceUID=%v: removed another lifetime's volume", enforce)
	}
}

func TestRemoveAssignmentRequiresUID(t *testing.T) {
	rt, fake := cleanupFixture()
	a := Assignment{Namespace: "default", Project: "blog", Generation: 3}
	assert.Error(t, rt.RemoveAssignment(context.Background(), a))
	_, err := rt.AssignmentRemaining(context.Background(), a)
	assert.Error(t, err)
	assert.Empty(t, fake.mutations, "an assignment with no UID would match every lifetime")
}

// An Ephemeral volume is labelled with the lifetime that created it, and a
// same-named volume of another lifetime is not adopted.
func TestEphemeralVolumeOwnership(t *testing.T) {
	identity := ProjectIdentity{Namespace: "default", Name: "blog", UID: "uid-2"}
	vName := VolumeName("blog", "cache")

	t.Run("created with the lifetime's UID", func(t *testing.T) {
		fake := newFakeDocker()
		rt := &DockerRuntime{client: fake, logger: zap.NewNop(), enforceUID: true}
		require.NoError(t, rt.ensureEphemeralVolume(context.Background(), identity, "cache"))
		assert.Equal(t, "uid-2", fake.volumeLabels[vName][labelUID])
		assert.Equal(t, string(v1.VolumeTypeEphemeral), fake.volumeLabels[vName][labelVolumeType])
	})

	t.Run("a predecessor's volume is refused", func(t *testing.T) {
		for _, labels := range []map[string]string{lifetimeLabels("uid-1"), {labelProject: "blog", labelNamespace: "default"}} {
			fake := newFakeDocker()
			fake.volumes[vName] = true
			fake.volumeLabels[vName] = labels
			rt := &DockerRuntime{client: fake, logger: zap.NewNop(), enforceUID: true}
			assert.Error(t, rt.ensureEphemeralVolume(context.Background(), identity, "cache"), "labels %v", labels)
			assert.Empty(t, fake.mutations)
		}
	})

	t.Run("its own volume is reused", func(t *testing.T) {
		fake := newFakeDocker()
		fake.volumes[vName] = true
		fake.volumeLabels[vName] = lifetimeLabels("uid-2")
		rt := &DockerRuntime{client: fake, logger: zap.NewNop(), enforceUID: true}
		require.NoError(t, rt.ensureEphemeralVolume(context.Background(), identity, "cache"))
		assert.Empty(t, fake.mutations)
	})
}

// A same-name Project created while its predecessor's Ephemeral volume is still
// on the daemon must fail without taking that volume: the reconcile refuses to
// adopt it, and the rollback that follows removes only what carries the new
// lifetime's UID — the network this attempt created — and leaves the volume
// for its own lifetime's cleanup.
func TestReconcileRollbackLeavesPredecessorVolume(t *testing.T) {
	fake := newFakeDocker()
	vName := VolumeName("blog", "cache")
	fake.volumes[vName] = true
	fake.volumeLabels[vName] = lifetimeLabels("uid-1")
	rt := &DockerRuntime{client: fake, logger: zap.NewNop(), dataRoot: t.TempDir(), enforceUID: true}

	p := &v1.Project{}
	p.Namespace, p.Name, p.UID = "default", "blog", "uid-2"
	p.Status.AssignmentGeneration = 1
	p.Spec.Volumes = []v1.VolumeDef{{Name: "cache", Type: v1.VolumeTypeEphemeral}}
	p.Spec.Services = []v1.ServiceDef{{Name: "web", Image: "nginx:alpine",
		VolumeMounts: []v1.VolumeMount{{Name: "cache", MountPath: "/cache"}}}}

	require.Error(t, rt.ReconcileProject(context.Background(), p))

	assert.True(t, fake.volumes[vName], "the rollback removed the predecessor's volume it had refused to adopt")
	assert.Equal(t, "uid-1", fake.volumeLabels[vName][labelUID])
	assert.NotContains(t, fake.networks, NetworkName("blog"), "the network this attempt created was left behind")
	assert.NotContains(t, fake.mutations, "VolumeRemove:"+vName)
}

// The rollback leaves a predecessor's network too. With UID enforcement on,
// ensureNetwork refuses it before anything is created; with it off the network
// is adopted, so the reconcile has to fail later — here on the image pull —
// for the rollback to run with that network in place.
func TestReconcileRollbackLeavesPredecessorNetwork(t *testing.T) {
	fake := newFakeDocker()
	fake.networks[NetworkName("blog")] = lifetimeLabels("uid-1")
	vName := VolumeName("blog", "cache")
	fake.volumes[vName] = true
	fake.volumeLabels[vName] = lifetimeLabels("uid-1")
	rt := &DockerRuntime{client: fake, logger: zap.NewNop(), dataRoot: t.TempDir(), enforceUID: false}

	p := &v1.Project{}
	p.Namespace, p.Name, p.UID = "default", "blog", "uid-2"
	p.Status.AssignmentGeneration = 1
	p.Spec.Services = []v1.ServiceDef{{Name: "web", Image: "nginx:alpine"}}
	fake.pullErr = map[string]error{"nginx:alpine": assert.AnError}

	require.Error(t, rt.ReconcileProject(context.Background(), p))

	assert.Contains(t, fake.networks, NetworkName("blog"), "the rollback removed another lifetime's network")
	assert.True(t, fake.volumes[vName])
}
