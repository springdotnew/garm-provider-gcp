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

package client

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"

	compute "cloud.google.com/go/compute/apiv1"
	"cloud.google.com/go/compute/apiv1/computepb"
	"github.com/cloudbase/garm-provider-common/params"
	"github.com/cloudbase/garm-provider-gcp/config"
	"github.com/cloudbase/garm-provider-gcp/internal/spec"
	"github.com/cloudbase/garm-provider-gcp/internal/util"
	"github.com/googleapis/gax-go/v2"
	"github.com/googleapis/gax-go/v2/apierror"
	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"
	"google.golang.org/api/googleapi"
	"google.golang.org/protobuf/proto"
)

func TestBuildRegionalInsertRequest(t *testing.T) {
	runnerSpec := &spec.RunnerSpec{
		RegionalPlacement: &spec.RegionalPlacement{
			Zones: []string{"us-central1-a", "us-central1-b"},
		},
		BootstrapParams: params.BootstrapInstance{
			Name:   "garm-instance",
			Flavor: "n1-standard-1",
			Image:  "projects/garm-testing/global/images/garm-image",
		},
	}
	instance := &computepb.Instance{
		Name: proto.String("garm-instance"),
		Labels: map[string]string{
			"garmpoolid": "garm-pool",
		},
		Disks: []*computepb.AttachedDisk{
			{
				Boot: proto.Bool(true),
				InitializeParams: &computepb.AttachedDiskInitializeParams{
					SourceImage: proto.String("projects/garm-testing/global/images/garm-image"),
					// generateBootDisk always sets SourceSnapshot, even when empty.
					SourceSnapshot: proto.String(""),
				},
			},
		},
		NetworkInterfaces: []*computepb.NetworkInterface{
			{
				Network: proto.String("my-network"),
			},
		},
		Metadata: &computepb.Metadata{Items: []*computepb.Items{
			{Key: proto.String("runner_name"), Value: proto.String("garm-instance")},
		}},
		ServiceAccounts: []*computepb.ServiceAccount{
			{Email: proto.String("runner@example.invalid"), Scopes: []string{"scope-a"}},
		},
		ShieldedInstanceConfig: &computepb.ShieldedInstanceConfig{
			EnableVtpm: proto.Bool(true),
		},
		Tags: &computepb.Tags{Items: []string{"automation-runner"}},
	}
	markRegionalInstance(instance)

	req := buildRegionalInsertRequest("my-project", runnerSpec, instance)
	require.Equal(t, "my-project", req.Project)
	require.Equal(t, "us-central1", req.Region)
	require.NotEmpty(t, req.GetRequestId())
	resource := req.BulkInsertInstanceResourceResource
	require.EqualValues(t, 1, resource.GetCount())
	require.EqualValues(t, 1, resource.GetMinCount())
	require.Equal(t, "ANY_SINGLE_ZONE", resource.LocationPolicy.GetTargetShape())
	require.Len(t, resource.LocationPolicy.Zones, 2)
	require.Equal(t, "zones/us-central1-a", resource.LocationPolicy.Zones[0].GetZone())
	require.Equal(t, "n1-standard-1", resource.InstanceProperties.GetMachineType())
	require.Equal(t, "projects/garm-testing/global/images/garm-image", resource.InstanceProperties.Disks[0].InitializeParams.GetSourceImage())
	require.Nil(t, resource.InstanceProperties.Disks[0].InitializeParams.SourceSnapshot)
	require.Equal(t, "true", resource.InstanceProperties.Labels[util.RegionalPlacementLabel])
	require.True(t, proto.Equal(instance.Metadata, resource.InstanceProperties.Metadata))
	require.Equal(t, instance.NetworkInterfaces, resource.InstanceProperties.NetworkInterfaces)
	require.Equal(t, instance.ServiceAccounts, resource.InstanceProperties.ServiceAccounts)
	require.True(t, proto.Equal(instance.ShieldedInstanceConfig, resource.InstanceProperties.ShieldedInstanceConfig))
	require.True(t, proto.Equal(instance.Tags, resource.InstanceProperties.Tags))
	require.Contains(t, resource.PerInstanceProperties, "garm-instance")
}

func TestSplitRegionalProviderID(t *testing.T) {
	tests := []struct {
		name         string
		providerID   string
		expectedZone string
		expectedName string
		expectedOk   bool
	}{
		{
			name:         "ZonedProviderID",
			providerID:   "US-CENTRAL1-B/Garm-Instance",
			expectedZone: "us-central1-b",
			expectedName: "garm-instance",
			expectedOk:   true,
		},
		{
			name:       "PlainInstanceName",
			providerID: "garm-instance",
			expectedOk: false,
		},
		{
			name:       "TooManySeparators",
			providerID: "us-central1-b/garm-instance/extra",
			expectedOk: false,
		},
		{
			name:       "MissingName",
			providerID: "us-central1-b/",
			expectedOk: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			zone, name, ok := splitRegionalProviderID(tt.providerID)
			require.Equal(t, tt.expectedOk, ok)
			require.Equal(t, tt.expectedZone, zone)
			require.Equal(t, tt.expectedName, name)
		})
	}
}

func TestFindInstanceInZonesLooksUpZonesConcurrently(t *testing.T) {
	ctx := context.Background()
	mockClient := new(MockGcpClient)
	gcpCli := &GcpCli{
		cfg:    &config.Config{ProjectId: "my-project"},
		client: mockClient,
	}

	notFound, _ := apierror.FromError(&googleapi.Error{Code: 404, Message: "not found"})
	started := make(chan struct{}, 2)
	release := make(chan struct{})
	var releaseOnce sync.Once
	releaseLookups := func() { releaseOnce.Do(func() { close(release) }) }
	defer releaseLookups()
	mockClient.On("Get", ctx, mock.Anything, mock.Anything).
		Run(func(mock.Arguments) {
			started <- struct{}{}
			<-release
		}).
		Return((*computepb.Instance)(nil), notFound).
		Twice()

	type lookupResult struct {
		instance *computepb.Instance
		err      error
	}
	done := make(chan lookupResult, 1)
	go func() {
		instance, err := gcpCli.findInstanceInZones(
			ctx,
			"garm-instance",
			[]string{"us-central1-a", "us-central1-b"},
		)
		done <- lookupResult{instance: instance, err: err}
	}()

	for range 2 {
		select {
		case <-started:
		case <-time.After(time.Second):
			releaseLookups()
			t.Fatal("zone lookups did not overlap")
		}
	}
	releaseLookups()
	result := <-done
	require.NoError(t, result.err)
	require.Nil(t, result.instance)
	mockClient.AssertExpectations(t)
}

func TestBuildRegionalInsertRequestWithRankedMachineTypes(t *testing.T) {
	runnerSpec := &spec.RunnerSpec{
		RegionalPlacement: &spec.RegionalPlacement{
			Zones: []string{"us-central1-a", "us-central1-b"},
		},
		InstanceFlexibility: &spec.InstanceFlexibility{
			Candidates: []spec.MachineTypeCandidate{
				{MachineType: "n2d-standard-4"},
				{MachineType: "n2-standard-4"},
			},
		},
		BootstrapParams: params.BootstrapInstance{
			Name:   "garm-instance",
			Flavor: "n1-standard-1",
		},
	}
	instance := &computepb.Instance{
		Name: proto.String("garm-instance"),
		Labels: map[string]string{
			"garmpoolid": "garm-pool",
		},
		Disks: []*computepb.AttachedDisk{
			{
				Boot: proto.Bool(true),
			},
		},
	}
	markRegionalInstance(instance)

	req := buildRegionalInsertRequest("my-project", runnerSpec, instance)
	resource := req.BulkInsertInstanceResourceResource
	require.Nil(t, resource.InstanceProperties.MachineType)
	require.Len(t, resource.InstanceFlexibilityPolicy.InstanceSelections, 2)
	require.EqualValues(t, 0, resource.InstanceFlexibilityPolicy.InstanceSelections["selection-000"].GetRank())
	require.Equal(t, []string{"n2d-standard-4"}, resource.InstanceFlexibilityPolicy.InstanceSelections["selection-000"].MachineTypes)
	require.EqualValues(t, 1, resource.InstanceFlexibilityPolicy.InstanceSelections["selection-001"].GetRank())
	require.Equal(t, []string{"n2-standard-4"}, resource.InstanceFlexibilityPolicy.InstanceSelections["selection-001"].MachineTypes)
	require.Len(t, resource.InstanceProperties.Disks, 1)
}

func TestBuildRegionalInsertRequestWithSpotProvisioning(t *testing.T) {
	runnerSpec := &spec.RunnerSpec{
		RegionalPlacement: &spec.RegionalPlacement{
			Zones: []string{"us-central1-a", "us-central1-b"},
		},
		RegionalProvisioningModel: "SPOT",
		BootstrapParams: params.BootstrapInstance{
			Name:   "garm-instance",
			Flavor: "n1-standard-1",
		},
	}
	instance := &computepb.Instance{
		Name: proto.String("garm-instance"),
		Labels: map[string]string{
			"garmpoolid": "garm-pool",
		},
		Disks: []*computepb.AttachedDisk{
			{
				Boot: proto.Bool(true),
			},
		},
	}
	markRegionalInstance(instance)

	req := buildRegionalInsertRequest("my-project", runnerSpec, instance)
	scheduling := req.BulkInsertInstanceResourceResource.InstanceProperties.Scheduling
	require.Equal(t, "SPOT", scheduling.GetProvisioningModel())
	require.True(t, scheduling.GetPreemptible())
	require.False(t, scheduling.GetAutomaticRestart())
	require.Equal(t, "DELETE", scheduling.GetInstanceTerminationAction())
	require.Equal(t, "TERMINATE", scheduling.GetOnHostMaintenance())
}

func TestBulkInsertRegionalRetriesTransientInternalError(t *testing.T) {
	ctx := context.Background()
	mockRegionalClient := new(MockRegionalGcpClient)
	gcpCli := &GcpCli{regionClient: mockRegionalClient}
	req := &computepb.BulkInsertRegionInstanceRequest{RequestId: proto.String("stable-request-id")}
	internalErr, ok := apierror.FromError(&googleapi.Error{
		Code:    503,
		Message: "Internal error. Please try again.",
		Errors:  []googleapi.ErrorItem{{Reason: "INTERNAL_ERROR"}},
	})
	require.True(t, ok)
	createdOp := &compute.Operation{}

	mockRegionalClient.On("BulkInsert", ctx, mock.MatchedBy(func(actual *computepb.BulkInsertRegionInstanceRequest) bool {
		return actual.GetRequestId() == "stable-request-id"
	}), mock.Anything).Return((*compute.Operation)(nil), internalErr).Once()
	mockRegionalClient.On("BulkInsert", ctx, mock.MatchedBy(func(actual *computepb.BulkInsertRegionInstanceRequest) bool {
		return actual.GetRequestId() == "stable-request-id"
	}), mock.Anything).Return(createdOp, nil).Once()

	result, err := gcpCli.bulkInsertRegional(ctx, req)
	require.NoError(t, err)
	require.Same(t, createdOp, result)
	mockRegionalClient.AssertExpectations(t)
}

func TestIsTransientRegionalCreateError(t *testing.T) {
	wrappedInternal, ok := apierror.FromError(&googleapi.Error{Code: 503, Message: "Internal error"})
	require.True(t, ok)
	structuredInternal, ok := apierror.FromError(&googleapi.Error{
		Code:   503,
		Errors: []googleapi.ErrorItem{{Reason: "INTERNAL_ERROR"}},
	})
	require.True(t, ok)

	tests := []struct {
		name     string
		err      error
		expected bool
	}{
		{name: "Internal503", err: &googleapi.Error{Code: 503, Message: "Internal error"}, expected: true},
		{name: "WrappedInternal503", err: wrappedInternal, expected: true},
		{name: "StructuredInternal503", err: structuredInternal, expected: true},
		{name: "Backend500", err: &googleapi.Error{Code: 500, Message: "Backend error"}, expected: true},
		{name: "Capacity503", err: &googleapi.Error{Code: 503, Errors: []googleapi.ErrorItem{{Reason: "ZONE_RESOURCE_POOL_EXHAUSTED"}}}},
		{name: "Quota503", err: &googleapi.Error{Code: 503, Errors: []googleapi.ErrorItem{{Reason: "QUOTA_EXCEEDED"}}}},
		{name: "Permission403", err: &googleapi.Error{Code: 403, Message: "permission denied"}},
		{name: "TransportAmbiguity", err: errors.New("unexpected EOF")},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			require.Equal(t, tt.expected, isTransientRegionalCreateError(tt.err))
		})
	}
}

func TestCreateRegionalInstanceSpotFallback(t *testing.T) {
	ctx := context.Background()
	mockClient := new(MockGcpClient)
	mockRegionalClient := new(MockRegionalGcpClient)
	WaitOp = func(op *compute.Operation, ctx context.Context, opts ...gax.CallOption) error {
		return nil
	}
	gcpCli := &GcpCli{
		cfg: &config.Config{
			ProjectId:               "my-project",
			EnableRegionalPlacement: true,
		},
		client:       mockClient,
		regionClient: mockRegionalClient,
	}

	notFound, _ := apierror.FromError(&googleapi.Error{Code: 404, Message: "not found"})
	mockClient.On("Get", ctx, mock.Anything, mock.Anything).Return((*computepb.Instance)(nil), notFound).Twice()
	created := &computepb.Instance{
		Name: proto.String("garm-instance"),
		Zone: proto.String("zones/us-central1-a"),
		Labels: map[string]string{
			"garmpoolid":                "garm-pool",
			util.RegionalPlacementLabel: "true",
		},
	}
	mockClient.On("Get", ctx, mock.Anything, mock.Anything).Return(created, nil).Once()
	capacityErr := &googleapi.Error{
		Code:   503,
		Errors: []googleapi.ErrorItem{{Reason: "ZONE_RESOURCE_POOL_EXHAUSTED"}},
	}
	mockRegionalClient.On("BulkInsert", ctx, mock.MatchedBy(func(req *computepb.BulkInsertRegionInstanceRequest) bool {
		return req.BulkInsertInstanceResourceResource.InstanceProperties.Scheduling.GetProvisioningModel() == "SPOT"
	}), mock.Anything).Return((*compute.Operation)(nil), capacityErr).Once()
	mockRegionalClient.On("BulkInsert", ctx, mock.MatchedBy(func(req *computepb.BulkInsertRegionInstanceRequest) bool {
		return req.BulkInsertInstanceResourceResource.InstanceProperties.Scheduling == nil
	}), mock.Anything).Return(&compute.Operation{}, nil).Once()

	runnerSpec := &spec.RunnerSpec{
		RegionalPlacement: &spec.RegionalPlacement{
			Zones: []string{"us-central1-a"},
		},
		RegionalProvisioningModel:  "SPOT",
		RegionalFallbackToStandard: true,
		BootstrapParams: params.BootstrapInstance{
			Name:   "garm-instance",
			Flavor: "n1-standard-1",
		},
	}
	instance := &computepb.Instance{
		Name: proto.String("garm-instance"),
		Labels: map[string]string{
			"garmpoolid": "garm-pool",
		},
	}

	result, err := gcpCli.createRegionalInstance(ctx, runnerSpec, instance)
	require.NoError(t, err)
	require.Equal(t, created, result)
	mockClient.AssertExpectations(t)
	mockRegionalClient.AssertExpectations(t)
}

func TestCreateRegionalInstanceFallsBackToNextZoneOnCapacityError(t *testing.T) {
	ctx := context.Background()
	mockClient := new(MockGcpClient)
	mockRegionalClient := new(MockRegionalGcpClient)
	WaitOp = func(op *compute.Operation, ctx context.Context, opts ...gax.CallOption) error {
		return nil
	}
	gcpCli := &GcpCli{
		cfg: &config.Config{
			ProjectId:               "my-project",
			EnableRegionalPlacement: true,
		},
		client:       mockClient,
		regionClient: mockRegionalClient,
	}

	notFound, _ := apierror.FromError(&googleapi.Error{Code: 404, Message: "not found"})
	mockClient.On("Get", ctx, mock.Anything, mock.Anything).
		Return((*computepb.Instance)(nil), notFound).Times(5)
	created := &computepb.Instance{
		Name: proto.String("garm-instance"),
		Zone: proto.String("zones/us-central1-c"),
		Labels: map[string]string{
			"garmpoolid":                "garm-pool",
			util.RegionalPlacementLabel: "true",
		},
	}
	mockClient.On("Get", ctx, mock.Anything, mock.Anything).Return(created, nil).Once()
	capacityErr := &googleapi.Error{
		Code:   503,
		Errors: []googleapi.ErrorItem{{Reason: "ZONE_RESOURCE_POOL_EXHAUSTED"}},
	}
	mockRegionalClient.On("BulkInsert", ctx, mock.MatchedBy(func(req *computepb.BulkInsertRegionInstanceRequest) bool {
		zones := req.BulkInsertInstanceResourceResource.LocationPolicy.Zones
		return len(zones) == 1 && zones[0].GetZone() == "zones/us-central1-a"
	}), mock.Anything).Return((*compute.Operation)(nil), capacityErr).Once()
	mockRegionalClient.On("BulkInsert", ctx, mock.MatchedBy(func(req *computepb.BulkInsertRegionInstanceRequest) bool {
		zones := req.BulkInsertInstanceResourceResource.LocationPolicy.Zones
		return len(zones) == 1 && zones[0].GetZone() == "zones/us-central1-c"
	}), mock.Anything).Return(&compute.Operation{}, nil).Once()

	runnerSpec := &spec.RunnerSpec{
		RegionalPlacement: &spec.RegionalPlacement{
			Zones: []string{"us-central1-a", "us-central1-c"},
		},
		RegionalZoneFallback: true,
		BootstrapParams: params.BootstrapInstance{
			Name:   "garm-instance",
			Flavor: "n1-standard-1",
		},
	}
	instance := &computepb.Instance{
		Name:   proto.String("garm-instance"),
		Labels: map[string]string{"garmpoolid": "garm-pool"},
	}

	result, err := gcpCli.createRegionalInstance(ctx, runnerSpec, instance)
	require.NoError(t, err)
	require.Equal(t, created, result)
	mockClient.AssertExpectations(t)
	mockRegionalClient.AssertExpectations(t)
}

func TestCreateRegionalInstanceDoesNotTryNextZoneForNonCapacityError(t *testing.T) {
	ctx := context.Background()
	mockClient := new(MockGcpClient)
	mockRegionalClient := new(MockRegionalGcpClient)
	gcpCli := &GcpCli{
		cfg: &config.Config{
			ProjectId:               "my-project",
			EnableRegionalPlacement: true,
		},
		client:       mockClient,
		regionClient: mockRegionalClient,
	}

	notFound, _ := apierror.FromError(&googleapi.Error{Code: 404, Message: "not found"})
	mockClient.On("Get", ctx, mock.Anything, mock.Anything).
		Return((*computepb.Instance)(nil), notFound).Times(4)
	quotaErr := errors.New("QUOTA_EXCEEDED")
	mockRegionalClient.On("BulkInsert", ctx, mock.MatchedBy(func(req *computepb.BulkInsertRegionInstanceRequest) bool {
		zones := req.BulkInsertInstanceResourceResource.LocationPolicy.Zones
		return len(zones) == 1 && zones[0].GetZone() == "zones/us-central1-a"
	}), mock.Anything).Return((*compute.Operation)(nil), quotaErr).Once()

	runnerSpec := &spec.RunnerSpec{
		RegionalPlacement: &spec.RegionalPlacement{
			Zones: []string{"us-central1-a", "us-central1-c"},
		},
		RegionalZoneFallback: true,
		BootstrapParams: params.BootstrapInstance{
			Name:   "garm-instance",
			Flavor: "n1-standard-1",
		},
	}
	instance := &computepb.Instance{
		Name:   proto.String("garm-instance"),
		Labels: map[string]string{"garmpoolid": "garm-pool"},
	}

	_, err := gcpCli.createRegionalInstance(ctx, runnerSpec, instance)
	require.ErrorIs(t, err, quotaErr)
	mockRegionalClient.AssertNumberOfCalls(t, "BulkInsert", 1)
	mockClient.AssertExpectations(t)
	mockRegionalClient.AssertExpectations(t)
}

func TestCreateRegionalInstanceTriesAllSpotZonesBeforeStandardFallback(t *testing.T) {
	ctx := context.Background()
	mockClient := new(MockGcpClient)
	mockRegionalClient := new(MockRegionalGcpClient)
	WaitOp = func(op *compute.Operation, ctx context.Context, opts ...gax.CallOption) error {
		return nil
	}
	gcpCli := &GcpCli{
		cfg: &config.Config{
			ProjectId:               "my-project",
			EnableRegionalPlacement: true,
		},
		client:       mockClient,
		regionClient: mockRegionalClient,
	}

	notFound, _ := apierror.FromError(&googleapi.Error{Code: 404, Message: "not found"})
	mockClient.On("Get", ctx, mock.Anything, mock.Anything).
		Return((*computepb.Instance)(nil), notFound).Times(6)
	created := &computepb.Instance{
		Name: proto.String("garm-instance"),
		Zone: proto.String("zones/us-central1-a"),
		Labels: map[string]string{
			"garmpoolid":                "garm-pool",
			util.RegionalPlacementLabel: "true",
		},
	}
	mockClient.On("Get", ctx, mock.Anything, mock.Anything).Return(created, nil).Once()
	mockClient.On("Get", ctx, mock.Anything, mock.Anything).
		Return((*computepb.Instance)(nil), notFound).Once()
	capacityErr := &googleapi.Error{
		Code:   503,
		Errors: []googleapi.ErrorItem{{Reason: "ZONE_RESOURCE_POOL_EXHAUSTED"}},
	}
	for _, zone := range []string{"us-central1-a", "us-central1-c"} {
		zone := zone
		mockRegionalClient.On("BulkInsert", ctx, mock.MatchedBy(func(req *computepb.BulkInsertRegionInstanceRequest) bool {
			zones := req.BulkInsertInstanceResourceResource.LocationPolicy.Zones
			return len(zones) == 1 &&
				zones[0].GetZone() == "zones/"+zone &&
				req.BulkInsertInstanceResourceResource.InstanceProperties.Scheduling.GetProvisioningModel() == "SPOT"
		}), mock.Anything).Return((*compute.Operation)(nil), capacityErr).Once()
	}
	mockRegionalClient.On("BulkInsert", ctx, mock.MatchedBy(func(req *computepb.BulkInsertRegionInstanceRequest) bool {
		zones := req.BulkInsertInstanceResourceResource.LocationPolicy.Zones
		return len(zones) == 1 &&
			zones[0].GetZone() == "zones/us-central1-a" &&
			req.BulkInsertInstanceResourceResource.InstanceProperties.Scheduling == nil
	}), mock.Anything).Return(&compute.Operation{}, nil).Once()

	runnerSpec := &spec.RunnerSpec{
		RegionalPlacement: &spec.RegionalPlacement{
			Zones: []string{"us-central1-a", "us-central1-c"},
		},
		RegionalProvisioningModel:  "SPOT",
		RegionalFallbackToStandard: true,
		RegionalZoneFallback:       true,
		BootstrapParams: params.BootstrapInstance{
			Name:   "garm-instance",
			Flavor: "n1-standard-1",
		},
	}
	instance := &computepb.Instance{
		Name:   proto.String("garm-instance"),
		Labels: map[string]string{"garmpoolid": "garm-pool"},
	}

	result, err := gcpCli.createRegionalInstance(ctx, runnerSpec, instance)
	require.NoError(t, err)
	require.Equal(t, created, result)
	mockClient.AssertExpectations(t)
	mockRegionalClient.AssertExpectations(t)
}

func TestCreateRegionalInstanceReconcilesAmbiguousError(t *testing.T) {
	ctx := context.Background()
	mockClient := new(MockGcpClient)
	mockRegionalClient := new(MockRegionalGcpClient)
	gcpCli := &GcpCli{
		cfg: &config.Config{
			ProjectId:               "my-project",
			EnableRegionalPlacement: true,
		},
		client:       mockClient,
		regionClient: mockRegionalClient,
	}

	notFound, _ := apierror.FromError(&googleapi.Error{Code: 404, Message: "not found"})
	mockClient.On("Get", ctx, mock.Anything, mock.Anything).Return((*computepb.Instance)(nil), notFound).Once()
	created := &computepb.Instance{
		Name: proto.String("garm-instance"),
		Zone: proto.String("zones/us-central1-b"),
		Labels: map[string]string{
			"garmpoolid":                "garm-pool",
			util.RegionalPlacementLabel: "true",
		},
	}
	mockClient.On("Get", mock.Anything, mock.Anything, mock.Anything).Return(created, nil).Once()
	mockRegionalClient.On("BulkInsert", ctx, mock.Anything, mock.Anything).
		Return((*compute.Operation)(nil), errors.New("unexpected EOF")).Once()

	runnerSpec := &spec.RunnerSpec{
		RegionalPlacement:          &spec.RegionalPlacement{Zones: []string{"us-central1-b"}},
		RegionalProvisioningModel:  "SPOT",
		RegionalFallbackToStandard: true,
		BootstrapParams: params.BootstrapInstance{
			Name:   "garm-instance",
			Flavor: "n1-standard-1",
		},
	}
	instance := &computepb.Instance{
		Name:   proto.String("garm-instance"),
		Labels: map[string]string{"garmpoolid": "garm-pool"},
	}

	result, err := gcpCli.createRegionalInstance(ctx, runnerSpec, instance)
	require.NoError(t, err)
	require.Equal(t, created, result)
	mockRegionalClient.AssertNumberOfCalls(t, "BulkInsert", 1)
	mockClient.AssertExpectations(t)
	mockRegionalClient.AssertExpectations(t)
}

func TestCreateRegionalInstanceDoesNotFallbackForNonCapacityErrors(t *testing.T) {
	mixedCapacityAndQuota, ok := apierror.FromError(&googleapi.Error{
		Code: 503,
		Errors: []googleapi.ErrorItem{
			{Reason: "ZONE_RESOURCE_POOL_EXHAUSTED"},
			{Reason: "QUOTA_EXCEEDED"},
		},
	})
	require.True(t, ok)

	tests := []struct {
		name            string
		err             error
		expectedReasons []string
	}{
		{name: "Unauthenticated", err: errors.New("UNAUTHENTICATED")},
		{name: "QuotaExceeded", err: errors.New("QUOTA_EXCEEDED")},
		{name: "PermissionDenied", err: errors.New("PERMISSION_DENIED")},
		{name: "InvalidImage", err: errors.New("INVALID_IMAGE")},
		{name: "InvalidDisk", err: errors.New("INVALID_DISK")},
		{name: "InvalidNetwork", err: errors.New("INVALID_NETWORK")},
		{name: "InvalidMachineType", err: errors.New("INVALID_MACHINE_TYPE")},
		{name: "ResourceNotReady", err: errors.New("RESOURCE_NOT_READY")},
		{
			name: "MixedCapacityAndUnauthenticatedMessage",
			err:  errors.New("ZONE_RESOURCE_POOL_EXHAUSTED: UNAUTHENTICATED"),
		},
		{
			name: "MixedCapacityAndInvalidImageMessage",
			err:  errors.New("ZONE_RESOURCE_POOL_EXHAUSTED: INVALID_IMAGE"),
		},
		{
			name: "MixedCapacityAndInvalidDiskMessage",
			err:  errors.New("ZONE_RESOURCE_POOL_EXHAUSTED: INVALID_DISK"),
		},
		{
			name: "MixedCapacityAndInvalidNetworkMessage",
			err:  errors.New("ZONE_RESOURCE_POOL_EXHAUSTED: INVALID_NETWORK"),
		},
		{
			name: "MixedCapacityAndInvalidMachineTypeMessage",
			err:  errors.New("ZONE_RESOURCE_POOL_EXHAUSTED: INVALID_MACHINE_TYPE"),
		},
		{
			name: "MixedCapacityAndMalformedConfigMessage",
			err:  errors.New("ZONE_RESOURCE_POOL_EXHAUSTED: MALFORMED_CONFIG"),
		},
		{
			name:            "MixedCapacityAndQuotaReasons",
			err:             mixedCapacityAndQuota,
			expectedReasons: []string{"ZONE_RESOURCE_POOL_EXHAUSTED", "QUOTA_EXCEEDED"},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ctx := context.Background()
			mockClient := new(MockGcpClient)
			mockRegionalClient := new(MockRegionalGcpClient)
			gcpCli := &GcpCli{
				cfg: &config.Config{
					ProjectId:               "my-project",
					EnableRegionalPlacement: true,
				},
				client:       mockClient,
				regionClient: mockRegionalClient,
			}

			notFound, _ := apierror.FromError(&googleapi.Error{Code: 404, Message: "not found"})
			mockClient.On("Get", ctx, mock.Anything, mock.Anything).
				Return((*computepb.Instance)(nil), notFound).Twice()
			mockRegionalClient.On("BulkInsert", ctx, mock.Anything, mock.Anything).
				Return((*compute.Operation)(nil), tt.err).Once()

			runnerSpec := &spec.RunnerSpec{
				RegionalPlacement:          &spec.RegionalPlacement{Zones: []string{"us-central1-a"}},
				RegionalProvisioningModel:  "SPOT",
				RegionalFallbackToStandard: true,
				BootstrapParams: params.BootstrapInstance{
					Name:   "garm-instance",
					Flavor: "n1-standard-1",
				},
			}
			instance := &computepb.Instance{
				Name:   proto.String("garm-instance"),
				Labels: map[string]string{"garmpoolid": "garm-pool"},
			}

			_, err := gcpCli.createRegionalInstance(ctx, runnerSpec, instance)
			require.ErrorIs(t, err, tt.err)
			if len(tt.expectedReasons) > 0 {
				var googleErr *googleapi.Error
				require.ErrorAs(t, err, &googleErr)
				reasons := make([]string, 0, len(googleErr.Errors))
				for _, item := range googleErr.Errors {
					reasons = append(reasons, item.Reason)
				}
				require.Equal(t, tt.expectedReasons, reasons)
			}
			mockRegionalClient.AssertNumberOfCalls(t, "BulkInsert", 1)
			mockClient.AssertExpectations(t)
			mockRegionalClient.AssertExpectations(t)
		})
	}
}

func TestIsRegionalCapacityError(t *testing.T) {
	mixedCapacityAndQuota, ok := apierror.FromError(&googleapi.Error{
		Code: 503,
		Errors: []googleapi.ErrorItem{
			{Reason: "ZONE_RESOURCE_POOL_EXHAUSTED"},
			{Reason: "QUOTA_EXCEEDED"},
		},
	})
	require.True(t, ok)

	tests := []struct {
		name     string
		err      error
		expected bool
	}{
		{
			name:     "NilError",
			err:      nil,
			expected: false,
		},
		{
			name: "CapacityReason",
			err: &googleapi.Error{
				Code:   503,
				Errors: []googleapi.ErrorItem{{Reason: "ZONE_RESOURCE_POOL_EXHAUSTED"}},
			},
			expected: true,
		},
		{
			name:     "MixedCapacityAndQuotaReasons",
			err:      mixedCapacityAndQuota,
			expected: false,
		},
		{
			name: "MixedCapacityAndPermissionReasons",
			err: &googleapi.Error{
				Code: 503,
				Errors: []googleapi.ErrorItem{
					{Reason: "RESOURCE_POOL_EXHAUSTED"},
					{Reason: "PERMISSION_DENIED"},
				},
			},
			expected: false,
		},
		{
			name:     "CapacityMessage",
			err:      fmt.Errorf("failed to create regional instance: ZONE_RESOURCE_POOL_EXHAUSTED"),
			expected: true,
		},
		{
			name:     "MixedCapacityAndQuotaMessage",
			err:      fmt.Errorf("ZONE_RESOURCE_POOL_EXHAUSTED: QUOTA_EXCEEDED"),
			expected: false,
		},
		{
			name:     "MixedCapacityAndUnauthenticatedMessage",
			err:      fmt.Errorf("ZONE_RESOURCE_POOL_EXHAUSTED: UNAUTHENTICATED"),
			expected: false,
		},
		{
			name:     "MixedCapacityAndInvalidImageMessage",
			err:      fmt.Errorf("ZONE_RESOURCE_POOL_EXHAUSTED: INVALID_IMAGE"),
			expected: false,
		},
		{
			name:     "MixedCapacityAndInvalidDiskMessage",
			err:      fmt.Errorf("ZONE_RESOURCE_POOL_EXHAUSTED: INVALID_DISK"),
			expected: false,
		},
		{
			name:     "MixedCapacityAndInvalidNetworkMessage",
			err:      fmt.Errorf("ZONE_RESOURCE_POOL_EXHAUSTED: INVALID_NETWORK"),
			expected: false,
		},
		{
			name:     "MixedCapacityAndInvalidMachineTypeMessage",
			err:      fmt.Errorf("ZONE_RESOURCE_POOL_EXHAUSTED: INVALID_MACHINE_TYPE"),
			expected: false,
		},
		{
			name:     "MixedCapacityAndMalformedConfigMessage",
			err:      fmt.Errorf("ZONE_RESOURCE_POOL_EXHAUSTED: MALFORMED_CONFIG"),
			expected: false,
		},
		{
			name:     "PoolExhaustedMessage",
			err:      fmt.Errorf("resourcePoolExhausted"),
			expected: true,
		},
		{
			name: "ResourceNotReadyReason",
			err: &googleapi.Error{
				Code:   400,
				Errors: []googleapi.ErrorItem{{Reason: "RESOURCE_NOT_READY"}},
			},
			expected: false,
		},
		{
			name:     "ResourceNotReadyMessage",
			err:      fmt.Errorf("resourceNotReady"),
			expected: false,
		},
		{
			name:     "QuotaError",
			err:      fmt.Errorf("QUOTA_EXCEEDED"),
			expected: false,
		},
		{
			name:     "UnrelatedError",
			err:      fmt.Errorf("permission denied"),
			expected: false,
		},
		{
			name:     "AmbiguousError",
			err:      fmt.Errorf("unexpected EOF"),
			expected: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			require.Equal(t, tt.expected, isRegionalCapacityError(tt.err))
		})
	}
}
