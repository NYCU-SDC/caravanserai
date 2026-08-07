package v1

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestValidateNamespace(t *testing.T) {
	tests := []struct {
		name      string
		namespace string
		wantErr   bool
	}{
		{name: "empty is valid", namespace: "", wantErr: false},
		{name: "default is valid", namespace: DefaultNamespace, wantErr: false},
		{name: "non-default is rejected", namespace: "blog-team", wantErr: true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := ValidateNamespace(tt.namespace)
			if tt.wantErr {
				assert.ErrorContains(t, err, "namespace support lands post-1.0")
			} else {
				assert.NoError(t, err)
			}
		})
	}
}
