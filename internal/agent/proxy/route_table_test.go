package proxy

import (
	"sync"
	"testing"

	v1 "NYCU-SDC/caravanserai/api/v1"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"
)

func TestResolveHost(t *testing.T) {
	tests := []struct {
		name        string
		host        string
		projectName string
		want        string
	}{
		{
			name:        "FQDN with dot — use as-is",
			host:        "blog.example.com",
			projectName: "my-project",
			want:        "blog.example.com",
		},
		{
			name:        "short name without dot — append project.local",
			host:        "web",
			projectName: "my-project",
			want:        "web.my-project.local",
		},
		{
			name:        "empty host — treated as no dot",
			host:        "",
			projectName: "app",
			want:        ".app.local",
		},
		{
			name:        "subdomain with dot",
			host:        "api.internal",
			projectName: "core",
			want:        "api.internal",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := ResolveHost(tt.host, tt.projectName)
			assert.Equal(t, tt.want, got)
		})
	}
}

func TestRouteTable_UpdateAndLookup(t *testing.T) {
	rt := NewRouteTable(zap.NewNop())

	project := &v1.Project{
		ObjectMeta: v1.ObjectMeta{Name: "wordpress"},
		Spec: v1.ProjectSpec{
			Services: []v1.ServiceDef{
				{Name: "app", Image: "wordpress:latest"},
				{Name: "db", Image: "mysql:8"},
			},
			Ingress: []v1.IngressDef{
				{
					Name:   "web",
					Host:   "blog.example.com",
					Target: v1.IngressTarget{Service: "app", Port: 80},
				},
				{
					Name:   "admin",
					Host:   "admin",
					Target: v1.IngressTarget{Service: "app", Port: 8080},
				},
			},
		},
	}

	containerIPs := map[string]string{
		"app": "172.18.0.2",
		"db":  "172.18.0.3",
	}

	rt.Update(project, containerIPs)

	// Lookup FQDN host.
	url, found := rt.Lookup("blog.example.com")
	require.True(t, found)
	assert.Equal(t, "http://172.18.0.2:80", url)

	// Lookup short host (resolved to admin.wordpress.local).
	url, found = rt.Lookup("admin.wordpress.local")
	require.True(t, found)
	assert.Equal(t, "http://172.18.0.2:8080", url)

	// Lookup unknown host.
	_, found = rt.Lookup("unknown.example.com")
	assert.False(t, found)
}

func TestRouteTable_LookupStripsPort(t *testing.T) {
	rt := NewRouteTable(zap.NewNop())

	project := &v1.Project{
		ObjectMeta: v1.ObjectMeta{Name: "app"},
		Spec: v1.ProjectSpec{
			Services: []v1.ServiceDef{{Name: "web", Image: "nginx"}},
			Ingress: []v1.IngressDef{
				{
					Name:   "main",
					Host:   "app.example.com",
					Target: v1.IngressTarget{Service: "web", Port: 80},
				},
			},
		},
	}

	rt.Update(project, map[string]string{"web": "172.18.0.5"})

	// Host header with port should still match.
	url, found := rt.Lookup("app.example.com:8081")
	require.True(t, found)
	assert.Equal(t, "http://172.18.0.5:80", url)
}

func TestRouteTable_Remove(t *testing.T) {
	rt := NewRouteTable(zap.NewNop())

	project := &v1.Project{
		ObjectMeta: v1.ObjectMeta{Name: "test-app"},
		Spec: v1.ProjectSpec{
			Services: []v1.ServiceDef{{Name: "web", Image: "nginx"}},
			Ingress: []v1.IngressDef{
				{
					Name:   "main",
					Host:   "test.example.com",
					Target: v1.IngressTarget{Service: "web", Port: 80},
				},
			},
		},
	}

	rt.Update(project, map[string]string{"web": "172.18.0.2"})

	// Verify route exists.
	_, found := rt.Lookup("test.example.com")
	require.True(t, found)

	// Remove and verify it's gone.
	rt.Remove("test-app")
	_, found = rt.Lookup("test.example.com")
	assert.False(t, found)

	// Routes map should be empty.
	assert.Empty(t, rt.Routes())
}

func TestRouteTable_UpdateReplacesOldRoutes(t *testing.T) {
	rt := NewRouteTable(zap.NewNop())

	project := &v1.Project{
		ObjectMeta: v1.ObjectMeta{Name: "my-app"},
		Spec: v1.ProjectSpec{
			Services: []v1.ServiceDef{{Name: "web", Image: "nginx"}},
			Ingress: []v1.IngressDef{
				{
					Name:   "old-route",
					Host:   "old.example.com",
					Target: v1.IngressTarget{Service: "web", Port: 80},
				},
			},
		},
	}

	rt.Update(project, map[string]string{"web": "172.18.0.2"})

	// Update with different ingress rules.
	project.Spec.Ingress = []v1.IngressDef{
		{
			Name:   "new-route",
			Host:   "new.example.com",
			Target: v1.IngressTarget{Service: "web", Port: 8080},
		},
	}
	rt.Update(project, map[string]string{"web": "172.18.0.3"})

	// Old route should be gone.
	_, found := rt.Lookup("old.example.com")
	assert.False(t, found)

	// New route should exist.
	url, found := rt.Lookup("new.example.com")
	require.True(t, found)
	assert.Equal(t, "http://172.18.0.3:8080", url)
}

func TestRouteTable_SkipsMissingServiceIP(t *testing.T) {
	rt := NewRouteTable(zap.NewNop())

	project := &v1.Project{
		ObjectMeta: v1.ObjectMeta{Name: "partial"},
		Spec: v1.ProjectSpec{
			Services: []v1.ServiceDef{
				{Name: "web", Image: "nginx"},
				{Name: "api", Image: "myapi"},
			},
			Ingress: []v1.IngressDef{
				{
					Name:   "web-route",
					Host:   "web.example.com",
					Target: v1.IngressTarget{Service: "web", Port: 80},
				},
				{
					Name:   "api-route",
					Host:   "api.example.com",
					Target: v1.IngressTarget{Service: "api", Port: 3000},
				},
			},
		},
	}

	// Only "web" has an IP, "api" is missing.
	rt.Update(project, map[string]string{"web": "172.18.0.2"})

	// web route should exist.
	_, found := rt.Lookup("web.example.com")
	assert.True(t, found)

	// api route should be skipped.
	_, found = rt.Lookup("api.example.com")
	assert.False(t, found)
}

func TestRouteTable_Routes(t *testing.T) {
	rt := NewRouteTable(zap.NewNop())

	project := &v1.Project{
		ObjectMeta: v1.ObjectMeta{Name: "demo"},
		Spec: v1.ProjectSpec{
			Services: []v1.ServiceDef{{Name: "web", Image: "nginx"}},
			Ingress: []v1.IngressDef{
				{
					Name:   "main",
					Host:   "demo.example.com",
					Target: v1.IngressTarget{Service: "web", Port: 80},
				},
			},
		},
	}

	rt.Update(project, map[string]string{"web": "172.18.0.2"})

	routes := rt.Routes()
	assert.Len(t, routes, 1)
	assert.Equal(t, "http://172.18.0.2:80", routes["demo.example.com"])

	// Mutating the returned map should not affect the original.
	routes["hack"] = "http://evil.com"
	assert.Len(t, rt.Routes(), 1)
}

func TestRouteTable_ConcurrentAccess(t *testing.T) {
	rt := NewRouteTable(zap.NewNop())

	var wg sync.WaitGroup
	const goroutines = 50

	// Concurrent writers.
	for i := 0; i < goroutines; i++ {
		wg.Add(1)
		go func(n int) {
			defer wg.Done()
			p := &v1.Project{
				ObjectMeta: v1.ObjectMeta{Name: "project"},
				Spec: v1.ProjectSpec{
					Services: []v1.ServiceDef{{Name: "web", Image: "nginx"}},
					Ingress: []v1.IngressDef{
						{
							Name:   "main",
							Host:   "test.example.com",
							Target: v1.IngressTarget{Service: "web", Port: 80},
						},
					},
				},
			}
			rt.Update(p, map[string]string{"web": "172.18.0.2"})
		}(i)
	}

	// Concurrent readers.
	for i := 0; i < goroutines; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			rt.Lookup("test.example.com")
			rt.Routes()
		}()
	}

	// Concurrent removers.
	for i := 0; i < goroutines/2; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			rt.Remove("project")
		}()
	}

	wg.Wait()
	// No race condition — if this passes with -race it's correct.
}

func TestRouteTable_RemoveNonexistent(t *testing.T) {
	rt := NewRouteTable(zap.NewNop())

	// Should not panic.
	rt.Remove("does-not-exist")
	assert.Empty(t, rt.Routes())
}

func TestRouteTable_MultipleProjects(t *testing.T) {
	rt := NewRouteTable(zap.NewNop())

	projectA := &v1.Project{
		ObjectMeta: v1.ObjectMeta{Name: "project-a"},
		Spec: v1.ProjectSpec{
			Services: []v1.ServiceDef{{Name: "web", Image: "nginx"}},
			Ingress: []v1.IngressDef{
				{
					Name:   "a-route",
					Host:   "a.example.com",
					Target: v1.IngressTarget{Service: "web", Port: 80},
				},
			},
		},
	}

	projectB := &v1.Project{
		ObjectMeta: v1.ObjectMeta{Name: "project-b"},
		Spec: v1.ProjectSpec{
			Services: []v1.ServiceDef{{Name: "api", Image: "myapi"}},
			Ingress: []v1.IngressDef{
				{
					Name:   "b-route",
					Host:   "b.example.com",
					Target: v1.IngressTarget{Service: "api", Port: 3000},
				},
			},
		},
	}

	rt.Update(projectA, map[string]string{"web": "172.18.0.2"})
	rt.Update(projectB, map[string]string{"api": "172.18.0.3"})

	assert.Len(t, rt.Routes(), 2)

	// Remove project A — project B should be unaffected.
	rt.Remove("project-a")

	_, found := rt.Lookup("a.example.com")
	assert.False(t, found)

	url, found := rt.Lookup("b.example.com")
	require.True(t, found)
	assert.Equal(t, "http://172.18.0.3:3000", url)
}
