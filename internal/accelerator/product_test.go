package accelerator

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestProductFromNodeSelector(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		sel  map[string]string
		want string
	}{
		{name: "nil", sel: nil, want: ""},
		{name: "no product label", sel: map[string]string{"pool": "train"}, want: ""},
		{name: "NVIDIA product label", sel: map[string]string{"nvidia.com/gpu.product": "NVIDIA-H100-80GB-HBM3"}, want: "NVIDIA-H100-80GB-HBM3"},
		{name: "an alias", sel: map[string]string{"cloud.google.com/gke-accelerator": "nvidia-h100-80gb"}, want: "nvidia-h100-80gb"},
		{
			name: "the product label before its aliases",
			sel:  map[string]string{"cloud.google.com/gke-accelerator": "nvidia-h100-80gb", "nvidia.com/gpu.product": "NVIDIA-H100-80GB-HBM3"},
			want: "NVIDIA-H100-80GB-HBM3",
		},
		{
			name: "vendor order: NVIDIA before AMD",
			sel:  map[string]string{"amd.com/gpu.product-name": "AMD-MI300X", "nvidia.com/gpu.product": "NVIDIA-A100"},
			want: "NVIDIA-A100",
		},
		{name: "an empty value is skipped", sel: map[string]string{"nvidia.com/gpu.product": "", "amd.com/gpu.product-name": "AMD-MI300X"}, want: "AMD-MI300X"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			assert.Equal(t, tt.want, ProductFromNodeSelector(tt.sel))
		})
	}
}
