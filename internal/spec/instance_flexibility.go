// SPDX-License-Identifier: Apache-2.0
// Copyright 2024 Cloudbase Solutions SRL
//
//    Licensed under the Apache License, Version 2.0 (the "License"); you may
//    not use this file except in compliance with the License. You may obtain
//    a copy of the License at
//
//         http://www.apache.org/licenses/LICENSE-2.0
//
//    Unless required by applicable law or agreed to in writing, software
//    distributed under the License is distributed on an "AS IS" BASIS, WITHOUT
//    WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied. See the
//    License for the specific language governing permissions and limitations
//    under the License.

package spec

import (
	"fmt"
	"strings"
)

const maxInstanceFlexibilityCandidates int = 10

type InstanceFlexibility struct {
	Candidates []MachineTypeCandidate `json:"candidates" jsonschema:"description=Machine types in descending preference order."`
}

type MachineTypeCandidate struct {
	MachineType string `json:"machine_type" jsonschema:"description=Compute Engine machine type name without a zone or URL."`
}

func (f *InstanceFlexibility) Validate() error {
	if f == nil {
		return nil
	}
	if len(f.Candidates) == 0 {
		return fmt.Errorf("instance_flexibility.candidates must not be empty")
	}
	if len(f.Candidates) > maxInstanceFlexibilityCandidates {
		return fmt.Errorf("instance_flexibility.candidates cannot exceed %d items", maxInstanceFlexibilityCandidates)
	}
	seen := make(map[string]struct{}, len(f.Candidates))
	for index, candidate := range f.Candidates {
		if candidate.MachineType == "" {
			return fmt.Errorf("instance flexibility candidate %d is missing machine_type", index)
		}
		if strings.Contains(candidate.MachineType, "/") {
			return fmt.Errorf("instance flexibility machine_type '%s' must not be a URL", candidate.MachineType)
		}
		if _, ok := seen[candidate.MachineType]; ok {
			return fmt.Errorf("duplicate instance flexibility machine_type '%s'", candidate.MachineType)
		}
		seen[candidate.MachineType] = struct{}{}
	}
	return nil
}
