package client

import (
	"testing"

	"github.com/cloudbase/garm-provider-common/params"
	"github.com/stretchr/testify/require"
)

func TestSelectStartupScript(t *testing.T) {
	tests := []struct {
		name                string
		osType              params.OSType
		useGCEStartupScript bool
		expected            string
	}{
		{name: "Linux cloud-init", osType: params.Linux, expected: linuxUserData},
		{name: "Linux GCE startup script", osType: params.Linux, useGCEStartupScript: true, expected: linuxStartupScript},
		{name: "Windows", osType: params.Windows, useGCEStartupScript: true, expected: windowsStartupScript},
		{name: "Unknown", osType: params.OSType("unknown"), useGCEStartupScript: true, expected: ""},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			require.Equal(t, test.expected, selectStartupScript(test.osType, test.useGCEStartupScript))
		})
	}
}
