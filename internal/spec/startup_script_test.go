package spec

import (
	"encoding/base64"
	"testing"

	"github.com/cloudbase/garm-provider-common/params"
	"github.com/stretchr/testify/require"
)

func TestComposeUserDataUsesGCEStartupScript(t *testing.T) {
	original := DefaultRunnerInstallScriptFunc
	t.Cleanup(func() { DefaultRunnerInstallScriptFunc = original })

	installScript := []byte("#!/bin/bash\necho ready\n")
	DefaultRunnerInstallScriptFunc = func(params.BootstrapInstance, params.RunnerApplicationDownload, string) ([]byte, error) {
		return installScript, nil
	}

	runnerSpec := RunnerSpec{
		UseGCEStartupScript: true,
		BootstrapParams: params.BootstrapInstance{
			Name:   "runner-1",
			OSType: params.Linux,
		},
	}

	userData, err := runnerSpec.ComposeUserData()
	require.NoError(t, err)
	require.Equal(t,
		"#!/bin/bash\nset -euo pipefail\nprintf '%s' '"+base64.StdEncoding.EncodeToString(installScript)+"' | base64 --decode | su -l runner -c 'bash -s'\n",
		userData,
	)
}

func TestComposeUserDataRejectsGCEStartupScriptForWindows(t *testing.T) {
	runnerSpec := RunnerSpec{
		UseGCEStartupScript: true,
		BootstrapParams: params.BootstrapInstance{
			OSType: params.Windows,
		},
	}

	_, err := runnerSpec.ComposeUserData()
	require.EqualError(t, err, "use_gce_startup_script requires a Linux runner")
}
