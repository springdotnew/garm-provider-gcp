// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Cloudbase Solutions SRL

package spec

import (
	"encoding/base64"
	"strings"
	"testing"

	"github.com/cloudbase/garm-provider-common/params"
)

func TestComposeEarlyBootstrapUserData(t *testing.T) {
	originalInstallScriptFunc := DefaultRunnerInstallScriptFunc
	originalCloudConfigFunc := DefaultCloudConfigFunc
	t.Cleanup(func() {
		DefaultRunnerInstallScriptFunc = originalInstallScriptFunc
		DefaultCloudConfigFunc = originalCloudConfigFunc
	})

	installScript := []byte("#!/bin/bash\necho runner-install\n")
	DefaultRunnerInstallScriptFunc = func(params.BootstrapInstance, params.RunnerApplicationDownload, string) ([]byte, error) {
		return installScript, nil
	}
	DefaultCloudConfigFunc = func(params.BootstrapInstance, params.RunnerApplicationDownload, string) (string, error) {
		t.Fatal("standard cloud config must not be composed for early bootstrap")
		return "", nil
	}

	runnerSpec := RunnerSpec{
		EarlyBootstrap: true,
		DisableUpdates: true,
		BootstrapParams: params.BootstrapInstance{
			Name:       "early-runner",
			OSType:     params.Linux,
			ExtraSpecs: []byte(`{"early_bootstrap":true,"disable_updates":true}`),
		},
	}

	userData, err := runnerSpec.ComposeUserData()
	if err != nil {
		t.Fatalf("ComposeUserData() error = %v", err)
	}
	for _, expected := range []string{
		"#cloud-boothook",
		"systemctl start --no-block garm-early-bootstrap.service",
		"ExecStartPre=/usr/bin/id runner",
		"ExecStart=/usr/bin/su -l -c /usr/local/sbin/garm-early-bootstrap runner",
		"GARM_PROVIDER_EARLY_BOOTHOOK_SCHEDULED",
	} {
		if !strings.Contains(userData, expected) {
			t.Errorf("early user data does not contain %q", expected)
		}
	}
	encodedInstallScript := base64.StdEncoding.EncodeToString(installScript)
	if !strings.Contains(userData, encodedInstallScript) {
		t.Error("early user data does not contain the encoded install script")
	}
}

func TestComposeEarlyBootstrapUserDataRejectsUnsupportedCloudConfig(t *testing.T) {
	tests := []struct {
		name          string
		disableUpdate bool
		bootstrap     params.BootstrapInstance
		wantError     string
	}{
		{
			name:          "Windows",
			disableUpdate: true,
			bootstrap:     params.BootstrapInstance{OSType: params.Windows},
			wantError:     "early_bootstrap supports only Linux",
		},
		{
			name:      "boot updates",
			bootstrap: params.BootstrapInstance{OSType: params.Linux},
			wantError: "early_bootstrap requires disable_updates",
		},
		{
			name:          "extra packages",
			disableUpdate: true,
			bootstrap: params.BootstrapInstance{
				OSType: params.Linux,
				UserDataOptions: params.UserDataOptions{
					ExtraPackages: []string{"git"},
				},
			},
			wantError: "early_bootstrap does not support extra_packages",
		},
		{
			name:          "CA bundle",
			disableUpdate: true,
			bootstrap: params.BootstrapInstance{
				OSType:       params.Linux,
				CACertBundle: []byte("certificate"),
			},
			wantError: "early_bootstrap does not support ca-cert-bundle",
		},
		{
			name:          "pre-install scripts",
			disableUpdate: true,
			bootstrap: params.BootstrapInstance{
				OSType:     params.Linux,
				ExtraSpecs: []byte(`{"pre_install_scripts":{"prepare.sh":"ZWNobyBwcmVwYXJl"}}`),
			},
			wantError: "early_bootstrap does not support pre_install_scripts",
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			runnerSpec := RunnerSpec{
				EarlyBootstrap:  true,
				DisableUpdates:  test.disableUpdate,
				BootstrapParams: test.bootstrap,
			}
			_, err := runnerSpec.ComposeUserData()
			if err == nil || !strings.Contains(err.Error(), test.wantError) {
				t.Fatalf("ComposeUserData() error = %v, want %q", err, test.wantError)
			}
		})
	}
}
