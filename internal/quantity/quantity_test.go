package quantity

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestParseCPU(t *testing.T) {
	tests := []struct {
		in      string
		want    int64
		wantErr bool
	}{
		{in: "500m", want: 500},
		{in: "0m", want: 0},
		{in: "2", want: 2000},
		{in: "0.5", want: 500},
		{in: "1.25", want: 1250},
		{in: " 250m ", want: 250},
		{in: "", wantErr: true},
		{in: "m", wantErr: true},
		{in: "-1", wantErr: true},
		{in: "-100m", wantErr: true},
		{in: "1.5m", wantErr: true},
		{in: "abc", wantErr: true},
		{in: "NaN", wantErr: true},
		{in: "Inf", wantErr: true},
	}

	for _, tt := range tests {
		t.Run(tt.in, func(t *testing.T) {
			got, err := ParseCPU(tt.in)
			if tt.wantErr {
				require.ErrorIs(t, err, ErrInvalid)
				return
			}
			require.NoError(t, err)
			assert.Equal(t, tt.want, got)
		})
	}
}

func TestParseMemory(t *testing.T) {
	tests := []struct {
		in      string
		want    int64
		wantErr bool
	}{
		{in: "1024", want: 1024},
		{in: "0", want: 0},
		{in: "64Ki", want: 64 << 10},
		{in: "512Mi", want: 512 << 20},
		{in: "4Gi", want: 4 << 30},
		{in: "1Ti", want: 1 << 40},
		{in: "500k", want: 500_000},
		{in: "500K", want: 500_000},
		{in: "500M", want: 500_000_000},
		{in: "1G", want: 1_000_000_000},
		{in: "2T", want: 2_000_000_000_000},
		{in: "", wantErr: true},
		{in: "Mi", wantErr: true},
		{in: "-1Gi", wantErr: true},
		{in: "1.5Gi", wantErr: true},
		{in: "4GB", wantErr: true},
		{in: "99999999999Ti", wantErr: true},
	}

	for _, tt := range tests {
		t.Run(tt.in, func(t *testing.T) {
			got, err := ParseMemory(tt.in)
			if tt.wantErr {
				require.ErrorIs(t, err, ErrInvalid)
				return
			}
			require.NoError(t, err)
			assert.Equal(t, tt.want, got)
		})
	}
}

func TestFormatCPU(t *testing.T) {
	assert.Equal(t, "0m", FormatCPU(0))
	assert.Equal(t, "500m", FormatCPU(500))
	assert.Equal(t, "2000m", FormatCPU(2000))
}

func TestFormatMemory(t *testing.T) {
	tests := []struct {
		in   int64
		want string
	}{
		{in: 0, want: "0"},
		{in: 1000, want: "1000"},
		{in: 1 << 10, want: "1Ki"},
		{in: 512 << 20, want: "512Mi"},
		{in: 4 << 30, want: "4Gi"},
		{in: 1536 << 20, want: "1536Mi"},
		{in: 2 << 40, want: "2Ti"},
		// A typical Linux MemTotal is KiB-granular but not MiB-aligned.
		{in: 8039428 << 10, want: "8039428Ki"},
	}

	for _, tt := range tests {
		t.Run(tt.want, func(t *testing.T) {
			assert.Equal(t, tt.want, FormatMemory(tt.in))
		})
	}
}

func TestFormatMemoryRoundTrips(t *testing.T) {
	for _, b := range []int64{0, 1000, 512 << 20, 8039428 << 10, 4 << 30} {
		got, err := ParseMemory(FormatMemory(b))
		require.NoError(t, err)
		assert.Equal(t, b, got)
	}
}
