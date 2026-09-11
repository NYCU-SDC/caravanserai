package agent

import (
	"testing"

	v1 "NYCU-SDC/caravanserai/api/v1"
	"NYCU-SDC/caravanserai/internal/agent/docker"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func projectWith(services ...string) *v1.Project {
	p := &v1.Project{}
	for _, name := range services {
		p.Spec.Services = append(p.Spec.Services, v1.ServiceDef{Name: name, Image: "scratch"})
	}
	return p
}

func running(service string) docker.ContainerState {
	return docker.ContainerState{ServiceName: service, Status: "running"}
}

// Every Docker status must map to an explicit decision. The four at the bottom
// of this table previously matched none of healthCheckOne's checks and fell
// through to "healthy", which is how a dead or paused container could sit
// broken while the Project reported Running.
func TestClassifyServiceCoversEveryDockerStatus(t *testing.T) {
	cases := []struct {
		status string
		want   serviceHealth
	}{
		{"running", healthRunning},
		{"exited", healthStartable},
		{"created", healthStartable},
		{"dead", healthNeedsRecreate},
		{"paused", healthPaused},
		{"restarting", healthTransient},
		{"", healthUnknown},
		{"something-docker-added-later", healthUnknown},
	}

	for _, tc := range cases {
		t.Run(tc.status, func(t *testing.T) {
			got := classifyService(docker.ContainerState{Status: tc.status})
			assert.Equal(t, tc.want, got, "status %q", tc.status)
		})
	}
}

// A clean exit is as broken as a crash. cara has no notion of a service that
// legitimately finishes, and `docker stop` sends SIGTERM — so the most common
// operator action produces exit 0 and must not be mistaken for health.
func TestClassifyServiceTreatsCleanExitAsBroken(t *testing.T) {
	clean := docker.ContainerState{Status: "exited", ExitCode: 0}
	crash := docker.ContainerState{Status: "exited", ExitCode: 1}

	assert.Equal(t, healthStartable, classifyService(clean))
	assert.Equal(t, healthStartable, classifyService(crash))
}

// Classification walks the spec, so a service with no container at all is a
// classification rather than something the caller infers by comparing counts.
func TestClassifyProjectReportsMissingServiceByName(t *testing.T) {
	p := projectWith("web", "db")

	got := classifyProject(p, []docker.ContainerState{running("web")})

	require.Len(t, got, 2)
	assert.Equal(t, "web", got[0].Service)
	assert.Equal(t, healthRunning, got[0].Health)
	assert.Equal(t, "db", got[1].Service)
	assert.Equal(t, healthNeedsRecreate, got[1].Health)
	assert.Empty(t, got[1].Status, "a missing container has no Docker status")
}

func TestClassifyProjectPreservesSpecOrder(t *testing.T) {
	p := projectWith("a", "b", "c")

	got := classifyProject(p, []docker.ContainerState{running("c"), running("a"), running("b")})

	require.Len(t, got, 3)
	assert.Equal(t, []string{"a", "b", "c"}, []string{got[0].Service, got[1].Service, got[2].Service})
}

func TestUnhealthySkipsRunningContainers(t *testing.T) {
	p := projectWith("web", "db")

	states := classifyProject(p, []docker.ContainerState{
		running("web"),
		{ServiceName: "db", Status: "exited", ExitCode: 1},
	})

	bad := unhealthy(states)
	require.Len(t, bad, 1)
	assert.Equal(t, "db", bad[0].Service)
}

// The three reasons that already existed are kept byte-for-byte so anything
// watching for them keeps working; the states that used to fall through get a
// new one rather than being folded into a reason that would misdescribe them.
func TestFailureReasonPicksTheMostSevereFault(t *testing.T) {
	cases := []struct {
		name string
		bad  []serviceState
		want string
	}{
		{
			name: "crash wins over everything",
			bad: []serviceState{
				{Service: "a", Health: healthStartable, Status: "exited", ExitCode: 1},
				{Service: "b", Health: healthNeedsRecreate},
			},
			want: "ContainerCrashed",
		},
		{
			name: "missing wins over a clean exit",
			bad: []serviceState{
				{Service: "a", Health: healthNeedsRecreate},
				{Service: "b", Health: healthStartable, Status: "exited", ExitCode: 0},
			},
			want: "ContainerMissing",
		},
		{
			name: "clean exit alone",
			bad:  []serviceState{{Service: "a", Health: healthStartable, Status: "exited", ExitCode: 0}},
			want: "ContainerExited",
		},
		{
			name: "dead container",
			bad:  []serviceState{{Service: "a", Health: healthNeedsRecreate, Status: "dead"}},
			want: "ContainerUnhealthy",
		},
		{
			name: "paused container",
			bad:  []serviceState{{Service: "a", Health: healthPaused, Status: "paused"}},
			want: "ContainerUnhealthy",
		},
		{
			name: "unrecognised status",
			bad:  []serviceState{{Service: "a", Health: healthUnknown, Status: "who-knows"}},
			want: "ContainerUnhealthy",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			reason, summary := failureReason(tc.bad)
			assert.Equal(t, tc.want, reason)
			assert.NotEmpty(t, summary)
		})
	}
}

// A Project with two different faults must report both. The previous
// implementation returned after the first matching check, so a crashed
// container hid a missing one.
func TestDetailListNamesEveryUnhealthyService(t *testing.T) {
	bad := []serviceState{
		{Service: "web", Health: healthStartable, Status: "exited", ExitCode: 1},
		{Service: "db", Health: healthNeedsRecreate},
		{Service: "cache", Health: healthPaused, Status: "paused"},
	}

	assert.Equal(t, "web(exit=1), db, cache(paused)", detailList(bad))
}

// One restarting service is enough to defer the whole Project: the others are
// judged once Docker has settled.
func TestAnyTransientDefersOnASingleRestartingService(t *testing.T) {
	restarting := serviceState{Service: "a", Health: healthTransient, Status: "restarting"}
	exited := serviceState{Service: "b", Health: healthStartable, Status: "exited"}

	assert.True(t, anyTransient([]serviceState{restarting}))
	assert.True(t, anyTransient([]serviceState{restarting, exited}))
	assert.False(t, anyTransient([]serviceState{exited}))
}

func TestNeedsHumanOnPausedOrUnknownOnly(t *testing.T) {
	assert.True(t, needsHuman([]serviceState{{Health: healthPaused}}))
	assert.True(t, needsHuman([]serviceState{{Health: healthUnknown}}))
	assert.True(t, needsHuman([]serviceState{{Health: healthTransient}, {Health: healthPaused}}))
	assert.False(t, needsHuman([]serviceState{{Health: healthTransient}, {Health: healthStartable}}))
	assert.False(t, needsHuman([]serviceState{{Health: healthNeedsRecreate}}))
}
