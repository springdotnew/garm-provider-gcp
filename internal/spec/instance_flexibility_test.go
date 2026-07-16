// SPDX-License-Identifier: Apache-2.0
// Copyright 2024 Cloudbase Solutions SRL
//
//	Licensed under the Apache License, Version 2.0 (the "License"); you may
//	not use this file except in compliance with the License. You may obtain
//	a copy of the License at
//
//	     http://www.apache.org/licenses/LICENSE-2.0
//
//	Unless required by applicable law or agreed to in writing, software
//	distributed under the License is distributed on an "AS IS" BASIS, WITHOUT
//	WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied. See the
//	License for the specific language governing permissions and limitations
//	under the License.

package spec

import (
	"fmt"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestInstanceFlexibilityValidate(t *testing.T) {
	tooManyCandidates := make([]MachineTypeCandidate, maxInstanceFlexibilityCandidates+1)
	for index := range tooManyCandidates {
		tooManyCandidates[index].MachineType = fmt.Sprintf("n2-standard-%d", index+1)
	}

	tests := []struct {
		name        string
		flexibility *InstanceFlexibility
		errString   string
	}{
		{
			name: "ValidCandidates",
			flexibility: &InstanceFlexibility{
				Candidates: []MachineTypeCandidate{
					{MachineType: "n2d-standard-4"},
					{MachineType: "n2-standard-4"},
				},
			},
			errString: "",
		},
		{
			name:        "MissingCandidates",
			flexibility: &InstanceFlexibility{},
			errString:   "instance_flexibility.candidates must not be empty",
		},
		{
			name: "TooManyCandidates",
			flexibility: &InstanceFlexibility{
				Candidates: tooManyCandidates,
			},
			errString: "instance_flexibility.candidates cannot exceed 10 items",
		},
		{
			name: "MissingMachineType",
			flexibility: &InstanceFlexibility{
				Candidates: []MachineTypeCandidate{
					{MachineType: ""},
				},
			},
			errString: "instance flexibility candidate 0 is missing machine_type",
		},
		{
			name: "MachineTypeAsURL",
			flexibility: &InstanceFlexibility{
				Candidates: []MachineTypeCandidate{
					{MachineType: "zones/us-central1-a/machineTypes/n2-standard-4"},
				},
			},
			errString: "must not be a URL",
		},
		{
			name: "DuplicateMachineType",
			flexibility: &InstanceFlexibility{
				Candidates: []MachineTypeCandidate{
					{MachineType: "n2-standard-4"},
					{MachineType: "n2-standard-4"},
				},
			},
			errString: "duplicate instance flexibility machine_type 'n2-standard-4'",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := tt.flexibility.Validate()
			if tt.errString == "" {
				require.NoError(t, err)
			} else {
				require.ErrorContains(t, err, tt.errString)
			}
		})
	}
}
