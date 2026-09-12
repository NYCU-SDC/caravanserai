package docker

import (
	"os"
	"path/filepath"
	"testing"

	v1 "NYCU-SDC/caravanserai/api/v1"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"
)

func TestBuildBinds(t *testing.T) {
	const dataRoot = "/var/lib/cara"
	r := &DockerRuntime{logger: zap.NewNop(), dataRoot: dataRoot}

	vols := []v1.VolumeDef{
		{Name: "db-data", Type: v1.VolumeTypeManaged},
		{Name: "scratch", Type: v1.VolumeTypeEphemeral},
	}

	t.Run("managed volume binds to the derived host path", func(t *testing.T) {
		svc := v1.ServiceDef{
			Name: "db",
			VolumeMounts: []v1.VolumeMount{
				{Name: "db-data", MountPath: "/var/lib/postgresql/data"},
			},
		}

		binds, managed, err := r.buildBinds("default", "blog", svc, vols)
		require.NoError(t, err)
		assert.Equal(t, []string{
			"/var/lib/cara/volumes/default/blog/db-data/data:/var/lib/postgresql/data",
		}, binds)
		assert.Equal(t, []string{"db-data"}, managed)
	})

	t.Run("ephemeral volume binds to the docker named volume", func(t *testing.T) {
		svc := v1.ServiceDef{
			Name: "cache",
			VolumeMounts: []v1.VolumeMount{
				{Name: "scratch", MountPath: "/scratch"},
			},
		}

		binds, managed, err := r.buildBinds("default", "blog", svc, vols)
		require.NoError(t, err)
		// Named volume, not a host path — the pre-fix behaviour for Ephemeral.
		assert.Equal(t, []string{"cara-blog-scratch:/scratch"}, binds)
		assert.Empty(t, managed)
	})

	t.Run("mixed mounts resolve each by type", func(t *testing.T) {
		svc := v1.ServiceDef{
			Name: "app",
			VolumeMounts: []v1.VolumeMount{
				{Name: "db-data", MountPath: "/data"},
				{Name: "scratch", MountPath: "/tmp/cache"},
			},
		}

		binds, managed, err := r.buildBinds("default", "blog", svc, vols)
		require.NoError(t, err)
		assert.Equal(t, []string{
			"/var/lib/cara/volumes/default/blog/db-data/data:/data",
			"cara-blog-scratch:/tmp/cache",
		}, binds)
		assert.Equal(t, []string{"db-data"}, managed)
	})

	t.Run("undeclared volume is an error, never a fall-through", func(t *testing.T) {
		svc := v1.ServiceDef{
			Name: "app",
			VolumeMounts: []v1.VolumeMount{
				{Name: "does-not-exist", MountPath: "/data"},
			},
		}

		_, _, err := r.buildBinds("default", "blog", svc, vols)
		assert.ErrorContains(t, err, "undeclared volume")
	})

	t.Run("no mounts yields no binds", func(t *testing.T) {
		binds, managed, err := r.buildBinds("default", "blog", v1.ServiceDef{Name: "web"}, vols)
		require.NoError(t, err)
		assert.Empty(t, binds)
		assert.Empty(t, managed)
	})
}

func TestPlanVolumeRemoval(t *testing.T) {
	const (
		dataRoot  = "/var/lib/cara"
		namespace = "default"
		project   = "blog"
	)

	t.Run("ephemeral is removed, no path retained", func(t *testing.T) {
		plan := planVolumeRemoval(dataRoot, namespace, project,
			[]v1.VolumeDef{{Name: "scratch", Type: v1.VolumeTypeEphemeral}})

		assert.Equal(t, []string{"cara-blog-scratch"}, plan.removeNamedVolumes)
		assert.Empty(t, plan.retainedManagedPaths)
	})

	t.Run("managed retains its host path and sweeps a legacy named volume", func(t *testing.T) {
		plan := planVolumeRemoval(dataRoot, namespace, project,
			[]v1.VolumeDef{{Name: "db-data", Type: v1.VolumeTypeManaged}})

		// Legacy orphan cleanup: the pre-CARA-66 auto-created name is removed.
		assert.Equal(t, []string{"cara-blog-db-data"}, plan.removeNamedVolumes)
		// Host data is retained, never scheduled for deletion.
		assert.Equal(t, []string{
			"/var/lib/cara/volumes/default/blog/db-data/data",
		}, plan.retainedManagedPaths)
	})

	t.Run("mixed project handles each volume by type", func(t *testing.T) {
		plan := planVolumeRemoval(dataRoot, namespace, project, []v1.VolumeDef{
			{Name: "db-data", Type: v1.VolumeTypeManaged},
			{Name: "scratch", Type: v1.VolumeTypeEphemeral},
		})

		assert.ElementsMatch(t, []string{"cara-blog-db-data", "cara-blog-scratch"}, plan.removeNamedVolumes)
		assert.Equal(t, []string{
			"/var/lib/cara/volumes/default/blog/db-data/data",
		}, plan.retainedManagedPaths)
	})

	t.Run("unresolvable host path is reported instead of silently dropped", func(t *testing.T) {
		// A volume name this malformed should never reach planVolumeRemoval in
		// practice (v1.ValidateName rejects it at create time), but the legacy
		// named volume must still be scheduled for removal, and the path
		// failure must be surfaced rather than silently skipped.
		plan := planVolumeRemoval(dataRoot, namespace, project,
			[]v1.VolumeDef{{Name: "../escape", Type: v1.VolumeTypeManaged}})

		assert.Equal(t, []string{"cara-blog-../escape"}, plan.removeNamedVolumes)
		assert.Empty(t, plan.retainedManagedPaths)
		require.Len(t, plan.pathErrors, 1)
		assert.ErrorContains(t, plan.pathErrors[0], "../escape")
	})
}

func TestEnsureManagedDir(t *testing.T) {
	root := t.TempDir()
	r := &DockerRuntime{logger: zap.NewNop(), dataRoot: root}

	t.Run("creates the directory with owner-only permissions", func(t *testing.T) {
		err := r.ensureManagedDir("default", "blog", "db-data")
		require.NoError(t, err)

		path := filepath.Join(root, "volumes", "default", "blog", "db-data", "data")
		info, statErr := os.Stat(path)
		require.NoError(t, statErr)
		assert.True(t, info.IsDir())
		assert.Equal(t, os.FileMode(0o700), info.Mode().Perm())
	})

	t.Run("is idempotent and preserves existing content", func(t *testing.T) {
		require.NoError(t, r.ensureManagedDir("default", "blog", "keep"))
		path := filepath.Join(root, "volumes", "default", "blog", "keep", "data")
		marker := filepath.Join(path, "existing.txt")
		require.NoError(t, os.WriteFile(marker, []byte("data"), 0o600))

		// Second call must not wipe the directory.
		require.NoError(t, r.ensureManagedDir("default", "blog", "keep"))
		_, statErr := os.Stat(marker)
		assert.NoError(t, statErr, "existing volume content must survive re-provision")
	})

	t.Run("rejects a name that would escape the data root", func(t *testing.T) {
		err := r.ensureManagedDir("default", "..", "db-data")
		assert.Error(t, err)
	})

	t.Run("fails when the path is occupied by a file", func(t *testing.T) {
		// Pre-create the volume dir's parent as a file so MkdirAll fails.
		clash := filepath.Join(root, "volumes", "default", "clash")
		require.NoError(t, os.MkdirAll(filepath.Dir(clash), 0o700))
		require.NoError(t, os.WriteFile(clash, []byte("x"), 0o600))

		err := r.ensureManagedDir("default", "clash", "db-data")
		assert.Error(t, err)
	})
}

func TestContainerOwnershipFiltersRequireCompleteIdentity(t *testing.T) {
	project := ProjectIdentity{Namespace: "team-a", Name: "payments"}
	labels := containerOwnershipFilters(project).Get("label")

	assert.ElementsMatch(t, []string{
		"cara.project=payments",
		"cara.namespace=team-a",
		"cara.service",
	}, labels)
}

func TestResourceOwnershipFiltersRequireNamespaceAndProject(t *testing.T) {
	project := ProjectIdentity{Namespace: "team-a", Name: "payments"}
	labels := resourceOwnershipFilters(project).Get("label")

	assert.ElementsMatch(t, []string{
		"cara.project=payments",
		"cara.namespace=team-a",
	}, labels)
}

func TestValidateNetworkOwnership(t *testing.T) {
	project := ProjectIdentity{Namespace: "team-a", Name: "payments"}

	tests := []struct {
		name    string
		labels  map[string]string
		wantErr bool
	}{
		{name: "complete Cara labels", labels: map[string]string{
			labelProject: "payments", labelNamespace: "team-a",
		}},
		{name: "legacy Cara network without namespace", labels: map[string]string{
			labelProject: "payments",
		}},
		{name: "foreign network with no labels", labels: nil, wantErr: true},
		{name: "different project", labels: map[string]string{
			labelProject: "inventory", labelNamespace: "team-a",
		}, wantErr: true},
		{name: "different namespace", labels: map[string]string{
			labelProject: "payments", labelNamespace: "team-b",
		}, wantErr: true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := (&DockerRuntime{}).validateNetworkOwnership("cara-payments", tt.labels, project)
			assert.Equal(t, tt.wantErr, err != nil)
		})
	}
}

// CARA-82: when the identity carries a UID, ownership filters scope the match
// to exactly that Project lifetime so orphan stop/remove of a previous lifetime
// never touches the current one.
func TestContainerOwnershipFilters_IncludeUIDWhenPresent(t *testing.T) {
	withUID := containerOwnershipFilters(ProjectIdentity{Namespace: "team-a", Name: "payments", UID: "uid-1"}).Get("label")
	assert.ElementsMatch(t, []string{
		"cara.project=payments",
		"cara.namespace=team-a",
		"cara.service",
		"cara.uid=uid-1",
	}, withUID)

	// Compatibility mode: no UID in the identity means no UID filter, matching
	// pre-CARA-82 behaviour.
	noUID := containerOwnershipFilters(ProjectIdentity{Namespace: "team-a", Name: "payments"}).Get("label")
	assert.ElementsMatch(t, []string{
		"cara.project=payments",
		"cara.namespace=team-a",
		"cara.service",
	}, noUID)
}

func TestResourceOwnershipFilters_IncludeUIDWhenPresent(t *testing.T) {
	withUID := resourceOwnershipFilters(ProjectIdentity{Namespace: "team-a", Name: "payments", UID: "uid-1"}).Get("label")
	assert.ElementsMatch(t, []string{
		"cara.project=payments",
		"cara.namespace=team-a",
		"cara.uid=uid-1",
	}, withUID)
}

// CARA-82: UID fencing on network adoption. When enforcing, an existing network
// must carry the current Project's UID; a different or missing value is refused.
// When not enforcing, UID is ignored so mixed-rollout networks keep working.
func TestValidateNetworkOwnership_UID(t *testing.T) {
	project := ProjectIdentity{Namespace: "team-a", Name: "payments", UID: "uid-new"}

	tests := []struct {
		name       string
		enforceUID bool
		labels     map[string]string
		wantErr    bool
	}{
		{name: "enforce: matching UID", enforceUID: true, labels: map[string]string{
			labelProject: "payments", labelNamespace: "team-a", labelUID: "uid-new",
		}},
		{name: "enforce: different UID rejected", enforceUID: true, labels: map[string]string{
			labelProject: "payments", labelNamespace: "team-a", labelUID: "uid-old",
		}, wantErr: true},
		{name: "enforce: legacy missing UID rejected", enforceUID: true, labels: map[string]string{
			labelProject: "payments", labelNamespace: "team-a",
		}, wantErr: true},
		{name: "compat: different UID tolerated", enforceUID: false, labels: map[string]string{
			labelProject: "payments", labelNamespace: "team-a", labelUID: "uid-old",
		}},
		{name: "compat: legacy missing UID tolerated", enforceUID: false, labels: map[string]string{
			labelProject: "payments", labelNamespace: "team-a",
		}},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			r := &DockerRuntime{enforceUID: tt.enforceUID}
			err := r.validateNetworkOwnership("cara-payments", tt.labels, project)
			assert.Equal(t, tt.wantErr, err != nil)
		})
	}
}

// The preflight is the only thing standing between a recovery and Docker's
// habit of creating whatever a container mounts and cannot find. Every case
// here is one where the container would otherwise have come up healthy and
// empty.
func TestPreflightVolumesManaged(t *testing.T) {
	root := t.TempDir()
	r := &DockerRuntime{logger: zap.NewNop(), dataRoot: root}

	project := &v1.Project{}
	project.Namespace = "default"
	project.Name = "blog"
	project.Spec.Volumes = []v1.VolumeDef{{Name: "db-data", Type: v1.VolumeTypeManaged}}

	svc := v1.ServiceDef{
		Name:         "db",
		VolumeMounts: []v1.VolumeMount{{Name: "db-data", MountPath: "/var/lib/postgresql/data"}},
	}

	path := filepath.Join(root, "volumes", "default", "blog", "db-data", "data")

	t.Run("passes when the host directory is in place", func(t *testing.T) {
		require.NoError(t, os.MkdirAll(path, 0o700))
		assert.NoError(t, r.preflightVolumes(t.Context(), project, svc))
	})

	t.Run("fails when the host directory is gone", func(t *testing.T) {
		require.NoError(t, os.RemoveAll(path))

		err := r.preflightVolumes(t.Context(), project, svc)
		require.Error(t, err)
		assert.ErrorContains(t, err, "db-data")
		assert.ErrorContains(t, err, "missing")
	})

	t.Run("fails when the path is not a directory", func(t *testing.T) {
		require.NoError(t, os.RemoveAll(path))
		require.NoError(t, os.MkdirAll(filepath.Dir(path), 0o700))
		require.NoError(t, os.WriteFile(path, []byte("not a directory"), 0o600))
		t.Cleanup(func() { _ = os.RemoveAll(path) })

		err := r.preflightVolumes(t.Context(), project, svc)
		require.Error(t, err)
		assert.ErrorContains(t, err, "not a directory")
	})

	t.Run("fails when the directory cannot be accessed", func(t *testing.T) {
		if os.Geteuid() == 0 {
			t.Skip("root bypasses permission bits")
		}
		require.NoError(t, os.RemoveAll(path))
		require.NoError(t, os.MkdirAll(path, 0o000))
		t.Cleanup(func() { _ = os.Chmod(path, 0o700) })

		err := r.preflightVolumes(t.Context(), project, svc)
		require.Error(t, err)
		assert.ErrorContains(t, err, "not accessible")
	})
}

// A service with no mounts must not be blocked, and an undeclared mount must
// not fall through to Docker — the same guard buildBinds applies at create
// time, applied one step earlier.
func TestPreflightVolumesEdgeCases(t *testing.T) {
	r := &DockerRuntime{logger: zap.NewNop(), dataRoot: t.TempDir()}

	project := &v1.Project{}
	project.Namespace = "default"
	project.Name = "blog"

	t.Run("a service with no mounts needs no volumes", func(t *testing.T) {
		assert.NoError(t, r.preflightVolumes(t.Context(), project, v1.ServiceDef{Name: "web"}))
	})

	t.Run("an undeclared volume is an error", func(t *testing.T) {
		svc := v1.ServiceDef{
			Name:         "web",
			VolumeMounts: []v1.VolumeMount{{Name: "nowhere", MountPath: "/data"}},
		}

		err := r.preflightVolumes(t.Context(), project, svc)
		require.Error(t, err)
		assert.ErrorContains(t, err, "undeclared volume")
	})

	t.Run("an unsupported volume type is an error", func(t *testing.T) {
		project.Spec.Volumes = []v1.VolumeDef{{Name: "weird", Type: v1.VolumeType("HostPath")}}
		svc := v1.ServiceDef{
			Name:         "web",
			VolumeMounts: []v1.VolumeMount{{Name: "weird", MountPath: "/data"}},
		}

		err := r.preflightVolumes(t.Context(), project, svc)
		require.Error(t, err)
		assert.ErrorContains(t, err, "unsupported type")
	})
}
