package controller

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	v1 "NYCU-SDC/caravanserai/api/v1"
	"NYCU-SDC/caravanserai/internal/event"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"
)

// fakeDNSStore is an in-memory DNSStore.
type fakeDNSStore struct {
	mu          sync.Mutex
	projects    []DNSProjectSnapshot
	nodeIPs     map[string]string
	projectsErr error
	nodesErr    error
	gotPhases   []v1.ProjectPhase
}

var _ DNSStore = (*fakeDNSStore)(nil)

func (f *fakeDNSStore) ListDNSProjects(_ context.Context, phases []v1.ProjectPhase) ([]DNSProjectSnapshot, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.gotPhases = phases
	if f.projectsErr != nil {
		return nil, f.projectsErr
	}
	return f.projects, nil
}

func (f *fakeDNSStore) ListNodeOverlayIPs(context.Context) (map[string]string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.nodesErr != nil {
		return nil, f.nodesErr
	}
	return f.nodeIPs, nil
}

// newTestDNSController returns a controller writing to a file in a fresh
// temporary directory, and that file's path.
func newTestDNSController(t *testing.T, s DNSStore, bus *event.Bus, opts ...Option) (*DNSRecordsController, string) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "extra-records.json")
	c, err := NewDNSRecordsController(zap.NewNop(), s, bus, path, fakeHostname, opts...)
	require.NoError(t, err)
	return c, path
}

func TestNewDNSRecordsController_Validation(t *testing.T) {
	s := &fakeDNSStore{}
	tests := []struct {
		name     string
		store    DNSStore
		path     string
		hostname HostnameFunc
		wantErr  string
	}{
		{name: "nil store", store: nil, path: "/tmp/x.json", hostname: fakeHostname, wantErr: "store is required"},
		{name: "empty path", store: s, path: "", hostname: fakeHostname, wantErr: "records file path is required"},
		{name: "nil hostname", store: s, path: "/tmp/x.json", hostname: nil, wantErr: "hostname function is required"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			c, err := NewDNSRecordsController(zap.NewNop(), tt.store, nil, tt.path, tt.hostname)

			require.ErrorContains(t, err, tt.wantErr)
			assert.Nil(t, c)
		})
	}
}

func TestNewDNSRecordsController_SeedInterval(t *testing.T) {
	s := &fakeDNSStore{}

	def, err := NewDNSRecordsController(zap.NewNop(), s, nil, "/tmp/x.json", fakeHostname)
	require.NoError(t, err)
	assert.Equal(t, dnsSeedInterval, def.seedInterval)

	custom, err := NewDNSRecordsController(zap.NewNop(), s, nil, "/tmp/x.json", fakeHostname, WithSeedInterval(time.Second))
	require.NoError(t, err)
	assert.Equal(t, time.Second, custom.seedInterval)
}

func TestDNSRecordsController_Reconcile(t *testing.T) {
	t.Run("writes the records file", func(t *testing.T) {
		s := &fakeDNSStore{
			projects: []DNSProjectSnapshot{dnsProject("blog", v1.ProjectPhaseRunning, 0, "www")},
			nodeIPs:  map[string]string{"node-a": "100.64.0.5"},
		}
		c, path := newTestDNSController(t, s, nil)

		res, err := c.Reconcile(context.Background(), dnsRecordsKey)

		require.NoError(t, err)
		assert.Equal(t, Result{}, res)
		assert.Equal(t, dnsRecordPhases, s.gotPhases)
		got, err := os.ReadFile(path)
		require.NoError(t, err)
		assert.JSONEq(t, `[{"name":"www.dev.cara.test","type":"A","value":"100.64.0.5"}]`, string(got))
	})

	t.Run("writes an empty array when nothing is published", func(t *testing.T) {
		c, path := newTestDNSController(t, &fakeDNSStore{}, nil)

		_, err := c.Reconcile(context.Background(), dnsRecordsKey)

		require.NoError(t, err)
		got, err := os.ReadFile(path)
		require.NoError(t, err)
		assert.Equal(t, "[]\n", string(got))
	})

	t.Run("unchanged records leave the file untouched", func(t *testing.T) {
		s := &fakeDNSStore{
			projects: []DNSProjectSnapshot{dnsProject("blog", v1.ProjectPhaseRunning, 0, "www")},
			nodeIPs:  map[string]string{"node-a": "100.64.0.5"},
		}
		c, path := newTestDNSController(t, s, nil)
		_, err := c.Reconcile(context.Background(), dnsRecordsKey)
		require.NoError(t, err)
		past := time.Date(2020, 1, 1, 0, 0, 0, 0, time.UTC)
		require.NoError(t, os.Chtimes(path, past, past))

		_, err = c.Reconcile(context.Background(), dnsRecordsKey)

		require.NoError(t, err)
		info, err := os.Stat(path)
		require.NoError(t, err)
		assert.True(t, info.ModTime().Equal(past), "file was rewritten")
	})

	t.Run("a move rewrites the record with the new node IP", func(t *testing.T) {
		s := &fakeDNSStore{
			projects: []DNSProjectSnapshot{dnsProject("blog", v1.ProjectPhaseRunning, 0, "www")},
			nodeIPs:  map[string]string{"node-a": "100.64.0.5", "node-b": "100.64.0.6"},
		}
		c, path := newTestDNSController(t, s, nil)
		_, err := c.Reconcile(context.Background(), dnsRecordsKey)
		require.NoError(t, err)

		s.mu.Lock()
		s.projects = []DNSProjectSnapshot{onNode(s.projects[0], "node-b")}
		s.mu.Unlock()
		_, err = c.Reconcile(context.Background(), dnsRecordsKey)

		require.NoError(t, err)
		got, err := os.ReadFile(path)
		require.NoError(t, err)
		assert.JSONEq(t, `[{"name":"www.dev.cara.test","type":"A","value":"100.64.0.6"}]`, string(got))
	})
}

func TestDNSRecordsController_ReconcileErrors(t *testing.T) {
	errStore := errors.New("store down")
	tests := []struct {
		name    string
		store   *fakeDNSStore
		path    func(t *testing.T) string
		wantErr string
	}{
		{
			name:    "listing projects fails",
			store:   &fakeDNSStore{projectsErr: errStore},
			wantErr: "list projects",
		},
		{
			name:    "listing nodes fails",
			store:   &fakeDNSStore{nodesErr: errStore},
			wantErr: "list node overlay IPs",
		},
		{
			name:  "the records directory is missing",
			store: &fakeDNSStore{},
			path: func(t *testing.T) string {
				return filepath.Join(t.TempDir(), "missing", "extra-records.json")
			},
			wantErr: "write records file",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "extra-records.json")
			if tt.path != nil {
				path = tt.path(t)
			}
			c, err := NewDNSRecordsController(zap.NewNop(), tt.store, nil, path, fakeHostname)
			require.NoError(t, err)

			_, err = c.Reconcile(context.Background(), dnsRecordsKey)

			require.ErrorContains(t, err, tt.wantErr)
		})
	}
}

// keyRecorder collects keys passed to enqueue from the Seed goroutine.
type keyRecorder struct {
	mu   sync.Mutex
	keys []string
}

func (r *keyRecorder) enqueue(key string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.keys = append(r.keys, key)
}

func (r *keyRecorder) count() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return len(r.keys)
}

func (r *keyRecorder) all() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]string(nil), r.keys...)
}

// runSeed starts Seed in a goroutine and returns a function that stops it and
// waits for it to return.
func runSeed(t *testing.T, c *DNSRecordsController, rec *keyRecorder) (stop func()) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		c.Seed(ctx, rec.enqueue)
	}()
	return func() {
		cancel()
		select {
		case <-done:
		case <-time.After(2 * time.Second):
			t.Fatal("Seed did not return after cancel")
		}
	}
}

func TestDNSRecordsController_Seed(t *testing.T) {
	t.Run("enqueues once at startup", func(t *testing.T) {
		// A long interval keeps the ticker out of this test.
		c, _ := newTestDNSController(t, &fakeDNSStore{}, nil, WithSeedInterval(time.Hour))
		rec := &keyRecorder{}

		stop := runSeed(t, c, rec)
		assert.Eventually(t, func() bool { return rec.count() == 1 }, time.Second, 5*time.Millisecond)
		stop()

		assert.Equal(t, []string{dnsRecordsKey}, rec.all())
	})

	t.Run("enqueues on project and node events", func(t *testing.T) {
		topics := []event.Topic{event.TopicProjectUpdated, event.TopicNodeCreated, event.TopicNodeUpdated}
		for _, topic := range topics {
			t.Run(string(topic), func(t *testing.T) {
				bus := event.New(zap.NewNop(), 16)
				c, _ := newTestDNSController(t, &fakeDNSStore{}, bus, WithSeedInterval(time.Hour))
				rec := &keyRecorder{}
				stop := runSeed(t, c, rec)
				defer stop()
				require.Eventually(t, func() bool { return rec.count() == 1 }, time.Second, 5*time.Millisecond)

				bus.Publish(topic, "anything")

				assert.Eventually(t, func() bool { return rec.count() == 2 }, time.Second, 5*time.Millisecond)
				assert.Equal(t, []string{dnsRecordsKey, dnsRecordsKey}, rec.all())
			})
		}
	})

	t.Run("enqueues on every tick", func(t *testing.T) {
		c, _ := newTestDNSController(t, &fakeDNSStore{}, nil, WithSeedInterval(10*time.Millisecond))
		rec := &keyRecorder{}

		stop := runSeed(t, c, rec)
		defer stop()

		assert.Eventually(t, func() bool { return rec.count() >= 3 }, time.Second, 5*time.Millisecond)
	})

	t.Run("returns when the context is cancelled", func(t *testing.T) {
		c, _ := newTestDNSController(t, &fakeDNSStore{}, event.New(zap.NewNop(), 16))

		stop := runSeed(t, c, &keyRecorder{})

		stop() // fails the test if Seed does not return
	})
}

func TestDNSRecordsController_OnEventClosedChannel(t *testing.T) {
	c, _ := newTestDNSController(t, &fakeDNSStore{}, nil)
	rec := &keyRecorder{}
	ch := make(event.Handler)

	got := c.onEvent(zap.NewNop(), false, ch, rec.enqueue)

	assert.Nil(t, got, "a closed channel must be dropped from select")
	assert.Zero(t, rec.count(), "a closed channel must not enqueue")
}
