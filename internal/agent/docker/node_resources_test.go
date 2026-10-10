package docker

import (
	"context"
	"errors"
	"testing"

	"github.com/docker/docker/api/types/system"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"
)

func TestNodeResources(t *testing.T) {
	tests := []struct {
		name       string
		info       system.Info
		infoErr    error
		wantCPU    int
		wantMemory int64
		wantErr    bool
	}{
		{
			name:       "reports NCPU and MemTotal",
			info:       system.Info{NCPU: 4, MemTotal: 8 << 30},
			wantCPU:    4,
			wantMemory: 8 << 30,
		},
		{
			name:    "daemon error",
			infoErr: errors.New("connection refused"),
			wantErr: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			fake := newFakeDocker()
			fake.info = tt.info
			fake.infoErr = tt.infoErr
			r := &DockerRuntime{client: fake, logger: zap.NewNop()}

			cpu, mem, err := r.NodeResources(context.Background())
			if tt.wantErr {
				require.ErrorIs(t, err, tt.infoErr)
				return
			}
			require.NoError(t, err)
			assert.Equal(t, tt.wantCPU, cpu)
			assert.Equal(t, tt.wantMemory, mem)
			assert.Empty(t, fake.mutations)
		})
	}
}
