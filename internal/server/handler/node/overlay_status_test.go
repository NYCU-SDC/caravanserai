package node

import (
	"testing"
	"time"

	v1 "NYCU-SDC/caravanserai/api/v1"

	"github.com/stretchr/testify/assert"
)

func TestComputeOverlayStatus(t *testing.T) {
	now := time.Date(2026, 8, 18, 12, 0, 0, 0, time.UTC)
	const threshold = 90 * time.Second
	const ip = "100.64.0.1"

	tests := []struct {
		name          string
		overlayIP     string
		lastHeartbeat time.Time
		want          v1.OverlayStatus
	}{
		{
			name:          "fresh heartbeat is Online",
			overlayIP:     ip,
			lastHeartbeat: now.Add(-10 * time.Second),
			want:          v1.OverlayStatusOnline,
		},
		{
			name:          "heartbeat exactly at threshold is Online",
			overlayIP:     ip,
			lastHeartbeat: now.Add(-threshold),
			want:          v1.OverlayStatusOnline,
		},
		{
			name:          "stale heartbeat is Offline",
			overlayIP:     ip,
			lastHeartbeat: now.Add(-threshold - time.Second),
			want:          v1.OverlayStatusOffline,
		},
		{
			name:          "never seen (zero heartbeat) is Unknown",
			overlayIP:     ip,
			lastHeartbeat: time.Time{},
			want:          v1.OverlayStatusUnknown,
		},
		{
			name:          "no overlay IP is Unknown even with a fresh heartbeat",
			overlayIP:     "",
			lastHeartbeat: now.Add(-1 * time.Second),
			want:          v1.OverlayStatusUnknown,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := computeOverlayStatus(tt.overlayIP, tt.lastHeartbeat, now, threshold)
			assert.Equal(t, tt.want, got)
		})
	}
}

func TestSetOverlayStatus(t *testing.T) {
	node := &v1.Node{}
	node.Status.Network.OverlayIP = "100.64.0.7"
	node.Status.LastHeartbeat = time.Now()

	setOverlayStatus(node, time.Now())

	assert.Equal(t, v1.OverlayStatusOnline, node.Status.Network.OverlayStatus)
}
