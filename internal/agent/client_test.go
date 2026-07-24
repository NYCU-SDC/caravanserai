package agent

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"go.uber.org/zap"
)

func TestClient_OverlayIP(t *testing.T) {
	c := NewClient(zap.NewNop(), "http://localhost:8080", "node-1")

	// Empty until the agent joins the overlay.
	assert.Empty(t, c.OverlayIP())

	c.SetOverlayIP("100.64.0.5")
	assert.Equal(t, "100.64.0.5", c.OverlayIP())
}
