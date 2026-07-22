// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Cloudbase Solutions SRL

package spec

import (
	"encoding/base64"
	"fmt"

	"github.com/cloudbase/garm-provider-common/cloudconfig"
	"github.com/cloudbase/garm-provider-common/params"
)

const earlyBootstrapRunnerUser = "runner"

func (r RunnerSpec) composeEarlyBootstrapUserData(bootstrapParams params.BootstrapInstance) (string, error) {
	if !r.DisableUpdates {
		return "", fmt.Errorf("early_bootstrap requires disable_updates")
	}
	if len(bootstrapParams.UserDataOptions.ExtraPackages) > 0 {
		return "", fmt.Errorf("early_bootstrap does not support extra_packages")
	}
	if len(bootstrapParams.CACertBundle) > 0 {
		return "", fmt.Errorf("early_bootstrap does not support ca-cert-bundle")
	}

	cloudSpecs, err := cloudconfig.GetSpecs(bootstrapParams)
	if err != nil {
		return "", fmt.Errorf("failed to load cloud config specs: %w", err)
	}
	if len(cloudSpecs.PreInstallScripts) > 0 {
		return "", fmt.Errorf("early_bootstrap does not support pre_install_scripts")
	}

	installScript, err := DefaultRunnerInstallScriptFunc(bootstrapParams, r.Tools, bootstrapParams.Name)
	if err != nil {
		return "", fmt.Errorf("failed to generate early bootstrap install script: %w", err)
	}

	encodedInstallScript := base64.StdEncoding.EncodeToString(installScript)
	return fmt.Sprintf(`#cloud-boothook
#!/usr/bin/env bash
set -euo pipefail

base64 --decode >/usr/local/sbin/garm-early-bootstrap <<'GARM_EARLY_BOOTSTRAP_BASE64'
%s
GARM_EARLY_BOOTSTRAP_BASE64
chmod 0755 /usr/local/sbin/garm-early-bootstrap

cat >/etc/systemd/system/garm-early-bootstrap.service <<'GARM_EARLY_BOOTSTRAP_UNIT'
[Unit]
Description=GARM early runner bootstrap
After=network-online.target
Wants=network-online.target

[Service]
Type=oneshot
ExecStartPre=/usr/bin/id %s
ExecStart=/usr/bin/su -l -c /usr/local/sbin/garm-early-bootstrap %s
StandardOutput=journal+console
StandardError=journal+console
TimeoutStartSec=15min
GARM_EARLY_BOOTSTRAP_UNIT

systemctl daemon-reload
systemctl start --no-block garm-early-bootstrap.service
echo "GARM_PROVIDER_EARLY_BOOTHOOK_SCHEDULED uptime=$(cut -d' ' -f1 /proc/uptime)"
`, encodedInstallScript, earlyBootstrapRunnerUser, earlyBootstrapRunnerUser), nil
}
