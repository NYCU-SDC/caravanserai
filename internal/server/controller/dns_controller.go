package controller

import (
	"context"
	"errors"
	"fmt"
	"time"

	v1 "NYCU-SDC/caravanserai/api/v1"
	"NYCU-SDC/caravanserai/internal/atomicfile"
	"NYCU-SDC/caravanserai/internal/event"

	"go.uber.org/zap"
)

const (
	// dnsRecordsKey is the only key DNSRecordsController reconciles. The
	// records file is one document built from every Project and Node, so
	// there is nothing to gain from per-object keys; the work queue
	// collapses a burst of events into one rebuild.
	dnsRecordsKey = "extra-records"

	// dnsSeedInterval is how often the Seed loop rebuilds the file even when
	// no event arrived. The store publishes no event when a Node is deleted
	// or when a Node's overlay IP changes without a state change, so this
	// interval bounds how long such a change takes to reach DNS.
	dnsSeedInterval = 30 * time.Second

	// dnsRecordsFileMode lets Headscale, which may run as another user,
	// read the file.
	dnsRecordsFileMode = 0o644
)

// DNSStore is the subset of the Store that DNSRecordsController needs.
type DNSStore interface {
	// ListDNSProjects returns every Project whose phase is one of phases.
	ListDNSProjects(ctx context.Context, phases []v1.ProjectPhase) ([]DNSProjectSnapshot, error)

	// ListNodeOverlayIPs maps each Node name to its overlay IP. A Node that
	// has not joined the overlay maps to an empty string or is absent.
	ListNodeOverlayIPs(ctx context.Context) (map[string]string, error)
}

// DNSRecordsController keeps the Headscale extra records file in step with
// where each Project runs.
//
// Every reconcile rebuilds the whole file from the store. Nothing is updated
// incrementally, so a missed event is repaired by the next rebuild. The file
// is only rewritten when its contents change, so Headscale reloads only when
// a record actually moved.
type DNSRecordsController struct {
	logger       *zap.Logger
	store        DNSStore
	bus          *event.Bus
	path         string
	hostname     HostnameFunc
	seedInterval time.Duration
}

// NewDNSRecordsController creates a DNSRecordsController that writes the
// records file to path. hostname computes each ingress's full hostname.
// bus may be nil; the controller then relies on its periodic rebuild alone.
func NewDNSRecordsController(
	logger *zap.Logger,
	store DNSStore,
	bus *event.Bus,
	path string,
	hostname HostnameFunc,
	opts ...Option,
) (*DNSRecordsController, error) {
	if store == nil {
		return nil, errors.New("dns controller: store is required")
	}
	if path == "" {
		return nil, errors.New("dns controller: records file path is required")
	}
	if hostname == nil {
		return nil, errors.New("dns controller: hostname function is required")
	}

	o := applyOptions(opts)
	interval := o.seedInterval
	if interval == 0 {
		interval = dnsSeedInterval
	}
	return &DNSRecordsController{
		logger:       logger,
		store:        store,
		bus:          bus,
		path:         path,
		hostname:     hostname,
		seedInterval: interval,
	}, nil
}

// Name implements Controller.
func (c *DNSRecordsController) Name() string { return "dns-records" }

// Reconcile implements Controller. It ignores name: every key rebuilds the
// one records file.
func (c *DNSRecordsController) Reconcile(ctx context.Context, _ string) (Result, error) {
	log := c.logger.With(zap.String("controller", c.Name()), zap.String("path", c.path))

	projects, err := c.store.ListDNSProjects(ctx, dnsRecordPhases)
	if err != nil {
		return Result{}, fmt.Errorf("dns controller: list projects: %w", err)
	}
	nodeIPs, err := c.store.ListNodeOverlayIPs(ctx)
	if err != nil {
		return Result{}, fmt.Errorf("dns controller: list node overlay IPs: %w", err)
	}

	records := buildDNSRecords(log, projects, nodeIPs, c.hostname)
	body, err := renderDNSRecords(records)
	if err != nil {
		return Result{}, err
	}

	written, err := atomicfile.WriteFile(c.path, body, dnsRecordsFileMode)
	if err != nil {
		return Result{}, fmt.Errorf("dns controller: write records file: %w", err)
	}
	if written {
		log.Info("DNS records file updated", zap.Int("records", len(records)))
	} else {
		log.Debug("DNS records unchanged", zap.Int("records", len(records)))
	}
	return Result{}, nil
}

// Seed implements Seeder. It enqueues a rebuild once at startup, on every
// Project or Node change event, and on every tick of the resync interval.
func (c *DNSRecordsController) Seed(ctx context.Context, enqueue func(name string)) {
	log := c.logger.With(zap.String("controller", c.Name()))

	var projectUpdated, nodeCreated, nodeUpdated event.Handler
	if c.bus != nil {
		projectUpdated = c.bus.Subscribe(event.TopicProjectUpdated)
		nodeCreated = c.bus.Subscribe(event.TopicNodeCreated)
		nodeUpdated = c.bus.Subscribe(event.TopicNodeUpdated)
	}

	tick := time.NewTicker(c.seedInterval)
	defer tick.Stop()

	// Build the file once before the first tick, so a restarted server
	// repairs anything that changed while it was down.
	enqueue(dnsRecordsKey)

	for {
		select {
		case <-ctx.Done():
			return
		case _, ok := <-projectUpdated:
			projectUpdated = c.onEvent(log, ok, projectUpdated, enqueue)
		case _, ok := <-nodeCreated:
			nodeCreated = c.onEvent(log, ok, nodeCreated, enqueue)
		case _, ok := <-nodeUpdated:
			nodeUpdated = c.onEvent(log, ok, nodeUpdated, enqueue)
		case <-tick.C:
			enqueue(dnsRecordsKey)
		}
	}
}

// onEvent enqueues a rebuild for a received event. When the channel was
// closed it returns nil, which blocks forever in select and so drops that
// source while the ticker keeps running.
func (c *DNSRecordsController) onEvent(log *zap.Logger, ok bool, ch event.Handler, enqueue func(string)) event.Handler {
	if !ok {
		log.Warn("DNS: event channel closed, relying on the resync ticker")
		return nil
	}
	enqueue(dnsRecordsKey)
	return ch
}
